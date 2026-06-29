// Command server runs the go-ledger HTTP API.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"

	"github.com/sohag-pro/go-ledger/internal/api"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/metrics"
	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/seed"
	"github.com/sohag-pro/go-ledger/internal/web"
)

// defaultTenantID is the tenant every request acts as until an auth layer
// resolves a real one. Override with DEFAULT_TENANT_ID.
const defaultTenantID = "00000000-0000-0000-0000-000000000001"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

type config struct {
	port          string
	metricsAddr   string
	databaseURL   string
	defaultTenant string
	seedEnabled   bool
	seedInterval  time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		port:          getenv("PORT", "8080"),
		metricsAddr:   getenv("METRICS_ADDR", "127.0.0.1:9090"),
		databaseURL:   os.Getenv("DATABASE_URL"),
		defaultTenant: getenv("DEFAULT_TENANT_ID", defaultTenantID),
		seedEnabled:   getenvBool("SEED_ENABLED", true),
		seedInterval:  getenvDuration("SEED_INTERVAL", 4*time.Hour),
	}
	if cfg.databaseURL == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return fallback
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return fallback
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()

	// Apply migrations before serving. On a single instance this is the simplest
	// correct option: the binary that needs a column also creates it.
	if err := runMigrations(cfg.databaseURL, logger); err != nil {
		return err
	}

	pool, err := postgres.NewPool(ctx, cfg.databaseURL, 0)
	if err != nil {
		return err
	}
	defer pool.Close()

	repo := postgres.NewRepository(pool)
	deps := api.Deps{
		Accounts:      ledger.NewAccountService(repo),
		Transactions:  ledger.NewTransactionService(repo, logger),
		DefaultTenant: cfg.defaultTenant,
	}

	// Demo seeder: reset and repopulate the demo ledger on startup and on an
	// interval, so the public demo always has fresh, realistic data. Stops on
	// shutdown.
	if cfg.seedEnabled {
		seedCtx, cancelSeed := context.WithCancel(context.Background())
		defer cancelSeed()
		go runSeeder(seedCtx, logger, pool, cfg.defaultTenant, cfg.seedInterval)
	}

	router := chi.NewRouter()
	// No RealIP middleware: it trusts client-set forwarding headers and is
	// spoofable. Revisit with a trusted-proxy allowlist when one is in front.
	router.Use(middleware.RequestID, middleware.Recoverer, slogLogger(logger))
	router.Get("/", web.Index)
	router.Get("/console", web.Console)
	router.Handle("/static/*", http.StripPrefix("/static/", web.Assets()))
	api.RegisterPlayground(router)
	api.New(router, deps) // mounts /v1/*, /healthz, /openapi.*, /schemas/

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Metrics on a separate loopback server so the Prometheus endpoint is never
	// exposed on the public interface (see ADR-004).
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", metrics.Handler())
	metricsSrv := &http.Server{
		Addr:              cfg.metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting server", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		logger.Info("starting metrics server", "addr", metricsSrv.Addr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-signalCtx.Done():
	}

	logger.Info("shutting down servers")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown", "error", err)
	}
	return srv.Shutdown(shutdownCtx)
}

// runMigrations applies the embedded goose migrations. goose uses database/sql,
// so it opens a short-lived handle over the pgx stdlib driver, separate from the
// app's pgx pool.
func runMigrations(dsn string, logger *slog.Logger) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db for migrations: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	logger.Info("migrations applied")
	return nil
}

// runSeeder seeds the demo ledger once immediately, then every interval, until
// ctx is cancelled. A failed seed is logged and the loop continues.
func runSeeder(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, tenant string, interval time.Duration) {
	doSeed := func() {
		start := time.Now()
		if err := seed.Seed(ctx, pool, tenant, time.Now()); err != nil {
			logger.Error("demo seed failed", "error", err)
			return
		}
		logger.Info("demo data seeded", "elapsed", time.Since(start))
	}

	doSeed()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			doSeed()
		}
	}
}

// slogLogger logs one structured line per request: method, path, status, size,
// duration, and the chi request id.
func slogLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("elapsed", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}

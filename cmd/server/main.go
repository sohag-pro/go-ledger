// Command server runs the go-ledger HTTP API.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/sohag-pro/go-ledger/internal/api"
	"github.com/sohag-pro/go-ledger/internal/auth"
	grpcserver "github.com/sohag-pro/go-ledger/internal/grpcserver"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/metrics"
	"github.com/sohag-pro/go-ledger/internal/observability"
	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/seed"
	"github.com/sohag-pro/go-ledger/internal/web"
)

const ledgerTracerName = "github.com/sohag-pro/go-ledger/internal/ledger"

// defaultTenantID is the tenant every request acts as until an auth layer
// resolves a real one. Override with DEFAULT_TENANT_ID.
const defaultTenantID = "00000000-0000-0000-0000-000000000001"

func main() {
	logger := slog.New(observability.NewTraceHandler(slog.NewJSONHandler(os.Stdout, nil)))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

type config struct {
	port          string
	metricsAddr   string
	grpcAddr      string
	databaseURL   string
	defaultTenant string
	env           string
	serviceName   string
	seedEnabled   bool
	seedInterval  time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		port:          getenv("PORT", "8080"),
		metricsAddr:   getenv("METRICS_ADDR", "127.0.0.1:9090"),
		grpcAddr:      getenv("GRPC_ADDR", ":9091"),
		databaseURL:   os.Getenv("DATABASE_URL"),
		defaultTenant: getenv("DEFAULT_TENANT_ID", defaultTenantID),
		env:           getenv("APP_ENV", "development"),
		serviceName:   getenv("OTEL_SERVICE_NAME", "go-ledger"),
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

	// Tracing and metrics providers first, so every later component records into
	// them. Setup chooses its exporter from the environment (see ADR-010) and is a
	// no-op when no OTLP endpoint is configured.
	obs, err := observability.Setup(ctx, observability.Config{
		ServiceName: cfg.serviceName,
		Environment: cfg.env,
		Logger:      logger,
	})
	if err != nil {
		return fmt.Errorf("observability setup: %w", err)
	}

	meterProvider, err := metrics.MeterProvider()
	if err != nil {
		return fmt.Errorf("metrics meter provider: %w", err)
	}
	otel.SetMeterProvider(meterProvider)

	// Flush telemetry last, on every exit path. Deferring it means the batch
	// exporter drains after the HTTP and gRPC servers have already stopped
	// accepting work (their shutdown runs earlier in run's body), so in-flight
	// requests still get their spans, and an unreachable OTLP endpoint cannot eat
	// the servers' drain budget: this flush has its own bounded one.
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(flushCtx); err != nil {
			logger.Error("observability shutdown", "error", err)
		}
		if err := meterProvider.Shutdown(flushCtx); err != nil {
			logger.Error("meter provider shutdown", "error", err)
		}
	}()

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
	// The tenant for both REST and gRPC now comes only from the caller's API
	// key (ADR-012): there is no DefaultTenant fallback for either surface.
	// Every /v1 request resolves through auth.HumaMiddleware and every gRPC
	// call resolves through the gRPC auth interceptor, both sharing this one
	// resolver instance (and its in-memory cache). Key rows are provisioned
	// directly in api_keys (see the ADR); a zero ttl falls back to the
	// resolver's default cache TTL.
	resolver := auth.NewResolver(repo, 0)
	deps := api.Deps{
		Accounts:     ledger.NewAccountService(repo),
		Transactions: ledger.NewTransactionService(repo, logger, otel.Tracer(ledgerTracerName)),
		Audit:        ledger.NewAuditService(repo),
		Auth:         resolver,
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
	router.Use(middleware.RequestID, middleware.Recoverer, otelRouteName, slogLogger(logger))
	router.Get("/", web.Index)
	router.Get("/console", web.Console)
	router.Handle("/static/*", http.StripPrefix("/static/", web.Assets()))
	api.RegisterPlayground(router)
	api.New(router, deps) // mounts /v1/*, /healthz, /openapi.*, /schemas/

	// Wrap the router in one OTel server span per request. Health checks are
	// filtered out so traces are real request work, not liveness noise; the
	// metrics server (below) is never wrapped (ADR-004, ADR-010).
	tracedHandler := otelhttp.NewHandler(router, "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }),
	)
	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           tracedHandler,
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

	errCh := make(chan error, 3)
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

	grpcSrv := grpcserver.NewGRPCServer(grpcserver.Deps{
		Accounts:     deps.Accounts,
		Transactions: deps.Transactions,
		Audit:        deps.Audit,
		Auth:         resolver,
	}, logger)
	grpcListener, err := net.Listen("tcp", cfg.grpcAddr)
	if err != nil {
		return fmt.Errorf("grpc listen on %s: %w", cfg.grpcAddr, err)
	}
	go func() {
		logger.Info("starting grpc server", "addr", cfg.grpcAddr)
		if err := grpcSrv.Serve(grpcListener); err != nil {
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
	// Wait for in-flight RPCs to finish, but do not let a stuck one outlast the
	// shutdown deadline: force-stop if the graceful stop is still running when
	// shutdownCtx expires.
	stopped := make(chan struct{})
	go func() { grpcSrv.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		grpcSrv.Stop()
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

// otelRouteName upgrades the otelhttp server span name from the raw path to the
// matched chi route pattern once routing has resolved, so high-cardinality URLs
// (account and transaction ids) cannot explode the trace backend.
func otelRouteName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if rc := chi.RouteContext(r.Context()); rc != nil {
			if pattern := rc.RoutePattern(); pattern != "" {
				span := oteltrace.SpanFromContext(r.Context())
				span.SetName(r.Method + " " + pattern)
				span.SetAttributes(semconv.HTTPRoute(pattern))
			}
		}
	})
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

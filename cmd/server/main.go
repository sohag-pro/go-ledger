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

	goledger "github.com/sohag-pro/go-ledger"
	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/api"
	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/crypto"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	grpcserver "github.com/sohag-pro/go-ledger/internal/grpcserver"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/metrics"
	"github.com/sohag-pro/go-ledger/internal/observability"
	"github.com/sohag-pro/go-ledger/internal/opsmetrics"
	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/seed"
	"github.com/sohag-pro/go-ledger/internal/web"
	"github.com/sohag-pro/go-ledger/internal/webhook"
)

const ledgerTracerName = "github.com/sohag-pro/go-ledger/internal/ledger"

// defaultTenantID is the tenant every request acts as until an auth layer
// resolves a real one. Override with DEFAULT_TENANT_ID.
const defaultTenantID = "00000000-0000-0000-0000-000000000001"

// buildRevision is the git short SHA the running binary was built from,
// stamped in by `make build` and the Dockerfile via
// `-ldflags "-X main.buildRevision=..."` (Task 5.6a, audit A6.1). It stays
// "dev" for `go run`/`go test`, where no ldflags are passed. Surfaced on GET
// /healthz (additively; the deploy health check's "ok" status contract is
// unchanged) and as the build_info{revision=...} 1 Prometheus gauge, so an
// operator can always tell which build is actually serving.
var buildRevision = "dev"

// migrateTimeout bounds how long the `migrate` subcommand's database
// connection and goose run may take (Task 5.6b, audit A4.3), so a hung
// migration fails the deploy pipeline's pre-swap step instead of hanging the
// CI job indefinitely.
const migrateTimeout = 2 * time.Minute

// errDatabaseURLRequired is returned by runMigrateCommand when DATABASE_URL
// is unset, distinct from run()'s own loadConfig check so `migrate` fails
// with the same clear message whether or not the server's other env vars
// are present.
var errDatabaseURLRequired = errors.New("DATABASE_URL is required")

func main() {
	logger := slog.New(observability.NewTraceHandler(slog.NewJSONHandler(os.Stdout, nil)))
	slog.SetDefault(logger)

	// `migrate` is a distinct entry point, not a server flag: the deploy
	// pipeline invokes `./go-ledger.new migrate` over SSH against the box's
	// DATABASE_URL BEFORE swapping the new binary into place (Task 5.6b,
	// audit A4.3), so a schema change lands ahead of the code that needs it
	// and a failed migration aborts the deploy without ever swapping (see
	// .github/workflows/deploy.yml and runMigrateCommand's own doc comment).
	// Every other invocation, including no args at all, falls through to the
	// normal server path unchanged.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrateCommand(os.Args[2:], logger); err != nil {
			logger.Error("migrate failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := run(logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// defaultDemoAPIKey is a known, public value (see ADR-012, "A public demo key
// keeps the console open"): shipping it in the console and playground is fine
// on purpose, since the key it names is tenant-scoped to the demo tenant,
// carries a tight rate limit, and that tenant is wiped every hour.
const defaultDemoAPIKey = "glk_demo_public_key_reset_every_4h" //nolint:gosec // public by design: a tenant-scoped, rate-limited demo key that the console and playground ship, not a real secret (ADR-012)

// demoAPIKeyRateLimitRPM and loadTestAPIKeyRateLimitRPM are the per-key
// overrides provisioned for the demo and load-test keys respectively (see
// ADR-012, "Per-key rate limiting"): the demo key is deliberately tighter
// than the server default so the public demo cannot be used to flood the
// service, and the load-test key is deliberately far looser so a k6 run
// exercises the ledger, not the limiter.
const (
	demoAPIKeyRateLimitRPM     = 60
	loadTestAPIKeyRateLimitRPM = 100000
)

// loadTestTenantBase is the first tenant id offset used when provisioning
// multi-tenant load-test keys (see provisionAPIKeys). Starting at 200 keeps
// generated tenants clear of the demo tenant (...001) and the single load
// tenant docker-compose's DEFAULT_TENANT_ID typically points at (...002).
const loadTestTenantBase = 200

type config struct {
	port                     string
	metricsAddr              string
	grpcAddr                 string
	databaseURL              string
	defaultTenant            string
	env                      string
	serviceName              string
	demoMode                 bool
	seedEnabled              bool
	seedInterval             time.Duration
	demoAPIKey               string
	loadTestKey              string
	loadTestTenants          int
	rateLimitRPM             int
	authCacheTTL             time.Duration
	authNegativeMaxFailures  int
	authNegativeWindow       time.Duration
	defaultCurrency          string
	fxRates                  string
	masterKey                string
	chainerEnabled           bool
	chainerInterval          time.Duration
	chainerBatch             int
	anchorEnabled            bool
	anchorInterval           time.Duration
	idempotencyTTL           time.Duration
	idempotencySweepInterval time.Duration
	webhooksEnabled          bool
	webhookMaxAttempts       int
	webhookDeliveryInterval  time.Duration
	metricsCollectInterval   time.Duration
	adminBootstrap           bool
}

// loadConfig loads the server's configuration from the environment, running
// interactive first-run Postgres setup (ADR-019) when DATABASE_URL is unset
// and a real terminal is attached to stdin. isInteractive() is the single
// TTY check for this whole feature; see loadConfigWithTTY for the injectable
// core this delegates to.
func loadConfig() (config, error) {
	return loadConfigWithTTY(isInteractive())
}

// loadConfigWithTTY is loadConfig's testable core: interactive is normally
// isInteractive()'s result, but a test can force either branch without a
// real terminal. When databaseURL is empty and interactive is false (the
// non-interactive path: docker, systemd, CI, or a plain pipe), this is
// byte-for-byte the fail-fast behavior that existed before ADR-019: a clear
// "DATABASE_URL is required" error, never a hang waiting on input.
func loadConfigWithTTY(interactive bool) (config, error) {
	cfg := config{
		port:        getenv("PORT", "8080"),
		metricsAddr: getenv("METRICS_ADDR", "127.0.0.1:9090"),
		// GRPC_ADDR defaults to loopback-only (Task 5.1, audit A2.2, ADR-015
		// Phase 5), matching Postgres's own "listen_addresses = 'localhost'"
		// posture: the gRPC surface moves the same money REST does but ships
		// with no TLS of its own, so binding every interface (the prior
		// ":9091" default) would serve it in the clear to anyone who could
		// reach the box. A deployment that needs gRPC reachable off-box must
		// terminate TLS in front of it first (see the TLS/loopback decision
		// recorded on grpcserver.NewGRPCServer) and set GRPC_ADDR explicitly.
		grpcAddr:        getenv("GRPC_ADDR", "127.0.0.1:9091"),
		databaseURL:     os.Getenv("DATABASE_URL"),
		defaultTenant:   getenv("DEFAULT_TENANT_ID", defaultTenantID),
		env:             getenv("APP_ENV", "development"),
		serviceName:     getenv("OTEL_SERVICE_NAME", "go-ledger"),
		demoMode:        getenvBool("DEMO_MODE", false),
		seedEnabled:     getenvBool("SEED_ENABLED", false),
		seedInterval:    getenvDuration("SEED_INTERVAL", time.Hour),
		demoAPIKey:      getenv("DEMO_API_KEY", defaultDemoAPIKey),
		loadTestKey:     getenv("LOAD_TEST_API_KEY", ""),
		loadTestTenants: getenvInt("LOAD_TEST_TENANTS", 8),
		rateLimitRPM:    getenvInt("RATE_LIMIT_RPM", 120),
		authCacheTTL:    getenvDuration("AUTH_CACHE_TTL", 30*time.Second),
		// AUTH_NEGATIVE_MAX_FAILURES / AUTH_NEGATIVE_WINDOW configure the
		// negative-lookup throttle (Task 5.2, audit A2.5/A6.4): auth.Resolver
		// deliberately never caches a miss (see its own doc comment), so
		// every unknown or expired key is a database round trip; this
		// throttle caps how many of those a single client IP can trigger
		// before being rejected without a lookup at all, protecting the
		// connection pool from a garbage-API-key flood. Defaults mirror
		// auth.DefaultNegativeThrottleMaxFailures / Window.
		authNegativeMaxFailures: getenvInt("AUTH_NEGATIVE_MAX_FAILURES", auth.DefaultNegativeThrottleMaxFailures),
		authNegativeWindow:      getenvDuration("AUTH_NEGATIVE_WINDOW", auth.DefaultNegativeThrottleWindow),
		defaultCurrency:         getenv("DEFAULT_CURRENCY", "USD"),
		fxRates:                 os.Getenv("FX_RATES"),
		// LEDGER_MASTER_KEY (Task 6.2, audit A9.3): a 32-byte, base64-encoded
		// master key for envelope-encrypting posting descriptions at rest
		// (internal/crypto.Cipher). Left empty by default (getenv's fallback
		// is deliberately "", not a generated or placeholder value): PII
		// encryption is a feature that turns ITSELF on only when this is
		// set, so existing dev/CI environments that never set it keep
		// storing descriptions in plaintext exactly as before Task 6.2, with
		// no other config flag required. A value that IS set but malformed
		// fails fast below, before the server ever starts accepting
		// requests, rather than surfacing lazily on the first real post.
		masterKey: os.Getenv("LEDGER_MASTER_KEY"),
		// The audit chainer (ADR-017): on by default, since every deployment
		// (single instance or a fleet) needs it to turn posted audit_outbox
		// rows into the tamper-evident chain. CHAINER_ENABLED exists mainly
		// for tests and for a deliberately chainer-less instance in a
		// multi-instance rollout (every instance runs one; leader election
		// picks exactly one active chainer regardless, so disabling it here
		// is never required for correctness, only ever a deployment choice).
		chainerEnabled:  getenvBool("CHAINER_ENABLED", true),
		chainerInterval: getenvDuration("CHAINER_INTERVAL", audit.DefaultInterval),
		chainerBatch:    getenvInt("CHAINER_BATCH", audit.DefaultBatch),
		// The audit anchor job (Task 5.3, audit A2.4): on by default, like the
		// chainer and webhook worker, since every deployment benefits from an
		// off-box, tamper-evident checkpoint of each tenant's chain head (see
		// migration 0025 and internal/audit.AnchorJob's own doc comments for
		// why a hash chain alone cannot catch a privileged, internally
		// consistent rewrite of its own history). AUDIT_ANCHOR_ENABLED exists
		// for the same reasons CHAINER_ENABLED does: tests, and a
		// deliberately job-less instance in a multi-instance rollout (leader
		// election picks exactly one active job regardless). The hour default
		// mirrors audit.DefaultAnchorInterval: an anchor's value is that it
		// outlives any single rewrite attempt once shipped off-box, not tight
		// recency, so a coarse cadence is the right default.
		anchorEnabled:  getenvBool("AUDIT_ANCHOR_ENABLED", true),
		anchorInterval: getenvDuration("AUDIT_ANCHOR_INTERVAL", audit.DefaultAnchorInterval),
		// IDEMPOTENCY_TTL bounds how long a stored idempotency key blocks
		// reuse before it is treated as absent (Task 4.5, audit A1.4): the
		// default matches ledger.DefaultIdempotencyTTL and migration 0019's
		// own backfill window, and a deployment can widen it (say, to 7d)
		// for a slower-retrying client population, or narrow it, without a
		// code change.
		idempotencyTTL: getenvDuration("IDEMPOTENCY_TTL", ledger.DefaultIdempotencyTTL),
		// IDEMPOTENCY_SWEEP_INTERVAL is how often the background sweep
		// deletes expired idempotency_keys rows; it is purely a maintenance
		// cadence (GetIdempotencyKey already treats an expired row as
		// absent regardless of whether it has been physically deleted yet),
		// so a slower or faster cadence is never a correctness concern.
		idempotencySweepInterval: getenvDuration("IDEMPOTENCY_SWEEP_INTERVAL", time.Hour),
		// The webhook worker (Task 4.1, audit A7.1): on by default, like the
		// chainer, since every deployment needs it to fan tenant-subscribed
		// events out and delivered. WEBHOOKS_ENABLED exists for the same
		// reasons CHAINER_ENABLED does (tests, and a deliberately
		// worker-less instance in a multi-instance rollout): leader election
		// picks exactly one active worker regardless, so disabling it here
		// is never required for correctness.
		webhooksEnabled:         getenvBool("WEBHOOKS_ENABLED", true),
		webhookMaxAttempts:      getenvInt("WEBHOOK_MAX_ATTEMPTS", webhook.DefaultMaxAttempts),
		webhookDeliveryInterval: getenvDuration("WEBHOOK_DELIVERY_INTERVAL", webhook.DefaultInterval),
		// METRICS_COLLECT_INTERVAL (Task 5.6a, audit A6.1): how often the
		// operational-gauge collector (internal/opsmetrics) refreshes the
		// audit outbox backlog/lag, webhook delivery backlog, and
		// balance-invariant canary gauges from the database. Unlike the
		// chainer/webhook/anchor intervals above, this has no enable flag:
		// every instance runs one unconditionally (see opsmetrics's own doc
		// comment for why no leader election is needed here).
		metricsCollectInterval: getenvDuration("METRICS_COLLECT_INTERVAL", opsmetrics.DefaultInterval),
		// ADMIN_BOOTSTRAP (ADR-019, "First-boot admin provisioning"): on by
		// default, so a self-hoster is never locked out of their own admin
		// surface on a fresh production deployment. Set to false to opt out
		// (for example when an operator would rather mint their own admin
		// key via ledgerctl and never have one printed to the logs).
		adminBootstrap: getenvBool("ADMIN_BOOTSTRAP", true),
	}
	if cfg.databaseURL == "" {
		if !interactive {
			return config{}, errors.New("DATABASE_URL is required")
		}
		// Interactive first-run setup (ADR-019): only reached when stdin is
		// a real terminal, never in docker/systemd/CI. Prompts for the
		// connection, tests it, and offers to save it for next time.
		dbURL, save, savePath, err := runInteractiveSetup(os.Stdin, os.Stdout, readPasswordFromStdin, pingDB)
		if err != nil {
			return config{}, fmt.Errorf("interactive setup: %w", err)
		}
		if save {
			if err := saveDatabaseURL(savePath, dbURL); err != nil {
				return config{}, fmt.Errorf("save database url to %s: %w", savePath, err)
			}
			_, _ = fmt.Fprintf(os.Stdout, "Saved to %s\n", savePath)
		}
		cfg.databaseURL = dbURL
	}
	// A zero or negative ttl would stamp every idempotency key pre-expired,
	// silently disabling replay protection for every retry: fail fast at
	// boot instead of leaking that into a per-request behavior nobody asked
	// for.
	if cfg.idempotencyTTL <= 0 {
		return config{}, fmt.Errorf("IDEMPOTENCY_TTL must be positive, got %s", cfg.idempotencyTTL)
	}
	// Fail fast on a malformed DEFAULT_CURRENCY (for example "usd", "US", or
	// "DOLLARS") rather than booting successfully and only surfacing the
	// problem later as per-request 422s plus a silently-repeating seeder log
	// (seed.Seed also stamps this currency on every seeded account).
	if err := domain.Currency(cfg.defaultCurrency).Validate(); err != nil {
		return config{}, fmt.Errorf("DEFAULT_CURRENCY %q is invalid: %w", cfg.defaultCurrency, err)
	}
	// A SET-but-malformed LEDGER_MASTER_KEY (Task 6.2, audit A9.3) fails the
	// server's boot immediately, before anything else is constructed, rather
	// than being discovered lazily on the first real post: crypto.ParseMasterKey
	// is the exact same check crypto.NewCipher runs internally, so this is a
	// clean, early error with the same message a later construction would
	// produce. An EMPTY value is not an error here: it means PII encryption is
	// simply disabled (see cfg.masterKey's own doc comment above), which is a
	// valid, working configuration outside production.
	if cfg.masterKey != "" {
		if _, err := crypto.ParseMasterKey(cfg.masterKey); err != nil {
			return config{}, err
		}
	}
	// Safe-by-default deployment (ADR-015): a plain deployment must be
	// production-safe without an operator having to remember to turn
	// demo-shaped defaults off. These checks only fire when APP_ENV is
	// exactly "production", so go.sohag.pro (which keeps DEMO_MODE=true and
	// does not set APP_ENV=production) is unaffected.
	if cfg.env == "production" {
		if cfg.demoMode {
			return config{}, errors.New("DEMO_MODE must not be enabled when APP_ENV=production")
		}
		if cfg.demoAPIKey == defaultDemoAPIKey {
			return config{}, errors.New("refusing to boot in production with the published public demo api key; set DEMO_API_KEY to a real value")
		}
		// PII crypto-shredding (Task 6.2, audit A9.3) is mandatory in
		// production: every posting description a production deployment
		// stores must be encrypted and shreddable on a data-subject erasure
		// request, per docs/ops/retention-and-erasure.md. This does not
		// affect go.sohag.pro, which does not set APP_ENV=production (see
		// this block's own opening comment).
		if cfg.masterKey == "" {
			return config{}, errors.New("LEDGER_MASTER_KEY must be set when APP_ENV=production (PII crypto-shredding, Task 6.2, is mandatory in production)")
		}
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
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

	// Apply migrations before serving. On a single instance (ADR-013; this
	// deployment) this is the simplest correct option: the binary that
	// needs a column also creates it. Its safety depends on that
	// single-instance shape, NOT on goose providing any lock: the legacy
	// goose.UpContext called below takes no advisory lock or other
	// cross-process serialization (that only exists via goose.NewProvider
	// with a SessionLocker, which this code does not use), so two instances
	// racing this call could race each other. The deploy pipeline (Task
	// 5.6b, audit A4.3) additionally runs `./go-ledger migrate` against
	// production BEFORE swapping the new binary in, so a migration failure
	// aborts the deploy before any new code runs at all; this on-boot call
	// stays as a second, idempotent safety net (goose Up on an
	// already-current database is a no-op) for local dev, docker-compose,
	// and any environment that starts the server directly without a
	// separate migrate step. See docs/ops/server-setup.md for what to
	// revisit if this project ever goes multi-instance.
	if err := runMigrations(ctx, cfg.databaseURL); err != nil {
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
	resolver := auth.NewResolver(repo, cfg.authCacheTTL)

	// Built once here and reused as api.Deps.Admin below (see deps), so
	// provisionAdminKey and the /v1/admin surface share the same instance
	// instead of each constructing their own thin wrapper over repo.
	adminSvc := admin.NewService(repo)

	// Provision the public demo key (and, when configured, a high-limit
	// load-test key) before serving. Both are idempotent: a row with the same
	// key_hash already existing is treated as success, so a restart or the
	// four-hour demo wipe (which clears tenant DATA tables, never api_keys)
	// leaves the console working with the same key against a fresh ledger.
	if err := provisionAPIKeys(ctx, repo, cfg, logger); err != nil {
		return err
	}

	// First-boot admin provisioning (ADR-019): in demo mode the demo key
	// above already carries admin scope, so this is a no-op there; in
	// production, mints and logs a bootstrap admin key exactly once, only if
	// the default tenant does not already have a live one.
	if err := provisionAdminKey(ctx, repo, adminSvc, cfg, logger); err != nil {
		return err
	}

	// Seed static FX rates from FX_RATES (ADR-014). fx.Seed is a no-op on an
	// empty/unset value, and it only inserts a pair when its parsed rate or
	// spread differs from the current row, so restarting the server does not
	// pile up redundant history for a pair whose configured rate never changed.
	if err := fx.Seed(ctx, pool, cfg.fxRates); err != nil {
		return fmt.Errorf("seed fx rates: %w", err)
	}

	// PII crypto-shredding (Task 6.2, audit A9.3): a nil cipher (the default
	// when LEDGER_MASTER_KEY is unset) leaves encryption disabled everywhere
	// it is wired below, so posting descriptions are stored and returned as
	// plain strings exactly as before this feature existed. loadConfig
	// already fail-fast validated cfg.masterKey if it was set (and refuses to
	// boot at all with it unset when APP_ENV=production), so
	// crypto.NewCipher below cannot fail on a malformed key; repo satisfies
	// crypto.KeyStore directly (internal/postgres/crypto_keys.go), the same
	// *Repository value postgres.NewRepository already returned above.
	var cipher ledger.DescriptionCipher
	if cfg.masterKey != "" {
		c, err := crypto.NewCipher(cfg.masterKey, repo)
		if err != nil {
			return fmt.Errorf("construct pii cipher: %w", err)
		}
		cipher = c
	} else {
		logger.Warn("PII encryption is disabled: LEDGER_MASTER_KEY is not set, posting descriptions are stored in plaintext")
	}

	// Per-key rate limiter, wired AFTER the auth middleware inside api.New so
	// the key auth resolved into the context is present when it runs (see
	// ADR-012, "Per-key rate limiting", and internal/api/api.go).
	limiter := auth.NewLimiter(cfg.rateLimitRPM)
	// Negative-lookup throttle (Task 5.2, audit A2.5/A6.4): wired into
	// auth.HumaMiddleware INSIDE api.New, ahead of resolver.Resolve, so a
	// client IP over its failed-auth budget is rejected before the database
	// lookup runs at all. See its own doc comment (internal/auth/negativethrottle.go)
	// for the full reasoning and the bounded-map design.
	negativeThrottle := auth.NewNegativeThrottle(cfg.authNegativeMaxFailures, cfg.authNegativeWindow)
	transactions := ledger.NewTransactionService(repo, logger, otel.Tracer(ledgerTracerName),
		ledger.WithFXProvider(fx.NewDBProvider(pool)),
		ledger.WithIdempotencyTTL(cfg.idempotencyTTL),
		// PrePostHook (Task 6.1, audit A9.1) is scaffolding for an external
		// compliance/screening integration: wired explicitly to
		// NoopPrePostHook here, the same default TransactionService falls
		// back to on its own, so this deployment allows every transaction
		// exactly as it did before this hook existed. A real screening
		// integration replaces this one line with its own PrePostHook
		// implementation; nothing else in the posting path needs to change.
		ledger.WithPrePostHook(ledger.NoopPrePostHook{}),
		ledger.WithCipher(cipher))
	deps := api.Deps{
		Accounts: ledger.NewAccountService(repo,
			ledger.WithDefaultCurrency(domain.Currency(cfg.defaultCurrency)),
			ledger.WithAccountCipher(cipher)),
		Transactions: transactions,
		Audit:        ledger.NewAuditService(repo, ledger.WithAuditCipher(cipher)),
		Admin:        adminSvc,
		Reports:      ledger.NewReportService(repo),
		// Disputes resolves action=reverse through the SAME
		// TransactionService instance handling POST /v1/transactions: a
		// dispute-driven reversal goes through the identical screening,
		// policy, account-status, and encryption checks a caller-initiated
		// reversal does (Task 6.3, audit A9.2).
		Disputes: ledger.NewDisputeService(repo, transactions),
		// FX backs the /v1/admin/fx operations (ADR-020): live rate and markup
		// config, over the same pool fx.NewDBProvider(pool) above already reads.
		FX:               fx.NewAdminService(pool),
		DemoMode:         cfg.demoMode,
		Auth:             resolver,
		RateLimiter:      limiter,
		NegativeThrottle: negativeThrottle,
		Revision:         buildRevision,
	}
	metrics.BuildInfo.WithLabelValues(buildRevision).Set(1)

	// Demo seeder: reset and repopulate the demo ledger on startup and on an
	// interval, so the public demo always has fresh, realistic data. Stops on
	// shutdown. Gated on both DEMO_MODE and SEED_ENABLED (ADR-015,
	// "Safe-by-default deployment"): a plain deployment runs neither.
	if cfg.demoMode && cfg.seedEnabled {
		seedCtx, cancelSeed := context.WithCancel(context.Background())
		defer cancelSeed()
		go runSeeder(seedCtx, logger, pool, cfg.defaultTenant, cfg.defaultCurrency, cfg.seedInterval, domain.HashAPIKey(cfg.demoAPIKey))
	}

	// The audit chainer (ADR-017): every instance runs one, and leader
	// election (a Postgres session advisory lock) guarantees exactly one is
	// ever active, so this is safe to start unconditionally on every
	// instance in a multi-instance deployment. Stops on shutdown; releasing
	// its advisory lock and connection is best-effort (see Chainer.Run), but
	// even if that races the process exiting, Postgres releases a session
	// advisory lock the moment its connection closes, so no lock is ever
	// left stuck held by a dead process.
	if cfg.chainerEnabled {
		chainerCtx, cancelChainer := context.WithCancel(context.Background())
		defer cancelChainer()
		chainer := audit.NewChainer(pool, logger, cfg.chainerInterval, cfg.chainerBatch)
		go chainer.Run(chainerCtx)
	}

	// The audit anchor job (Task 5.3, audit A2.4): every instance runs one,
	// and leader election (a THIRD, distinct Postgres advisory lock key,
	// alongside the chainer's and the webhook worker's own) guarantees
	// exactly one is ever active, the same reasoning that makes starting the
	// chainer unconditionally on every instance safe above. It records, and
	// logs off-box, every tenant's current chain head on an interval; see
	// internal/audit.AnchorJob's own doc comment for why that off-box copy is
	// what makes a rewrite of already-anchored history detectable at all.
	if cfg.anchorEnabled {
		anchorCtx, cancelAnchor := context.WithCancel(context.Background())
		defer cancelAnchor()
		anchorJob := audit.NewAnchorJob(pool, logger, cfg.anchorInterval)
		go anchorJob.Run(anchorCtx)
	}

	// The webhook worker (Task 4.1, audit A7.1): fans posted transactions out
	// to tenant-subscribed callback URLs, signed and retried, driven off the
	// same audit_log stream the chainer produces. Every instance runs one;
	// leader election (a DIFFERENT Postgres advisory lock key than the
	// chainer's) guarantees exactly one is ever active, the same reasoning
	// that makes starting the chainer unconditionally on every instance
	// safe above.
	if cfg.webhooksEnabled {
		webhookCtx, cancelWebhook := context.WithCancel(context.Background())
		defer cancelWebhook()
		webhookWorker := webhook.NewWorker(pool, logger, webhook.Config{
			Interval:    cfg.webhookDeliveryInterval,
			MaxAttempts: cfg.webhookMaxAttempts,
		})
		go webhookWorker.Run(webhookCtx)
	}

	// The operational-gauge collector (Task 5.6a, audit A6.1): every
	// instance runs one unconditionally, unlike the chainer, anchor job, and
	// webhook worker above, since it only reads cross-tenant aggregates and
	// sets a handful of gauges, never writes to the database (see
	// internal/opsmetrics's own doc comment for why leader election is not
	// needed here). It keeps the audit outbox backlog/lag, webhook delivery
	// backlog, and balance-invariant canary gauges fresh on /metrics.
	collectorCtx, cancelCollector := context.WithCancel(context.Background())
	defer cancelCollector()
	collector := opsmetrics.NewCollector(pool, logger)
	go collector.Run(collectorCtx, cfg.metricsCollectInterval)

	// The idempotency key sweep (Task 4.5, audit A1.4): every deployment runs
	// one unconditionally (unlike the seeder and chainer, there is no
	// enable/disable flag, since deleting rows already past their expiry is
	// never wrong and never a deployment-shaped choice the way the demo
	// seeder or a multi-instance chainer topology is). It only reclaims
	// space; GetIdempotencyKey already treats an expired row as absent
	// regardless of whether this has run yet, so its cadence is a pure
	// maintenance concern, not a correctness one.
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	go runIdempotencySweep(sweepCtx, logger, repo, cfg.idempotencySweepInterval)

	router := chi.NewRouter()
	// No RealIP middleware: it trusts client-set forwarding headers and is
	// spoofable. Revisit with a trusted-proxy allowlist when one is in front.
	// maxBodyBytes is last (innermost, closest to the handlers) so RequestID,
	// Recoverer, the trace span, and the request log all still wrap and
	// observe a request it rejects.
	router.Use(middleware.RequestID, middleware.Recoverer, otelRouteName, slogLogger(logger), maxBodyBytes(api.MaxRequestBodyBytes))
	router.Get("/", web.Index)
	router.Get("/favicon.ico", web.Favicon)
	router.Get("/console", web.Console)
	router.Get("/console/config", web.ConsoleConfig(web.ConsoleConfigData{
		DemoMode:        cfg.demoMode,
		DefaultTenantID: cfg.defaultTenant,
	}))
	router.Handle("/static/*", http.StripPrefix("/static/", web.Assets()))
	router.Get("/the-ledger-book.pdf", web.BookPDF(goledger.LedgerBookPDF))
	api.RegisterPlayground(router)
	api.New(router, deps) // mounts /v1/*, /healthz, /openapi.*, /schemas/

	// Wrap the router in one OTel server span per request. Health checks are
	// filtered out so traces are real request work, not liveness noise; the
	// metrics server (below) is never wrapped (ADR-004, ADR-010).
	tracedHandler := otelhttp.NewHandler(
		router, "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }),
	)
	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           tracedHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Metrics on a separate loopback server so the Prometheus endpoint is never
	// exposed on the public interface (see ADR-004). Same timeouts as the main
	// server: it is loopback-only, but a slow scrape should not hold a
	// connection open indefinitely either.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", metrics.Handler())
	metricsSrv := &http.Server{
		Addr:              cfg.metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
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

	// RateLimiter is the SAME limiter instance api.Deps.RateLimiter uses above
	// (Task 5.1, audit A2.2): a shared bucket per API key means gRPC cannot be
	// used to spend a fresh budget after REST has exhausted a key's limit, or
	// vice versa. See rateLimitUnaryInterceptor's doc comment
	// (internal/grpcserver/interceptors.go) for the full reasoning.
	grpcSrv := grpcserver.NewGRPCServer(grpcserver.Deps{
		Accounts:     deps.Accounts,
		Transactions: deps.Transactions,
		Audit:        deps.Audit,
		Auth:         resolver,
		RateLimiter:  limiter,
	}, logger)
	// cfg.grpcAddr defaults to loopback-only; see its own doc comment in
	// loadConfig and the TLS/loopback decision recorded on
	// grpcserver.NewGRPCServer for why exposing it further requires TLS.
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

// runMigrations applies every pending embedded goose migration to
// databaseURL, up to the latest version. It is the testable core shared by
// the on-boot call in run() and the `migrate` subcommand's deploy-pipeline
// path (runMigrateCommand): goose uses database/sql, so it opens a
// short-lived handle over the pgx stdlib driver, separate from the app's pgx
// pool. Re-running it against an already-migrated database is a no-op (goose
// Up only applies versions not yet recorded as run), so both callers can
// invoke it without coordinating with each other.
func runMigrations(ctx context.Context, databaseURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open db for migrations: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// migrationStatus reports the database's current goose schema version
// (0 for a database with no migrations applied yet), backing `migrate
// status`. It never changes the database.
func migrationStatus(ctx context.Context, databaseURL string) (int64, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return 0, fmt.Errorf("open db for migration status: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return 0, fmt.Errorf("set goose dialect: %w", err)
	}
	version, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("get db version: %w", err)
	}
	return version, nil
}

// runMigrateCommand implements the `migrate` subcommand (Task 5.6b, audit
// A4.3). `migrate` with no further argument (or `migrate up`) applies every
// pending migration and exits 0, or non-zero with a clear error if
// DATABASE_URL is unset or unreachable, or a migration fails. `migrate
// status` reports the current schema version without changing anything,
// useful to sanity-check a box by hand. This is the exact step the deploy
// pipeline runs over SSH against the new binary, BEFORE swapping it into
// place: see .github/workflows/deploy.yml's "Migrate (pre-swap)" step, and
// docs/ops/server-setup.md for the full migrate-then-swap-then-health-check
// flow and its rollback.
func runMigrateCommand(args []string, logger *slog.Logger) error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errDatabaseURLRequired
	}

	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()

	switch sub {
	case "up":
		if err := runMigrations(ctx, databaseURL); err != nil {
			return err
		}
		logger.Info("migrations applied")
		return nil
	case "status":
		version, err := migrationStatus(ctx, databaseURL)
		if err != nil {
			return err
		}
		logger.Info("migration status", "version", version)
		return nil
	default:
		return fmt.Errorf("unknown migrate subcommand %q (want %q or %q)", sub, "up", "status")
	}
}

// apiKeyStore is the slice of the repository provisionAPIKeys needs: create a
// tenant row and insert a key row by hash. The postgres repository satisfies
// it; a test uses a fake. CreateTenant is needed because api_keys_tenant_fk
// (migration 0011, Task 2.1) requires the tenant to already exist: on a fresh
// deployment, the demo and load-test tenant ids have never been backfilled by
// anything, so provisionKey below must create each one before the key that
// names it.
type apiKeyStore interface {
	InsertAPIKey(ctx context.Context, k domain.APIKey, keyHash string) error
	CreateTenant(ctx context.Context, tenantID, name string) error
	// SetAPIKeyScopesByHash reconciles the demo key's scopes on every boot
	// (see provisionAPIKeys): InsertAPIKey's insert-or-ignore on key_hash
	// means a demo key row surviving from a previous boot never has its
	// scopes touched by a plain re-insert, so a deployment that already had
	// a demo key row before DEMO_MODE gained admin scope (or lost it) would
	// otherwise keep the row's stale scopes forever.
	SetAPIKeyScopesByHash(ctx context.Context, keyHash string, scopes []domain.Scope) error
}

// demoKeyScopes returns the scopes the demo key is provisioned with. In demo
// mode the demo key carries admin scope so the public operator console can
// exercise the admin surface (safe: the demo resets every four hours and is
// rate limited, ADR-019). Outside demo mode it never gets admin scope.
func demoKeyScopes(demoMode bool) []domain.Scope {
	if demoMode {
		return []domain.Scope{domain.ScopeRead, domain.ScopePost, domain.ScopeAdmin}
	}
	return []domain.Scope{domain.ScopeRead, domain.ScopePost}
}

// provisionAPIKeys provisions the public demo key when DEMO_MODE is on (ADR-015,
// "Safe-by-default deployment": demo behavior is opt-in, so a plain deployment
// with DEMO_MODE unset provisions and logs nothing about a demo key), and when
// LOAD_TEST_API_KEY is set both the single-tenant load-test key (kept for
// backward compat) and a set of LOAD_TEST_TENANTS high-limit keys spread
// across distinct tenants, idempotently. It hashes each plaintext and inserts
// one row per key; a unique-violation on key_hash (the row already exists
// from a previous boot) is treated as success, so this is safe to run on
// every startup and after the four-hour demo wipe (which never touches
// api_keys). The key plaintext is never logged, only the fact that a key is
// active (ADR-012).
func provisionAPIKeys(ctx context.Context, store apiKeyStore, cfg config, logger *slog.Logger) error {
	// The demo key is always reconciled, whether or not DEMO_MODE is on
	// (review fix, ADR-019 follow-up): provisionKey's InsertAPIKey is
	// insert-or-ignore on the unique key_hash, so a demo key row surviving
	// from a previous boot (for example a deployment that already has one
	// with the pre-admin-scope {read,post} set) would never have its scopes
	// touched by a plain re-insert, and demo-mode's admin elevation would be
	// a silent no-op forever. SetAPIKeyScopesByHash below runs unconditionally
	// so the row's scopes always match demoKeyScopes(cfg.demoMode) exactly,
	// including correctly DROPPING admin if a deployment flips out of demo
	// mode. It is keyed by the demo key's known plaintext hash, so it never
	// needs the row's generated id.
	demoKeyHash := domain.HashAPIKey(cfg.demoAPIKey)
	if cfg.demoMode {
		demoRPM := demoAPIKeyRateLimitRPM
		if err := provisionKey(ctx, store, domain.APIKey{
			TenantID:     cfg.defaultTenant,
			Name:         "demo",
			RateLimitRPM: &demoRPM,
			Scopes:       demoKeyScopes(cfg.demoMode),
		}, cfg.demoAPIKey); err != nil {
			return fmt.Errorf("provision demo api key: %w", err)
		}
		logger.Info("demo api key active", "tenant", cfg.defaultTenant, "rate_limit_rpm", demoRPM)
	}
	if err := store.SetAPIKeyScopesByHash(ctx, demoKeyHash, demoKeyScopes(cfg.demoMode)); err != nil {
		return fmt.Errorf("reconcile demo api key scopes: %w", err)
	}

	if cfg.loadTestKey == "" {
		return nil
	}

	loadRPM := loadTestAPIKeyRateLimitRPM
	if err := provisionKey(ctx, store, domain.APIKey{
		TenantID:     cfg.defaultTenant,
		Name:         "load-test",
		RateLimitRPM: &loadRPM,
	}, cfg.loadTestKey); err != nil {
		return fmt.Errorf("provision load-test api key: %w", err)
	}
	logger.Info("load-test api key active", "tenant", cfg.defaultTenant, "rate_limit_rpm", loadRPM)

	// Multi-tenant load-test keys: each tenant's audit hash chain serializes
	// same-tenant transaction posts through an in-process mutex, so a single
	// tenant's throughput is bounded no matter how high its rate limit is.
	// Spreading the load across LOAD_TEST_TENANTS distinct tenants lets
	// aggregate throughput scale instead. Tenant ids are deterministic so a
	// restart provisions the same set (and provisionKey's unique-violation
	// swallow keeps this idempotent).
	for i := range cfg.loadTestTenants {
		tenantID := fmt.Sprintf("00000000-0000-0000-0000-%012d", loadTestTenantBase+i)
		plaintext := fmt.Sprintf("%s-t%d", cfg.loadTestKey, i)
		if err := provisionKey(ctx, store, domain.APIKey{
			TenantID:     tenantID,
			Name:         fmt.Sprintf("load-test-%d", i),
			RateLimitRPM: &loadRPM,
		}, plaintext); err != nil {
			return fmt.Errorf("provision load-test tenant %d api key: %w", i, err)
		}
	}
	logger.Info("multi-tenant load-test api keys active", "tenants", cfg.loadTestTenants, "rate_limit_rpm", loadRPM)
	return nil
}

// provisionKey ensures k's tenant exists, then inserts one key row for the
// given plaintext, idempotently: a tenant that already exists
// (domain.ErrTenantAlreadyExists) and a key_hash that already exists (a
// unique-violation) are both swallowed as success. It never logs or returns
// the plaintext.
func provisionKey(ctx context.Context, store apiKeyStore, k domain.APIKey, plaintext string) error {
	tenantName := "provisioned-" + k.TenantID
	if err := store.CreateTenant(ctx, k.TenantID, tenantName); err != nil && !errors.Is(err, domain.ErrTenantAlreadyExists) {
		return fmt.Errorf("create tenant %s: %w", k.TenantID, err)
	}
	err := store.InsertAPIKey(ctx, k, domain.HashAPIKey(plaintext))
	if err != nil && !postgres.IsUniqueViolationError(err) {
		return err
	}
	return nil
}

// adminKeyIssuer is the slice of *admin.Service that provisionAdminKey needs:
// list a tenant's existing keys (to check whether a live admin-scoped one
// already exists) and mint a new one. *admin.Service satisfies it directly;
// a test uses a fake.
type adminKeyIssuer interface {
	ListKeys(ctx context.Context, tenantID string) ([]domain.APIKey, error)
	IssueKey(ctx context.Context, tenantID, name string, scopes []domain.Scope, expiresAt *time.Time) (string, domain.APIKey, error)
}

// tenantHasAdminKey reports whether keys already contains a live (non-revoked)
// key carrying admin scope.
func tenantHasAdminKey(keys []domain.APIKey) bool {
	for _, k := range keys {
		if k.RevokedAt == nil && k.HasScope(domain.ScopeAdmin) {
			return true
		}
	}
	return false
}

// provisionAdminKey auto-provisions a production admin credential on first
// boot (ADR-019, "First-boot admin provisioning"), so a self-hoster is never
// locked out of their own admin surface. It is a no-op in demo mode (the demo
// key itself already carries admin scope there, see demoKeyScopes) and when
// ADMIN_BOOTSTRAP is disabled. It is idempotent: if the default tenant
// already holds a live admin-scoped key, whether from a previous boot of
// this function or minted by hand via ledgerctl or the admin API, nothing is
// generated and nothing is logged. Otherwise it mints one admin-scoped key
// with no expiry and logs its plaintext exactly once: this is the only place
// in the codebase permitted to log a key's plaintext (contrast provisionKey,
// which never does).
func provisionAdminKey(ctx context.Context, store apiKeyStore, adminSvc adminKeyIssuer, cfg config, logger *slog.Logger) error {
	if cfg.demoMode || !cfg.adminBootstrap {
		return nil
	}

	// Ensure the default tenant row exists before checking or minting keys
	// against it, mirroring provisionKey's own ordering above: on a
	// brand-new deployment nothing has created it yet, and admin.Service.IssueKey
	// requires the tenant to already exist and be active.
	tenantName := "provisioned-" + cfg.defaultTenant
	if err := store.CreateTenant(ctx, cfg.defaultTenant, tenantName); err != nil && !errors.Is(err, domain.ErrTenantAlreadyExists) {
		return fmt.Errorf("provision admin key: create tenant %s: %w", cfg.defaultTenant, err)
	}

	existing, err := adminSvc.ListKeys(ctx, cfg.defaultTenant)
	if err != nil {
		return fmt.Errorf("provision admin key: list existing keys: %w", err)
	}
	if tenantHasAdminKey(existing) {
		return nil
	}

	plaintext, key, err := adminSvc.IssueKey(ctx, cfg.defaultTenant, "bootstrap-admin", []domain.Scope{domain.ScopeAdmin}, nil)
	if err != nil {
		// A suspended or closed default tenant (review fix): IssueKey fails
		// closed with a *domain.TenantNotActiveError via
		// requireActiveTenant, same as it would for any other tenant. That is
		// correct for the admin API, but here it is boot-time provisioning:
		// letting it propagate as a fatal error out of run() would mean an
		// operator who suspends the default tenant while it holds no live
		// admin key bricks the ENTIRE server on the next restart, not just the
		// admin surface. Treat it as nothing to provision instead: log a
		// warning and let boot continue. The tenant can be reactivated and
		// this function will mint the bootstrap key on the following restart.
		var tenantErr *domain.TenantNotActiveError
		if errors.As(err, &tenantErr) {
			logger.Warn("default tenant not active, skipping admin bootstrap key provisioning",
				"tenant", cfg.defaultTenant, "status", tenantErr.Status)
			return nil
		}
		return fmt.Errorf("provision admin key: issue key: %w", err)
	}
	logger.Info("provisioned bootstrap admin key: store it now, it will not be shown again",
		"tenant", cfg.defaultTenant, "key_id", key.ID, "api_key", plaintext)
	return nil
}

// runSeeder seeds the demo ledger once immediately, then every interval, until
// ctx is cancelled. A failed seed is logged and the loop continues. currency
// is stamped on every seeded account and posting (ADR-014): it is the same
// DEFAULT_CURRENCY used as the fallback for a caller-created account.
// demoKeyHash is passed through to seed.Seed, which refuses to reset tenant
// if it holds any api key other than the demo one (ADR-015).
func runSeeder(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, tenant, currency string, interval time.Duration, demoKeyHash string) {
	doSeed := func() {
		start := time.Now()
		// Wipe any visitor-created tenants first, so the public demo does not
		// accumulate them across resets (the seeder below only touches the fixed
		// demo ids). Demo-only and destructive; guarded by DEMO_MODE + SEED_ENABLED
		// at this call site (see PurgeNonDemoTenants). A purge failure is logged
		// but does not block the reseed.
		if purged, err := seed.PurgeNonDemoTenants(ctx, pool, seed.DemoTenantIDs(tenant)); err != nil {
			logger.Error("demo purge of non-demo tenants failed", "error", err)
		} else if purged > 0 {
			logger.Info("demo purged visitor-created tenants", "count", purged)
		}
		if err := seed.Demo(ctx, pool, tenant, time.Now(), currency, demoKeyHash); err != nil {
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

// idempotencySweeper is the slice of the repository the background sweep
// needs: a plain, best-effort maintenance delete, not a business transaction
// (see domain.Repository.SweepExpiredIdempotencyKeys). A fake in
// main_test.go exercises runIdempotencySweep without a real database.
type idempotencySweeper interface {
	SweepExpiredIdempotencyKeys(ctx context.Context) (int64, error)
}

// runIdempotencySweep deletes expired idempotency_keys rows once immediately,
// then every interval, until ctx is cancelled (Task 4.5, audit A1.4). It
// mirrors runSeeder's shape: a failed sweep is logged and the loop continues
// rather than exiting, since this is pure housekeeping (GetIdempotencyKey
// already treats an expired row as absent whether or not it has been
// physically deleted), never something a request is waiting on.
func runIdempotencySweep(ctx context.Context, logger *slog.Logger, sweeper idempotencySweeper, interval time.Duration) {
	doSweep := func() {
		n, err := sweeper.SweepExpiredIdempotencyKeys(ctx)
		if err != nil {
			logger.Error("idempotency key sweep failed", "error", err)
			return
		}
		if n > 0 {
			logger.Info("idempotency keys swept", "deleted", n)
		} else {
			logger.Debug("idempotency key sweep found nothing to delete")
		}
	}

	doSweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			doSweep()
		}
	}
}

// pendingSweeper is the slice of the ApprovalService the background TTL
// sweep needs (Task 8, ADR-025): a plain, best-effort maintenance sweep that
// expires stale pendings, not a per-request business call. A fake in
// main_test.go exercises runPendingSweep without a real database.
type pendingSweeper interface {
	SweepExpiredPending(ctx context.Context) (int64, error)
}

// runPendingSweep expires pending transactions left undecided past
// PENDING_TTL once immediately, then every interval, until ctx is cancelled
// (Task 8, ADR-025). It mirrors runIdempotencySweep's shape exactly: a
// failed sweep is logged and the loop continues rather than exiting, since
// this is pure housekeeping, never something a request is waiting on.
//
// The goroutine start call (go runPendingSweep(...)) is wired in Task 9,
// alongside the ApprovalService's own construction in main.go: this task
// only adds the runner and its narrow interface so Task 9 can wire it
// without also having to design the sweep loop.
func runPendingSweep(ctx context.Context, logger *slog.Logger, sweeper pendingSweeper, interval time.Duration) {
	doSweep := func() {
		n, err := sweeper.SweepExpiredPending(ctx)
		if err != nil {
			logger.Error("pending approval sweep failed", "error", err)
			return
		}
		if n > 0 {
			logger.Info("pending approvals swept", "expired", n)
		} else {
			logger.Debug("pending approval sweep found nothing to expire")
		}
	}

	doSweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			doSweep()
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

// maxBodyBytes caps every request body at limit bytes (see ADR-012, "Input
// hardening"), so one request can no longer become an arbitrarily large
// transaction or exhaust memory before validation runs. A request that
// declares a Content-Length over the limit is rejected with 413 before any
// handler reads a byte of it. The body reader is also wrapped with
// http.MaxBytesReader so a request with no declared length (or one that
// understates it, for example chunked transfer-encoding) is still capped once
// a handler starts reading; huma's write operations set the same limit as
// their own MaxBodyBytes, so that path also ends in a clean 413 rather than a
// generic read error. GET requests such as the console, static assets, and
// the playground are unaffected: they carry no body, so the check and the
// wrapper are both no-ops for them.
func maxBodyBytes(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// slogLogger logs one structured line per request: method, path, status,
// size, duration, the chi request id, and, when the request authenticated
// successfully, the resolved API key id and tenant id (follow-up F2, audit
// A6.3 partial). Those two are best-effort: auth.HumaMiddleware resolves the
// key and tenant deep inside huma's own request pipeline, on a context huma
// derives internally (see huma.WithContext), not on r itself, so there is no
// return value or r.Context() read after next.ServeHTTP that would see them
// directly. Instead, a *auth.RequestLogInfo box is installed on r's context
// here, BEFORE next.ServeHTTP runs; auth.SetRequestLogInfo (called from
// HumaMiddleware once auth resolves) writes into that same box through the
// pointer, and this middleware, holding the same pointer, reads whatever
// landed in it once next.ServeHTTP returns. An unauthenticated or
// failed-auth request simply leaves the box's fields at their zero value:
// key_id and tenant_id are omitted from the log line rather than logged
// empty, and nothing here can panic or error on that path. Never logs the
// key's plaintext or hash, only its id.
func slogLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			reqLog := &auth.RequestLogInfo{}
			r = r.WithContext(auth.WithRequestLogInfo(r.Context(), reqLog))
			next.ServeHTTP(ww, r)
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("elapsed", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			}
			if reqLog.KeyID != "" {
				attrs = append(attrs, slog.String("key_id", reqLog.KeyID))
			}
			if reqLog.TenantID != "" {
				attrs = append(attrs, slog.String("tenant_id", reqLog.TenantID))
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request", attrs...)
		})
	}
}

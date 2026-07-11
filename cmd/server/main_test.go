package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sohag-pro/go-ledger/internal/api"
	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// TestMaxBodyBytes exercises the router-level body-size middleware directly
// (see ADR-012, "Input hardening"): a request whose declared Content-Length
// exceeds the limit is rejected with 413 before the wrapped handler ever
// runs, a request within the limit reaches it, and a bodyless GET (the
// shape of the console, static assets, and the playground) is unaffected.
func TestMaxBodyBytes(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := maxBodyBytes(api.MaxRequestBodyBytes)(next)

	tests := []struct {
		name       string
		method     string
		bodySize   int
		wantStatus int
		wantCalled bool
	}{
		{
			name:       "oversized body rejected before the handler runs",
			method:     http.MethodPost,
			bodySize:   int(api.MaxRequestBodyBytes) + 1,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCalled: false,
		},
		{
			name:       "body within the limit reaches the handler",
			method:     http.MethodPost,
			bodySize:   int(api.MaxRequestBodyBytes),
			wantStatus: http.StatusOK,
			wantCalled: true,
		},
		{
			name:       "GET with no body is unaffected",
			method:     http.MethodGet,
			bodySize:   0,
			wantStatus: http.StatusOK,
			wantCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called = false
			var req *http.Request
			if tt.bodySize > 0 {
				req = httptest.NewRequest(tt.method, "/v1/transactions", strings.NewReader(strings.Repeat("a", tt.bodySize)))
			} else {
				req = httptest.NewRequest(tt.method, "/console", nil)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if called != tt.wantCalled {
				t.Errorf("handler called = %v, want %v", called, tt.wantCalled)
			}
		})
	}
}

// TestSlogLogger_KeyAndTenantEnrichment proves the follow-up F2 fix (audit
// A6.3 partial): slogLogger installs a *auth.RequestLogInfo box on the
// request's context before calling the wrapped handler, and, when something
// downstream (the real chain being auth.HumaMiddleware, simulated here
// directly since this test does not need a full huma pipeline) calls
// auth.SetRequestLogInfo against that same context, the resulting log line
// carries key_id and tenant_id. A request where nothing ever calls
// SetRequestLogInfo (the unauthenticated/failed-auth shape) logs cleanly
// with neither field present, and never panics.
func TestSlogLogger_KeyAndTenantEnrichment(t *testing.T) {
	t.Run("authenticated request carries key_id and tenant_id", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Mirrors what auth.HumaMiddleware does once a key resolves.
			auth.SetRequestLogInfo(r.Context(), "key-abc-123", "tenant-xyz-789")
			w.WriteHeader(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/v1/transactions", nil)
		rec := httptest.NewRecorder()
		slogLogger(logger)(next).ServeHTTP(rec, req)

		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("log line not JSON: %v (%q)", err, buf.String())
		}
		if line["key_id"] != "key-abc-123" {
			t.Errorf("key_id = %v, want %q", line["key_id"], "key-abc-123")
		}
		if line["tenant_id"] != "tenant-xyz-789" {
			t.Errorf("tenant_id = %v, want %q", line["tenant_id"], "tenant-xyz-789")
		}
		if line["method"] != http.MethodGet || line["path"] != "/v1/transactions" {
			t.Errorf("line = %v, missing expected method/path", line)
		}
	})

	t.Run("unauthenticated request omits key_id and tenant_id without error", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})

		req := httptest.NewRequest(http.MethodGet, "/v1/transactions", nil)
		rec := httptest.NewRecorder()
		slogLogger(logger)(next).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("log line not JSON: %v (%q)", err, buf.String())
		}
		if _, ok := line["key_id"]; ok {
			t.Errorf("line has key_id = %v, want absent for an unauthenticated request", line["key_id"])
		}
		if _, ok := line["tenant_id"]; ok {
			t.Errorf("line has tenant_id = %v, want absent for an unauthenticated request", line["tenant_id"])
		}
		if line["status"] != float64(http.StatusUnauthorized) {
			t.Errorf("status field = %v, want %d", line["status"], http.StatusUnauthorized)
		}
	})
}

// TestLoadConfig_ValidatesDefaultCurrency proves loadConfig fails fast on a
// malformed DEFAULT_CURRENCY (ADR-014's "New-account default currency is
// env-configured" only holds if the configured value is a well-formed code):
// without this check, a typo like "usd", "US", or "DOLLARS" boots the server
// successfully and only surfaces as per-request 422s plus a
// silently-repeating seeder log, instead of a clear boot-time error next to
// the existing DATABASE_URL check.
func TestLoadConfig_ValidatesDefaultCurrency(t *testing.T) {
	tests := []struct {
		name            string
		defaultCurrency string
		wantErr         bool
	}{
		{name: "unset falls back to USD", defaultCurrency: "", wantErr: false},
		{name: "valid three-letter uppercase code", defaultCurrency: "EUR", wantErr: false},
		{name: "lowercase rejected", defaultCurrency: "usd", wantErr: true},
		{name: "too short rejected", defaultCurrency: "US", wantErr: true},
		{name: "not a code rejected", defaultCurrency: "DOLLARS", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/db")
			if tt.defaultCurrency == "" {
				// t.Setenv cannot unset; loadConfig's getenv already treats an
				// empty string as unset, so setting it to "" here has the same
				// effect as the variable never being set.
				t.Setenv("DEFAULT_CURRENCY", "")
			} else {
				t.Setenv("DEFAULT_CURRENCY", tt.defaultCurrency)
			}

			_, err := loadConfig()
			if tt.wantErr && err == nil {
				t.Fatalf("loadConfig() with DEFAULT_CURRENCY=%q: got nil error, want an error", tt.defaultCurrency)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("loadConfig() with DEFAULT_CURRENCY=%q: got error %v, want nil", tt.defaultCurrency, err)
			}
		})
	}
}

// TestLoadConfig_ValidatesMasterKey proves LEDGER_MASTER_KEY (Task 6.2,
// audit A9.3) is validated at config-load time, fail-fast, before the server
// or any dependent component is constructed: an unset key is a valid
// configuration (PII encryption simply disabled), but a SET, malformed key
// (not base64, or not exactly 32 bytes once decoded) is rejected immediately,
// with the same error crypto.NewCipher would produce later.
func TestLoadConfig_ValidatesMasterKey(t *testing.T) {
	tests := []struct {
		name      string
		masterKey string
		wantErr   bool
	}{
		{name: "unset: PII encryption disabled, not an error", masterKey: "", wantErr: false},
		{name: "valid 32-byte base64 key", masterKey: testMasterKeyB64, wantErr: false},
		{name: "not valid base64 rejected", masterKey: "not-valid-base64!!!", wantErr: true},
		{name: "too short once decoded rejected", masterKey: "c2hvcnQ=", wantErr: true}, // base64("short")
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/db")
			t.Setenv("DEFAULT_CURRENCY", "")
			t.Setenv("LEDGER_MASTER_KEY", tt.masterKey)

			_, err := loadConfig()
			if tt.wantErr && err == nil {
				t.Fatalf("loadConfig() with LEDGER_MASTER_KEY=%q: got nil error, want an error", tt.masterKey)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("loadConfig() with LEDGER_MASTER_KEY=%q: got error %v, want nil", tt.masterKey, err)
			}
		})
	}
}

// TestLoadConfig_GRPCAddrDefaultsToLoopback proves GRPC_ADDR defaults to
// loopback-only (Task 5.1, audit A2.2, ADR-015 Phase 5): the gRPC server
// ships with no TLS of its own, so binding every interface by default (the
// prior ":9091") would serve it in the clear to anyone who could reach the
// box. An explicit GRPC_ADDR is still honored unchanged, so a deployment
// that has terminated TLS in front of gRPC can still widen it deliberately.
func TestLoadConfig_GRPCAddrDefaultsToLoopback(t *testing.T) {
	tests := []struct {
		name       string
		grpcAddr   string
		wantResult string
	}{
		{name: "unset defaults to loopback", grpcAddr: "", wantResult: "127.0.0.1:9091"},
		{name: "explicit value is honored unchanged", grpcAddr: "0.0.0.0:9091", wantResult: "0.0.0.0:9091"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/db")
			t.Setenv("DEFAULT_CURRENCY", "")
			// t.Setenv cannot unset; loadConfig's getenv already treats an
			// empty string as unset, so setting it to "" here has the same
			// effect as GRPC_ADDR never being set.
			t.Setenv("GRPC_ADDR", tt.grpcAddr)

			cfg, err := loadConfig()
			if err != nil {
				t.Fatalf("loadConfig() with GRPC_ADDR=%q: unexpected error: %v", tt.grpcAddr, err)
			}
			if cfg.grpcAddr != tt.wantResult {
				t.Errorf("grpcAddr = %q, want %q", cfg.grpcAddr, tt.wantResult)
			}
		})
	}
}

// TestLoadConfig_IdempotencyTTL proves IDEMPOTENCY_TTL (Task 4.5, audit
// A1.4) defaults to ledger.DefaultIdempotencyTTL (24h) when unset, accepts a
// widened or narrowed override (a week, a minute), and fails loadConfig fast
// on a zero or negative duration rather than silently stamping every
// idempotency key pre-expired.
func TestLoadConfig_IdempotencyTTL(t *testing.T) {
	tests := []struct {
		name           string
		idempotencyTTL string
		wantErr        bool
		wantTTL        time.Duration
	}{
		{name: "unset falls back to the 24h default", idempotencyTTL: "", wantErr: false, wantTTL: ledger.DefaultIdempotencyTTL},
		{name: "widened to a week", idempotencyTTL: "168h", wantErr: false, wantTTL: 168 * time.Hour},
		{name: "narrowed to a minute", idempotencyTTL: "1m", wantErr: false, wantTTL: time.Minute},
		{name: "zero rejected", idempotencyTTL: "0s", wantErr: true},
		{name: "negative rejected", idempotencyTTL: "-1h", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/db")
			t.Setenv("IDEMPOTENCY_TTL", tt.idempotencyTTL)

			cfg, err := loadConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("loadConfig() with IDEMPOTENCY_TTL=%q: got nil error, want an error", tt.idempotencyTTL)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() with IDEMPOTENCY_TTL=%q: got error %v, want nil", tt.idempotencyTTL, err)
			}
			if cfg.idempotencyTTL != tt.wantTTL {
				t.Errorf("idempotencyTTL = %s, want %s", cfg.idempotencyTTL, tt.wantTTL)
			}
		})
	}
}

// TestLoadConfig_SafeByDefault proves the safe-by-default deployment
// behavior of ADR-015: a plain development boot leaves both DEMO_MODE and
// SEED_ENABLED off, and a production boot (APP_ENV=production) refuses to
// start with either DEMO_MODE=true or the published public demo api key,
// while a production boot with a real DEMO_API_KEY and demo mode off
// succeeds.
// testMasterKeyB64 is a fixed, valid 32-byte LEDGER_MASTER_KEY (Task 6.2,
// audit A9.3), base64-encoded, used wherever a test needs loadConfig to see
// a well-formed key (for example a production boot, which requires one) but
// does not care about its actual bytes.
const testMasterKeyB64 = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=" // base64("01234567890123456789012345678901")

func TestLoadConfig_SafeByDefault(t *testing.T) {
	tests := []struct {
		name            string
		appEnv          string
		demoMode        string
		demoAPIKey      string
		masterKey       string
		wantErr         bool
		wantDemoMode    bool
		wantSeedEnabled bool
	}{
		{
			name: "development boot with no DEMO_MODE set stays fully off by default",
			// appEnv, demoMode, demoAPIKey, masterKey all left unset (empty):
			// PII encryption is optional outside production (Task 6.2).
			wantDemoMode:    false,
			wantSeedEnabled: false,
		},
		{ //nolint:gosec // demoAPIKey below is a test fixture, not a real credential
			name:       "production refuses DEMO_MODE=true",
			appEnv:     "production",
			demoMode:   "true",
			demoAPIKey: "glk_real_production_key",
			masterKey:  testMasterKeyB64,
			wantErr:    true,
		},
		{
			name:      "production refuses the published public demo api key",
			appEnv:    "production",
			masterKey: testMasterKeyB64,
			// demoAPIKey left unset so it defaults to the public constant.
			wantErr: true,
		},
		{ //nolint:gosec // demoAPIKey below is a test fixture, not a real credential
			name:       "production refuses an unset LEDGER_MASTER_KEY",
			appEnv:     "production",
			demoAPIKey: "glk_real_production_key",
			// masterKey left unset: PII crypto-shredding is mandatory in
			// production (Task 6.2, audit A9.3).
			wantErr: true,
		},
		{ //nolint:gosec // demoAPIKey below is a test fixture, not a real credential
			name:            "production boots with a real demo api key, demo mode off, and a master key",
			appEnv:          "production",
			demoAPIKey:      "glk_real_production_key",
			masterKey:       testMasterKeyB64,
			wantDemoMode:    false,
			wantSeedEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/db")
			t.Setenv("DEFAULT_CURRENCY", "")
			// t.Setenv cannot unset; loadConfig's getenv/getenvBool already
			// treat an empty string as unset, so setting these to "" when a
			// test case leaves them blank has the same effect as never
			// setting them.
			t.Setenv("APP_ENV", tt.appEnv)
			t.Setenv("DEMO_MODE", tt.demoMode)
			t.Setenv("DEMO_API_KEY", tt.demoAPIKey)
			t.Setenv("LEDGER_MASTER_KEY", tt.masterKey)
			t.Setenv("SEED_ENABLED", "")

			cfg, err := loadConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("loadConfig() with APP_ENV=%q DEMO_MODE=%q DEMO_API_KEY=%q: got nil error, want an error",
						tt.appEnv, tt.demoMode, tt.demoAPIKey)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig(): unexpected error: %v", err)
			}
			if cfg.demoMode != tt.wantDemoMode {
				t.Errorf("demoMode = %v, want %v", cfg.demoMode, tt.wantDemoMode)
			}
			if cfg.seedEnabled != tt.wantSeedEnabled {
				t.Errorf("seedEnabled = %v, want %v", cfg.seedEnabled, tt.wantSeedEnabled)
			}
		})
	}
}

// TestDemoKeyScopes proves demoKeyScopes (ADR-019, "First-boot admin
// provisioning") elevates the demo key to admin scope only in demo mode: the
// public operator console needs the demo key to exercise the admin surface,
// but a plain (non-demo) deployment's demo key must never carry admin scope.
func TestDemoKeyScopes(t *testing.T) {
	if got := demoKeyScopes(true); !hasScope(got, domain.ScopeAdmin) {
		t.Fatalf("demo mode should include admin scope, got %v", got)
	}
	if got := demoKeyScopes(false); hasScope(got, domain.ScopeAdmin) {
		t.Fatalf("non-demo demo key must NOT have admin scope, got %v", got)
	}
}

// hasScope reports whether scopes contains want. A small test helper: unlike
// domain.APIKey.HasScope, this checks literal membership in a raw
// []domain.Scope rather than the admin-is-a-superset rule, which is exactly
// what TestDemoKeyScopes needs to assert on demoKeyScopes' return value
// directly.
func hasScope(scopes []domain.Scope, want domain.Scope) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// fakeKeyStore is an in-memory api_keys store for the provisioning test. It
// mirrors the behaviours provisionAPIKeys depends on from the real postgres
// repository: a second insert of the same key_hash fails with a Postgres
// unique-violation (23505) rather than overwriting, a second CreateTenant for
// the same id fails with domain.ErrTenantAlreadyExists rather than
// overwriting, and a stored key resolves back by hash so the resolver can
// find it. It also implements ListKeys/IssueKey (the adminKeyIssuer
// interface) over the same in-memory map, so the provisionAdminKey unit
// tests below can use one fake for both roles instead of standing up a real
// admin.Service and database.
type fakeKeyStore struct {
	byHash map[string]domain.APIKey
	// tenants tracks status directly rather than a bare bool, so a test can
	// suspend or close a tenant (see setTenantStatus) and have IssueKey below
	// gate on it exactly the way admin.Service.requireActiveTenant does,
	// without standing up a real admin.Service and database.
	tenants map[string]domain.TenantStatus
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{byHash: map[string]domain.APIKey{}, tenants: map[string]domain.TenantStatus{}}
}

func (s *fakeKeyStore) CreateTenant(_ context.Context, tenantID, _ string) error {
	if _, exists := s.tenants[tenantID]; exists {
		return domain.ErrTenantAlreadyExists
	}
	s.tenants[tenantID] = domain.TenantActive
	return nil
}

// setTenantStatus overrides tenantID's status directly (it must already
// exist via CreateTenant), for tests that need to exercise IssueKey's
// active-tenant gate against a suspended or closed tenant.
func (s *fakeKeyStore) setTenantStatus(tenantID string, status domain.TenantStatus) {
	s.tenants[tenantID] = status
}

func (s *fakeKeyStore) InsertAPIKey(_ context.Context, k domain.APIKey, keyHash string) error {
	if _, exists := s.byHash[keyHash]; exists {
		// Same shape the real repository surfaces: a wrapped *pgconn.PgError
		// with SQLSTATE 23505, which postgres.IsUniqueViolationError unwraps.
		return &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	}
	if k.ID == "" {
		k.ID = "id-" + keyHash
	}
	if len(k.Scopes) == 0 {
		// Mirrors the real api_keys.scopes column default (migration 0012):
		// provisionAPIKeys never sets Scopes explicitly, so a real insert
		// picks up {read,post} from the DB default. This in-memory fake has
		// no DB default to fall back on, so it applies the same one here.
		k.Scopes = []domain.Scope{domain.ScopeRead, domain.ScopePost}
	}
	s.byHash[keyHash] = k
	return nil
}

// GetAPIKeyByHash looks up the stored key's tenant status from s.tenants,
// defaulting an unrecognized tenant (one never created via CreateTenant, the
// common case in tests that predate tenant-status gating) to active: every
// key provisionAPIKeys inserts here stands for a tenant that exists and is
// active unless a test explicitly suspends or closes it via setTenantStatus.
func (s *fakeKeyStore) GetAPIKeyByHash(_ context.Context, hash string) (domain.APIKey, error) {
	k, ok := s.byHash[hash]
	if !ok {
		return domain.APIKey{}, domain.ErrAPIKeyNotFound
	}
	if status, ok := s.tenants[k.TenantID]; ok {
		k.TenantStatus = status
	} else {
		k.TenantStatus = domain.TenantActive
	}
	return k, nil
}

// TouchAPIKeyLastUsed is a no-op: these provisioning tests do not assert on
// last_used_at, which is covered in internal/auth's own tests.
func (s *fakeKeyStore) TouchAPIKeyLastUsed(_ context.Context, _ string, _ time.Time) error {
	return nil
}

// SetAPIKeyScopesByHash overwrites the stored scopes for the key at hash,
// mirroring the real repository's ADR-019-follow-up reconciliation method
// closely enough for provisionAPIKeys's own tests: a no-op (not an error) if
// hash has no matching row, the same as the real UPDATE affecting zero rows.
func (s *fakeKeyStore) SetAPIKeyScopesByHash(_ context.Context, hash string, scopes []domain.Scope) error {
	k, ok := s.byHash[hash]
	if !ok {
		return nil
	}
	k.Scopes = scopes
	s.byHash[hash] = k
	return nil
}

// ListKeys returns every key stored for tenantID, mirroring
// admin.Service.ListKeys closely enough for provisionAdminKey's own tests:
// it never returns a key's plaintext (there is none stored here either), and
// includes revoked keys, matching the real ListAPIKeysByTenant.
func (s *fakeKeyStore) ListKeys(_ context.Context, tenantID string) ([]domain.APIKey, error) {
	var out []domain.APIKey
	for _, k := range s.byHash {
		if k.TenantID == tenantID {
			out = append(out, k)
		}
	}
	return out, nil
}

// IssueKey mints and stores a fresh key for tenantID, mirroring
// admin.Service.IssueKey closely enough for provisionAdminKey's own tests:
// it fails closed with a *domain.TenantNotActiveError against a suspended or
// closed tenant, the same gate real admin.Service.requireActiveTenant
// applies (this is what lets TestProvisionAdminKey_SuspendedTenant below
// exercise that path without a real admin.Service and database), and
// otherwise generates a real plaintext/hash pair via domain.GenerateAPIKey
// and inserts it through InsertAPIKey above, so a duplicate insert is caught
// the same way a real one would be.
func (s *fakeKeyStore) IssueKey(ctx context.Context, tenantID, name string, scopes []domain.Scope, expiresAt *time.Time) (string, domain.APIKey, error) {
	if status, ok := s.tenants[tenantID]; ok && status != domain.TenantActive {
		return "", domain.APIKey{}, &domain.TenantNotActiveError{TenantID: tenantID, Status: status}
	}
	plaintext, hash, err := domain.GenerateAPIKey()
	if err != nil {
		return "", domain.APIKey{}, err
	}
	k := domain.APIKey{TenantID: tenantID, Name: name, Scopes: scopes, ExpiresAt: expiresAt}
	if err := s.InsertAPIKey(ctx, k, hash); err != nil {
		return "", domain.APIKey{}, err
	}
	return plaintext, s.byHash[hash], nil
}

// TestProvisionAPIKeysIsIdempotent proves provisionAPIKeys is safe to run on
// every startup and after the four-hour demo wipe (ADR-012): calling it twice
// against the same store returns no error the second time (the demo key row
// already exists, a unique-violation is swallowed), and the demo key resolves
// through the resolver both times to the demo tenant with its tighter rate
// limit. It also confirms the load-test key is only provisioned when
// LOAD_TEST_API_KEY is set.
func TestProvisionAPIKeysIsIdempotent(t *testing.T) {
	const demoTenant = "00000000-0000-0000-0000-0000000000aa"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name            string
		loadTestKey     string
		loadTestTenants int
		wantRows        int // distinct api_keys rows after provisioning
	}{
		{name: "demo only", loadTestKey: "", wantRows: 1},
		{name: "demo plus load-test", loadTestKey: "glk_load_test_key", wantRows: 2},
		{name: "demo plus load-test plus multi-tenant", loadTestKey: "glk_load_test_key", loadTestTenants: 3, wantRows: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeKeyStore()
			cfg := config{
				defaultTenant:   demoTenant,
				demoMode:        true,
				demoAPIKey:      defaultDemoAPIKey,
				loadTestKey:     tt.loadTestKey,
				loadTestTenants: tt.loadTestTenants,
			}

			// First provisioning: fresh store.
			if err := provisionAPIKeys(context.Background(), store, cfg, logger); err != nil {
				t.Fatalf("first provision: %v", err)
			}
			// Second provisioning: every key row already exists. Must not error
			// (idempotent), and must not create duplicate rows.
			if err := provisionAPIKeys(context.Background(), store, cfg, logger); err != nil {
				t.Fatalf("second provision (idempotent) returned error: %v", err)
			}
			if got := len(store.byHash); got != tt.wantRows {
				t.Errorf("api key rows = %d, want %d (idempotent provisioning must not duplicate)", got, tt.wantRows)
			}

			// The demo key resolves through the resolver to the demo tenant
			// with its tighter rate limit.
			resolver := auth.NewResolver(store, time.Minute)
			key, err := resolver.Resolve(context.Background(), "Bearer "+defaultDemoAPIKey)
			if err != nil {
				t.Fatalf("resolve demo key: %v", err)
			}
			if key.TenantID != demoTenant {
				t.Errorf("demo key tenant = %q, want %q", key.TenantID, demoTenant)
			}
			if key.Name != "demo" {
				t.Errorf("demo key name = %q, want %q", key.Name, "demo")
			}
			if key.RateLimitRPM == nil || *key.RateLimitRPM != demoAPIKeyRateLimitRPM {
				t.Errorf("demo key rate_limit_rpm = %v, want %d", key.RateLimitRPM, demoAPIKeyRateLimitRPM)
			}

			// The load-test key resolves only when it was configured.
			_, loadErr := resolver.Resolve(context.Background(), "Bearer glk_load_test_key")
			if tt.loadTestKey == "" {
				if !errors.Is(loadErr, auth.ErrUnauthorized) {
					t.Errorf("load-test key resolve err = %v, want ErrUnauthorized when LOAD_TEST_API_KEY unset", loadErr)
				}
			} else if loadErr != nil {
				t.Errorf("load-test key resolve: %v", loadErr)
			}

			// Each multi-tenant load-test key resolves to its own distinct
			// tenant, so aggregate throughput across them is not bounded by
			// any one tenant's serialized audit-chain writes.
			seenTenants := map[string]bool{}
			for i := 0; i < tt.loadTestTenants; i++ {
				tenantKey, err := resolver.Resolve(context.Background(), "Bearer "+tt.loadTestKey+"-t"+strconv.Itoa(i))
				if err != nil {
					t.Fatalf("resolve multi-tenant load-test key %d: %v", i, err)
				}
				if tenantKey.TenantID == demoTenant {
					t.Errorf("multi-tenant load-test key %d resolved to the demo tenant, want a distinct tenant", i)
				}
				if seenTenants[tenantKey.TenantID] {
					t.Errorf("multi-tenant load-test key %d reused tenant %q already seen", i, tenantKey.TenantID)
				}
				seenTenants[tenantKey.TenantID] = true
			}
		})
	}
}

// TestProvisionAPIKeysDemoModeGate proves demo behavior is opt-in (ADR-015,
// "Safe-by-default deployment"): with demoMode false, provisionAPIKeys
// provisions no demo key row, the demo key does not resolve, and nothing
// about a demo key is logged.
func TestProvisionAPIKeysDemoModeGate(t *testing.T) {
	const demoTenant = "00000000-0000-0000-0000-0000000000bb"
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	store := newFakeKeyStore()
	cfg := config{
		defaultTenant: demoTenant,
		demoMode:      false,
		demoAPIKey:    defaultDemoAPIKey,
	}

	if err := provisionAPIKeys(context.Background(), store, cfg, logger); err != nil {
		t.Fatalf("provisionAPIKeys: %v", err)
	}

	if got := len(store.byHash); got != 0 {
		t.Errorf("api key rows = %d, want 0 when demo mode is off", got)
	}

	resolver := auth.NewResolver(store, time.Minute)
	if _, err := resolver.Resolve(context.Background(), "Bearer "+defaultDemoAPIKey); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("demo key resolve err = %v, want ErrUnauthorized when demo mode is off", err)
	}

	if strings.Contains(strings.ToLower(logBuf.String()), "demo") {
		t.Errorf("log mentions a demo key when demo mode is off: %q", logBuf.String())
	}
}

// fakeSweeper is an in-memory idempotencySweeper: each call to
// SweepExpiredIdempotencyKeys pops the next queued result (or reports an
// error) and signals a buffered channel so a test can wait for a specific
// number of calls without a real database or a sleep-based race.
type fakeSweeper struct {
	mu      sync.Mutex
	results []int64 // -1 means "return an error instead"
	calls   chan int64
}

func newFakeSweeper(results ...int64) *fakeSweeper {
	return &fakeSweeper{results: results, calls: make(chan int64, len(results)+8)}
}

func (s *fakeSweeper) SweepExpiredIdempotencyKeys(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	if len(s.results) > 0 {
		n = s.results[0]
		s.results = s.results[1:]
	}
	if n == -1 {
		s.calls <- -1
		return 0, errors.New("fake sweep failure")
	}
	s.calls <- n
	return n, nil
}

// TestRunIdempotencySweep proves the background sweep (Task 4.5, audit A1.4)
// runs once immediately (not waiting a full interval first), keeps running
// on the ticker until its context is cancelled, and survives a failed sweep
// (logged, not fatal) rather than exiting the loop.
func TestRunIdempotencySweep(t *testing.T) {
	sweeper := newFakeSweeper(3, -1, 0, 5)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runIdempotencySweep(ctx, logger, sweeper, time.Millisecond)
		close(done)
	}()

	// Wait for all four queued results to have been consumed: the immediate
	// call plus three ticks.
	for i := 0; i < 4; i++ {
		select {
		case <-sweeper.calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for sweep call %d", i+1)
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runIdempotencySweep did not return after its context was cancelled")
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "idempotency key sweep failed") {
		t.Errorf("log missing the failed-sweep error line: %q", logs)
	}
	if !strings.Contains(logs, "idempotency keys swept") {
		t.Errorf("log missing a successful non-zero sweep line: %q", logs)
	}
}

// TestProvisionAdminKey_DemoModeIsANoOp proves provisionAdminKey does nothing
// in demo mode (ADR-019): the demo key itself already carries admin scope
// there (demoKeyScopes), so a separate bootstrap-admin key would be
// redundant. No key is minted and nothing is logged.
func TestProvisionAdminKey_DemoModeIsANoOp(t *testing.T) {
	const tenant = "00000000-0000-0000-0000-0000000000cc"
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	store := newFakeKeyStore()
	cfg := config{defaultTenant: tenant, demoMode: true, adminBootstrap: true}

	if err := provisionAdminKey(context.Background(), store, store, cfg, logger); err != nil {
		t.Fatalf("provisionAdminKey: %v", err)
	}
	if got := len(store.byHash); got != 0 {
		t.Errorf("api key rows = %d, want 0 in demo mode", got)
	}
	if logBuf.Len() != 0 {
		t.Errorf("log = %q, want nothing logged in demo mode", logBuf.String())
	}
}

// TestProvisionAdminKey_BootstrapDisabledIsANoOp proves ADMIN_BOOTSTRAP=false
// suppresses auto-provisioning entirely, even outside demo mode: an operator
// who wants to mint their own admin key via ledgerctl, with no server-minted
// key ever appearing in the logs, can opt out this way.
func TestProvisionAdminKey_BootstrapDisabledIsANoOp(t *testing.T) {
	const tenant = "00000000-0000-0000-0000-0000000000dd"
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	store := newFakeKeyStore()
	cfg := config{defaultTenant: tenant, demoMode: false, adminBootstrap: false}

	if err := provisionAdminKey(context.Background(), store, store, cfg, logger); err != nil {
		t.Fatalf("provisionAdminKey: %v", err)
	}
	if got := len(store.byHash); got != 0 {
		t.Errorf("api key rows = %d, want 0 when ADMIN_BOOTSTRAP=false", got)
	}
	if logBuf.Len() != 0 {
		t.Errorf("log = %q, want nothing logged when ADMIN_BOOTSTRAP=false", logBuf.String())
	}
}

// TestProvisionAdminKey_ProdModeProvisionsOnceAndIsIdempotent proves the
// production auto-provisioning path (ADR-019): with no existing admin key,
// provisionAdminKey mints one, admin-scoped, and logs its plaintext exactly
// once; a second call (mirroring a restart) finds the admin key already
// there and mints nothing further, so the plaintext is never logged again
// and the key count does not grow.
func TestProvisionAdminKey_ProdModeProvisionsOnceAndIsIdempotent(t *testing.T) {
	const tenant = "00000000-0000-0000-0000-0000000000ee"
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	store := newFakeKeyStore()
	cfg := config{defaultTenant: tenant, demoMode: false, adminBootstrap: true}

	if err := provisionAdminKey(context.Background(), store, store, cfg, logger); err != nil {
		t.Fatalf("first provisionAdminKey: %v", err)
	}
	if got := len(store.byHash); got != 1 {
		t.Fatalf("api key rows after first boot = %d, want 1", got)
	}
	var minted domain.APIKey
	for _, k := range store.byHash {
		minted = k
	}
	if !minted.HasScope(domain.ScopeAdmin) {
		t.Errorf("provisioned key scopes = %v, want admin", minted.Scopes)
	}
	if !strings.Contains(logBuf.String(), "provisioned bootstrap admin key") {
		t.Errorf("log missing the one-time bootstrap-admin notice: %q", logBuf.String())
	}

	logBuf.Reset()
	if err := provisionAdminKey(context.Background(), store, store, cfg, logger); err != nil {
		t.Fatalf("second provisionAdminKey (idempotent): %v", err)
	}
	if got := len(store.byHash); got != 1 {
		t.Errorf("api key rows after second boot = %d, want still 1 (idempotent)", got)
	}
	if logBuf.Len() != 0 {
		t.Errorf("log on second boot = %q, want nothing (admin key already exists)", logBuf.String())
	}
}

// TestProvisionAdminKey_SuspendedDefaultTenantDoesNotBrickBoot proves the
// review fix for the first Task 2 gap (ADR-019 follow-up): an operator can
// suspend the default tenant (for example accidentally, or while it holds no
// live admin key) at any time. Before this fix, admin.Service.IssueKey's
// *domain.TenantNotActiveError propagated straight out of provisionAdminKey,
// and run() treats any error from it as fatal, so the NEXT server restart
// would refuse to boot at all until an operator noticed and reactivated the
// tenant by hand. provisionAdminKey must instead treat this as nothing to
// provision: log a warning and return nil so boot continues.
func TestProvisionAdminKey_SuspendedDefaultTenantDoesNotBrickBoot(t *testing.T) {
	const tenant = "00000000-0000-0000-0000-0000000000ff"
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	store := newFakeKeyStore()
	cfg := config{defaultTenant: tenant, demoMode: false, adminBootstrap: true}

	// The tenant already exists (mirroring provisionAdminKey's own
	// CreateTenant call finding domain.ErrTenantAlreadyExists on a real
	// restart) but is suspended, and holds no admin key yet: exactly the
	// state an operator can put the default tenant into.
	if err := store.CreateTenant(context.Background(), tenant, "suspended tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	store.setTenantStatus(tenant, domain.TenantSuspended)

	if err := provisionAdminKey(context.Background(), store, store, cfg, logger); err != nil {
		t.Fatalf("provisionAdminKey against a suspended tenant returned an error (would brick server boot on restart): %v", err)
	}
	if got := len(store.byHash); got != 0 {
		t.Errorf("api key rows = %d, want 0 (nothing provisioned into a suspended tenant)", got)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "default tenant not active") {
		t.Errorf("log = %q, want a warning naming the default tenant as not active", logs)
	}
	if !strings.Contains(logs, "level=WARN") {
		t.Errorf("log = %q, want a WARN-level line, not silence or an ERROR", logs)
	}
}

// fakeFailingIssuer is an adminKeyIssuer whose IssueKey always fails with a
// plain, unrelated error (never *domain.TenantNotActiveError), used by
// TestProvisionAdminKey_OtherIssueKeyErrorsStayFatal to prove the review
// fix's errors.As check is narrow: only a suspended/closed tenant is
// swallowed as a warning, every other IssueKey failure (a real database
// error, for example) must still propagate and fail boot.
type fakeFailingIssuer struct{}

func (fakeFailingIssuer) ListKeys(_ context.Context, _ string) ([]domain.APIKey, error) {
	return nil, nil
}

func (fakeFailingIssuer) IssueKey(_ context.Context, _, _ string, _ []domain.Scope, _ *time.Time) (string, domain.APIKey, error) {
	return "", domain.APIKey{}, errors.New("boom: some unrelated database error")
}

// TestProvisionAdminKey_OtherIssueKeyErrorsStayFatal proves the review fix
// does not overreach: a non-TenantNotActiveError failure from IssueKey must
// still return an error out of provisionAdminKey (and so still fail boot in
// run()), exactly as before this fix.
func TestProvisionAdminKey_OtherIssueKeyErrorsStayFatal(t *testing.T) {
	const tenant = "00000000-0000-0000-0000-0000000000ff1"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newFakeKeyStore() // used only for its CreateTenant

	err := provisionAdminKey(context.Background(), store, fakeFailingIssuer{}, config{
		defaultTenant:  tenant,
		demoMode:       false,
		adminBootstrap: true,
	}, logger)
	if err == nil {
		t.Fatal("provisionAdminKey with a non-TenantNotActiveError IssueKey failure returned nil, want a fatal error")
	}
	var tenantErr *domain.TenantNotActiveError
	if errors.As(err, &tenantErr) {
		t.Errorf("err = %v unexpectedly matched *domain.TenantNotActiveError; want it to have stayed a plain fatal error", err)
	}
}

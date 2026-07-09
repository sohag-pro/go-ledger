package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sohag-pro/go-ledger/internal/api"
	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
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

// fakeKeyStore is an in-memory api_keys store for the provisioning test. It
// mirrors the two behaviours provisionAPIKeys depends on from the real
// postgres repository: a second insert of the same key_hash fails with a
// Postgres unique-violation (23505) rather than overwriting, and a stored key
// resolves back by hash so the resolver can find it.
type fakeKeyStore struct {
	byHash map[string]domain.APIKey
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{byHash: map[string]domain.APIKey{}}
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
	s.byHash[keyHash] = k
	return nil
}

func (s *fakeKeyStore) GetAPIKeyByHash(_ context.Context, hash string) (domain.APIKey, error) {
	k, ok := s.byHash[hash]
	if !ok {
		return domain.APIKey{}, domain.ErrAPIKeyNotFound
	}
	return k, nil
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

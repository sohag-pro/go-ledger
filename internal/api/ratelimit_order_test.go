package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// newRateLimitedRouter wires the API with a per-key rate limiter whose default
// is defaultRPM, provisioning testAPIKeyPlaintext against testTenant so a
// request that presents it authenticates as a real key through the real auth
// middleware. It is the same wiring cmd/server does (auth first, then rate
// limit), so these tests assert the production ordering, not a test-only
// arrangement.
func newRateLimitedRouter(t *testing.T, repo domain.Repository, defaultRPM int) chi.Router {
	t.Helper()
	if err := repo.InsertAPIKey(t.Context(),
		domain.APIKey{TenantID: testTenant, Name: "rate limit order test key"},
		domain.HashAPIKey(testAPIKeyPlaintext),
	); err != nil {
		t.Fatalf("provision test key: %v", err)
	}

	r := chi.NewRouter()
	New(r, Deps{
		Accounts:     ledger.NewAccountService(repo),
		Transactions: ledger.NewTransactionService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), nil),
		Audit:        ledger.NewAuditService(repo),
		Auth:         auth.NewResolver(repo, time.Minute),
		RateLimiter:  auth.NewLimiter(defaultRPM),
	})
	return r
}

// TestRateLimitRunsAfterAuth proves the middleware order ADR-012 requires:
// the rate limiter sits behind auth on /v1, so a request with a valid key that
// is over its limit gets a 429, while a request with no key at all gets a 401
// from auth (never a rate-limit bypass, and never a 429 for an unauthenticated
// caller). The rate-limit middleware fails OPEN on a missing key, so if it ran
// before auth an unauthenticated flood would slip past it; asserting the 401
// here is what guards that ordering.
func TestRateLimitRunsAfterAuth(t *testing.T) {
	// A default of 1 rpm means burst == 1: the first valid-key request is
	// allowed, the second is over the limit.
	r := newRateLimitedRouter(t, newFakeRepo(), 1)

	// First request with the valid key: allowed (200 on a GET list).
	firstRec := doGet(t, r, "/v1/accounts", testAPIKeyPlaintext)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first authed request status = %d, want 200 (%s)", firstRec.Code, firstRec.Body.String())
	}

	// Second request with the same valid key: over the limit, so 429 from the
	// rate limiter, which only runs because auth resolved the key first.
	overLimitRec := doGet(t, r, "/v1/accounts", testAPIKeyPlaintext)
	if overLimitRec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit authed request status = %d, want 429 (%s)", overLimitRec.Code, overLimitRec.Body.String())
	}
	if overLimitRec.Header().Get("Retry-After") == "" {
		t.Errorf("429 response missing Retry-After header")
	}

	// A request with NO key must be rejected by auth (401) before the rate
	// limiter ever sees it: auth runs first, and the limiter's fail-open on a
	// missing key must not turn into a bypass for unauthenticated traffic.
	noKeyReq := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	noKeyRec := httptest.NewRecorder()
	r.ServeHTTP(noKeyRec, noKeyReq)
	if noKeyRec.Code != http.StatusUnauthorized {
		t.Fatalf("no-key request status = %d, want 401 from auth (%s)", noKeyRec.Code, noKeyRec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(noKeyRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("no-key body not JSON: %v (%q)", err, noKeyRec.Body.String())
	}
	if body["status"] != float64(401) || body["title"] != "Unauthorized" {
		t.Errorf("no-key body = %v, want status 401 title Unauthorized (auth fired, not the limiter)", body)
	}
}

// doGet issues a bare GET with the given bearer key and returns the recorder.
func doGet(t *testing.T, r chi.Router, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

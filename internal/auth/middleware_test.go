package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// tenantEchoOutput is a minimal huma response body that hands back whatever
// tenant TenantFromContext found, so tests can assert what the middleware put
// there without a real service behind it.
type tenantEchoOutput struct {
	Body struct {
		Tenant string `json:"tenant"`
	}
}

// healthEchoOutput stands in for the real /healthz response body, so the
// non-/v1 test operation has an actual 200 body instead of huma's default 204
// for an empty response.
type healthEchoOutput struct {
	Body struct {
		Status string `json:"status"`
	}
}

// discardLogger never writes anywhere, so tests do not spam stderr with the
// expected failure-path log lines.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestAPI builds a huma test API with HumaMiddleware installed: one GET
// and one POST operation under /v1 (both echo the resolved tenant, so scope
// enforcement tests can tell a 200 apart from a rejection), one operation
// under /v1/admin/ (Task 2.2: no real admin routes exist yet, but the scope
// rule is wired and needs something to exercise it against), and one
// operation outside /v1 (standing in for /healthz) that requires nothing.
func newTestAPI(t *testing.T, resolver *Resolver) (http.Handler, humatest.TestAPI) {
	t.Helper()

	mux, api := humatest.New(t)
	api.UseMiddleware(HumaMiddleware(api, resolver, discardLogger()))

	echoTenant := func(ctx context.Context, _ *struct{}) (*tenantEchoOutput, error) {
		tenant, _ := TenantFromContext(ctx)
		out := &tenantEchoOutput{}
		out.Body.Tenant = tenant
		return out, nil
	}

	huma.Register(api, huma.Operation{
		OperationID: "v1-echo-tenant-get",
		Method:      http.MethodGet,
		Path:        "/v1/thing",
	}, echoTenant)

	huma.Register(api, huma.Operation{
		OperationID: "v1-echo-tenant-post",
		Method:      http.MethodPost,
		Path:        "/v1/thing",
	}, echoTenant)

	huma.Register(api, huma.Operation{
		OperationID: "v1-admin-thing-get",
		Method:      http.MethodGet,
		Path:        "/v1/admin/thing",
	}, echoTenant)

	huma.Register(api, huma.Operation{
		OperationID: "open-health",
		Method:      http.MethodGet,
		Path:        "/healthz",
	}, func(_ context.Context, _ *struct{}) (*healthEchoOutput, error) {
		out := &healthEchoOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	return mux, api
}

func TestHumaMiddleware_NoKeyOnV1Is401(t *testing.T) {
	t.Parallel()

	resolver := NewResolver(newFakeLookup(nil), time.Minute)
	mux, _ := newTestAPI(t, resolver)

	req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["status"] != float64(401) || body["title"] != "Unauthorized" {
		t.Errorf("body = %v, want status 401 title Unauthorized", body)
	}
	if _, hasDetail := body["detail"]; hasDetail {
		t.Errorf("body has an unexpected detail field: %v", body)
	}
}

func TestHumaMiddleware_InvalidKeyOnV1Is401(t *testing.T) {
	t.Parallel()

	resolver := NewResolver(newFakeLookup(nil), time.Minute)
	mux, _ := newTestAPI(t, resolver)

	req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	req.Header.Set(authHeader, "Bearer glk_never-issued")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
}

func TestHumaMiddleware_ValidKeyInjectsTenantAndCallsNext(t *testing.T) {
	t.Parallel()

	const plaintext = "glk_middleware-test-key"
	key := domain.APIKey{ID: "key-1", TenantID: "tenant-xyz", Name: "test key", TenantStatus: domain.TenantActive, Scopes: []domain.Scope{domain.ScopeRead, domain.ScopePost}}
	resolver := NewResolver(newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key}), time.Minute)
	mux, _ := newTestAPI(t, resolver)

	req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	req.Header.Set(authHeader, "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var out struct {
		Tenant string `json:"tenant"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Tenant != key.TenantID {
		t.Errorf("tenant seen by handler = %q, want %q", out.Tenant, key.TenantID)
	}
}

// TestHumaMiddleware_SuspendedOrClosedTenantIs403 proves a valid key whose
// tenant is suspended or closed is rejected with 403 Forbidden, not 401: the
// credential itself is fine, only the tenant is gated (Task 2.1, ADR-015).
// The response names the reason, unlike the deliberately-generic 401 body.
func TestHumaMiddleware_SuspendedOrClosedTenantIs403(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status domain.TenantStatus
	}{
		{"suspended", domain.TenantSuspended},
		{"closed", domain.TenantClosed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plaintext := "glk_middleware-test-key-" + tt.name
			key := domain.APIKey{ID: "key-" + tt.name, TenantID: "tenant-" + tt.name, Name: "test key", TenantStatus: tt.status}
			resolver := NewResolver(newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key}), time.Minute)
			mux, _ := newTestAPI(t, resolver)

			req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
			req.Header.Set(authHeader, "Bearer "+plaintext)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not JSON: %v (%q)", err, rec.Body.String())
			}
			if body["status"] != float64(403) || body["title"] != "Forbidden" {
				t.Errorf("body = %v, want status 403 title Forbidden", body)
			}
			wantDetail := "tenant is " + string(tt.status)
			if body["detail"] != wantDetail {
				t.Errorf("body detail = %v, want %q", body["detail"], wantDetail)
			}
		})
	}
}

func TestHumaMiddleware_NonV1PathIsOpenWithNoKey(t *testing.T) {
	t.Parallel()

	resolver := NewResolver(newFakeLookup(nil), time.Minute)
	mux, _ := newTestAPI(t, resolver)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no Authorization header (%s)", rec.Code, rec.Body.String())
	}
}

func TestHumaMiddleware_NilResolverFailsClosedOnV1(t *testing.T) {
	t.Parallel()

	mux, _ := newTestAPI(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a misconfigured server must not silently allow /v1 through)", rec.Code)
	}
}

func TestIsV1Path(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want bool
	}{
		{"/v1", true},
		{"/v1/accounts", true},
		{"/v1/accounts/{id}", true},
		{"/healthz", false},
		{"/openapi.json", false},
		{"/", false},
		{"/v1beta/accounts", false},
	}
	for _, tt := range tests {
		if got := isV1Path(tt.path); got != tt.want {
			t.Errorf("isV1Path(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestMiddleware_NetHTTPFallback(t *testing.T) {
	t.Parallel()

	const plaintext = "glk_nethttp-test-key"
	key := domain.APIKey{ID: "key-2", TenantID: "tenant-abc", Name: "test key", TenantStatus: domain.TenantActive, Scopes: []domain.Scope{domain.ScopeRead, domain.ScopePost}}
	resolver := NewResolver(newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key}), time.Minute)

	var sawTenant string
	handler := Middleware(resolver, discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, _ := TenantFromContext(r.Context())
		sawTenant = tenant
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("no key is 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != `{"status":401,"title":"Unauthorized"}` {
			t.Errorf("body = %q, want the exact 401 problem+json shape", rec.Body.String())
		}
	})

	t.Run("valid key injects tenant and calls next", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		req.Header.Set(authHeader, "Bearer "+plaintext)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if sawTenant != key.TenantID {
			t.Errorf("tenant seen by handler = %q, want %q", sawTenant, key.TenantID)
		}
	})

	t.Run("suspended tenant is 403 naming the reason", func(t *testing.T) {
		const suspendedPlaintext = "glk_nethttp-suspended-key"
		suspendedKey := domain.APIKey{ID: "key-3", TenantID: "tenant-susp", Name: "test key", TenantStatus: domain.TenantSuspended}
		suspendedResolver := NewResolver(newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(suspendedPlaintext): suspendedKey}), time.Minute)
		called := false
		suspendedHandler := Middleware(suspendedResolver, discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		req.Header.Set(authHeader, "Bearer "+suspendedPlaintext)
		rec := httptest.NewRecorder()
		suspendedHandler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
		}
		if called {
			t.Error("handler should not run for a suspended tenant")
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body not JSON: %v (%q)", err, rec.Body.String())
		}
		if body["detail"] != "tenant is suspended" {
			t.Errorf("body detail = %v, want %q", body["detail"], "tenant is suspended")
		}
	})
}

// --- Scope enforcement (Task 2.2): these exercise HumaMiddleware itself, not
// just RequiredHTTPScope/CheckScope in isolation. ---

// resolverWithScopedKey returns a resolver whose single key, reachable with
// plaintext, carries exactly scopes.
func resolverWithScopedKey(plaintext string, scopes ...domain.Scope) *Resolver {
	key := domain.APIKey{ID: "key-scoped", TenantID: "tenant-scoped", Name: "scoped test key", TenantStatus: domain.TenantActive, Scopes: scopes}
	return NewResolver(newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key}), time.Minute)
}

func TestHumaMiddleware_ReadOnlyKeyAllowedOnGetRejectedOnPost(t *testing.T) {
	t.Parallel()

	const plaintext = "glk_scope-read-only-key"
	resolver := resolverWithScopedKey(plaintext, domain.ScopeRead)
	mux, _ := newTestAPI(t, resolver)

	getReq := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	getReq.Header.Set(authHeader, "Bearer "+plaintext)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET with read-only key: status = %d, want 200 (%s)", getRec.Code, getRec.Body.String())
	}

	postReq := httptest.NewRequest(http.MethodPost, "/v1/thing", nil)
	postReq.Header.Set(authHeader, "Bearer "+plaintext)
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusForbidden {
		t.Fatalf("POST with read-only key: status = %d, want 403 (%s)", postRec.Code, postRec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(postRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, postRec.Body.String())
	}
	if body["detail"] != "missing required scope: post" {
		t.Errorf("body detail = %v, want %q", body["detail"], "missing required scope: post")
	}
}

func TestHumaMiddleware_PostScopedKeyAllowedOnPostRejectedOnGet(t *testing.T) {
	t.Parallel()

	const plaintext = "glk_scope-post-only-key"
	resolver := resolverWithScopedKey(plaintext, domain.ScopePost)
	mux, _ := newTestAPI(t, resolver)

	postReq := httptest.NewRequest(http.MethodPost, "/v1/thing", nil)
	postReq.Header.Set(authHeader, "Bearer "+plaintext)
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Fatalf("POST with post-only key: status = %d, want 200 (%s)", postRec.Code, postRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	getReq.Header.Set(authHeader, "Bearer "+plaintext)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusForbidden {
		t.Fatalf("GET with post-only key: status = %d, want 403 (%s)", getRec.Code, getRec.Body.String())
	}
}

// TestHumaMiddleware_AdminKeyAllowedEverywhere proves ScopeAdmin is a
// superset (the chosen model, Task 2.2): a key carrying only ScopeAdmin can
// call GET, POST, and the /v1/admin/ path without also listing read/post.
func TestHumaMiddleware_AdminKeyAllowedEverywhere(t *testing.T) {
	t.Parallel()

	const plaintext = "glk_scope-admin-key"
	resolver := resolverWithScopedKey(plaintext, domain.ScopeAdmin)
	mux, _ := newTestAPI(t, resolver)

	for _, tt := range []struct {
		name   string
		method string
		path   string
	}{
		{"get", http.MethodGet, "/v1/thing"},
		{"post", http.MethodPost, "/v1/thing"},
		{"admin path", http.MethodGet, "/v1/admin/thing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set(authHeader, "Bearer "+plaintext)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s with admin key: status = %d, want 200 (%s)", tt.method, tt.path, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHumaMiddleware_AdminPathRequiresAdminScope proves a path under
// /v1/admin/ requires ScopeAdmin regardless of method, so a key with read and
// post (but not admin) still cannot reach it, even via GET.
func TestHumaMiddleware_AdminPathRequiresAdminScope(t *testing.T) {
	t.Parallel()

	const plaintext = "glk_scope-read-post-not-admin-key"
	resolver := resolverWithScopedKey(plaintext, domain.ScopeRead, domain.ScopePost)
	mux, _ := newTestAPI(t, resolver)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/thing", nil)
	req.Header.Set(authHeader, "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (a read+post key must not reach /v1/admin/) (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["detail"] != "missing required scope: admin" {
		t.Errorf("body detail = %v, want %q", body["detail"], "missing required scope: admin")
	}
}

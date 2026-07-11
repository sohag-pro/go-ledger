package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// newFXAdminTestRouter wires the full API against a real Postgres-backed
// repository (sharedAuthPool, the same testcontainer TestMain in
// auth_test.go starts for this package) and an FX AdminService over the same
// pool, provisioning a fresh tenant with an admin-scoped key and a post-only
// key. fx.AdminService wraps sqlc.Queries directly, not domain.Repository, so
// (unlike newAdminTestRouter's fakeRepo) this needs the real database.
func newFXAdminTestRouter(t *testing.T) (r chi.Router, adminKey, nonAdminKey string) {
	t.Helper()
	if authPoolErr != nil {
		t.Skipf("skipping integration test: %v", authPoolErr)
	}

	repo := postgres.NewRepository(sharedAuthPool)
	ctx := context.Background()
	tenant := newUUID(t)
	if err := repo.CreateTenant(ctx, tenant, "fx admin test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	const adminPlaintext = "glk_fx-admin-test-admin-scoped-key" //nolint:gosec // test fixture key, not a real credential
	const postOnlyPlaintext = "glk_fx-admin-test-post-only-key" //nolint:gosec // test fixture key, not a real credential
	if err := repo.InsertAPIKey(ctx,
		domain.APIKey{TenantID: tenant, Name: "admin", Scopes: []domain.Scope{domain.ScopeAdmin}},
		domain.HashAPIKey(adminPlaintext),
	); err != nil {
		t.Fatalf("provision admin key: %v", err)
	}
	if err := repo.InsertAPIKey(ctx,
		domain.APIKey{TenantID: tenant, Name: "post-only", Scopes: []domain.Scope{domain.ScopePost}},
		domain.HashAPIKey(postOnlyPlaintext),
	); err != nil {
		t.Fatalf("provision post-only key: %v", err)
	}

	router := chi.NewRouter()
	New(router, Deps{
		Admin: admin.NewService(repo),
		Auth:  auth.NewResolver(repo, time.Minute),
		FX:    fx.NewAdminService(sharedAuthPool),
	})
	return router, adminPlaintext, postOnlyPlaintext
}

// TestFXAdminEndpoints covers the /v1/admin/fx surface end to end (ADR-020):
// inserting a rate with no spread, overriding it with a spread, listing
// current rates, setting and getting the global markup default, the domain
// validation errors (same-currency pair) mapping to 422, huma's own
// struct-tag validation (spread_bps out of range) also mapping to 422, and
// every one of these being rejected with 403 for a non-admin key.
func TestFXAdminEndpoints(t *testing.T) {
	r, adminKey, nonAdminKey := newFXAdminTestRouter(t)

	t.Run("insert rate with no spread", func(t *testing.T) {
		rec := doAs(t, r, adminKey, http.MethodPost, "/v1/admin/fx/rates", map[string]any{
			"base": "USD", "quote": "EUR", "mid_rate_e8": 92_000_000,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
		var body FXRateBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.SpreadBps != nil {
			t.Errorf("SpreadBps = %v, want nil", *body.SpreadBps)
		}
		if body.MidRateE8 != 92_000_000 {
			t.Errorf("MidRateE8 = %d, want 92000000", body.MidRateE8)
		}
	})

	t.Run("insert rate with a spread override", func(t *testing.T) {
		rec := doAs(t, r, adminKey, http.MethodPost, "/v1/admin/fx/rates", map[string]any{
			"base": "USD", "quote": "EUR", "mid_rate_e8": 92_500_000, "spread_bps": 25,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
		var body FXRateBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.SpreadBps == nil || *body.SpreadBps != 25 {
			t.Errorf("SpreadBps = %v, want 25", body.SpreadBps)
		}
	})

	t.Run("list rates shows the effective spread", func(t *testing.T) {
		rec := doAs(t, r, adminKey, http.MethodGet, "/v1/admin/fx/rates", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var out struct {
			Rates []FXRateBody `json:"rates"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		found := false
		for _, rate := range out.Rates {
			if rate.Base == "USD" && rate.Quote == "EUR" {
				found = true
				if rate.EffectiveSpreadBps != 25 {
					t.Errorf("USD:EUR EffectiveSpreadBps = %d, want 25", rate.EffectiveSpreadBps)
				}
			}
		}
		if !found {
			t.Errorf("rates = %+v, want a USD:EUR entry", out.Rates)
		}
	})

	t.Run("set and get the default markup", func(t *testing.T) {
		setRec := doAs(t, r, adminKey, http.MethodPost, "/v1/admin/fx/markup", map[string]any{
			"default_spread_bps": 50,
		})
		if setRec.Code != http.StatusCreated {
			t.Fatalf("set status = %d, want 201 (%s)", setRec.Code, setRec.Body.String())
		}

		getRec := doAs(t, r, adminKey, http.MethodGet, "/v1/admin/fx/markup", nil)
		if getRec.Code != http.StatusOK {
			t.Fatalf("get status = %d, want 200 (%s)", getRec.Code, getRec.Body.String())
		}
		var out struct {
			Global *FXMarkupBody `json:"global"`
			Tenant *FXMarkupBody `json:"tenant"`
		}
		if err := json.Unmarshal(getRec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Global == nil || out.Global.DefaultSpreadBps != 50 {
			t.Errorf("Global = %+v, want DefaultSpreadBps 50", out.Global)
		}
	})

	t.Run("same-currency pair is 422", func(t *testing.T) {
		rec := doAs(t, r, adminKey, http.MethodPost, "/v1/admin/fx/rates", map[string]any{
			"base": "USD", "quote": "USD", "mid_rate_e8": 100_000_000,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("spread at the excluded upper bound (10000) is 422 via huma validation", func(t *testing.T) {
		// Valid range is [0, 10000): 10000 itself is the first invalid value,
		// so it must be rejected as clearly as the 20000 case further out of
		// range would be.
		rec := doAs(t, r, adminKey, http.MethodPost, "/v1/admin/fx/rates", map[string]any{
			"base": "USD", "quote": "GBP", "mid_rate_e8": 100_000_000, "spread_bps": 10_000,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("scoped write to an unknown tenant is 422", func(t *testing.T) {
		rec := doAs(t, r, adminKey, http.MethodPost, "/v1/admin/fx/rates", map[string]any{
			"tenant_id": newUUID(t), "base": "USD", "quote": "CAD", "mid_rate_e8": 100_000_000,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-admin key is 403 on every fx route", func(t *testing.T) {
		cases := []struct {
			method, path string
			body         any
		}{
			{http.MethodPost, "/v1/admin/fx/rates", map[string]any{"base": "USD", "quote": "JPY", "mid_rate_e8": 100_000_000}},
			{http.MethodGet, "/v1/admin/fx/rates", nil},
			{http.MethodPost, "/v1/admin/fx/markup", map[string]any{"default_spread_bps": 10}},
			{http.MethodGet, "/v1/admin/fx/markup", nil},
		}
		for _, tc := range cases {
			rec := doAs(t, r, nonAdminKey, tc.method, tc.path, tc.body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with non-admin key: status = %d, want 403 (%s)", tc.method, tc.path, rec.Code, rec.Body.String())
			}
		}
	})
}

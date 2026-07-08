package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// --- Unit-level auth tests, backed by fakeRepo (no Docker required). ---

// TestV1RequiresAPIKey checks that every /v1 route rejects a request with no
// Authorization header at all, with the exact 401 problem+json shape ADR-012
// specifies: {"status":401,"title":"Unauthorized"}, no extra "detail" leaking
// which of "missing", "unknown", or "revoked" applied.
func TestV1RequiresAPIKey(t *testing.T) {
	r := newAPIRouter(newFakeRepo())

	req := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

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

// TestV1UnknownKeyIs401 checks a syntactically plausible but never-issued key
// is rejected the same way as no key at all.
func TestV1UnknownKeyIs401(t *testing.T) {
	r := newAPIRouter(newFakeRepo())

	req := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer glk_never-issued")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
}

// TestV1ValidKeyActsAsThatTenant checks the happy path end to end through the
// real middleware: a valid bearer key lets a /v1 request through and the
// account it creates is visible when listed back with the same key.
func TestV1ValidKeyActsAsThatTenant(t *testing.T) {
	r := newAPIRouter(newFakeRepo())

	createRec := do(t, r, http.MethodPost, "/v1/accounts",
		map[string]string{"name": "Cash", "type": "asset", "currency": "USD"})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%s)", createRec.Code, createRec.Body.String())
	}

	listRec := do(t, r, http.MethodGet, "/v1/accounts", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (%s)", listRec.Code, listRec.Body.String())
	}
	var out struct {
		Accounts []AccountBody `json:"accounts"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].Name != "Cash" {
		t.Errorf("accounts = %+v, want exactly the one Cash account just created", out.Accounts)
	}
}

// TestHealthAndOpenAPIOpenWithNoKey checks the routes ADR-012 says must stay
// public keep working with no Authorization header at all, even though they
// are huma operations registered on the very same API as /v1.
func TestHealthAndOpenAPIOpenWithNoKey(t *testing.T) {
	r := newAPIRouter(newFakeRepo())

	for _, path := range []string{"/healthz", "/openapi.json", "/openapi.yaml"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 with no Authorization header (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

// --- Integration test: real Postgres-backed cross-tenant isolation. ---
//
// fakeRepo (used above) does not filter by tenant at all, so it cannot prove
// isolation; the real repository does, via tenant_id-scoped queries and the
// composite foreign keys from Week 3. This test runs against an actual
// Postgres so it exercises that real scoping, not a test double's stand-in.

var (
	sharedAuthPool *pgxpool.Pool
	authPoolErr    error
)

func TestMain(m *testing.M) {
	os.Exit(runAuthTestMain(m))
}

func runAuthTestMain(m *testing.M) int {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(120*time.Second)),
	)
	if err != nil {
		authPoolErr = fmt.Errorf("cannot start postgres (is Docker running?): %w", err)
		return m.Run()
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		authPoolErr = err
		return m.Run()
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		authPoolErr = err
		return m.Run()
	}
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		authPoolErr = err
		_ = db.Close()
		return m.Run()
	}
	if err := goose.Up(db, "migrations"); err != nil {
		authPoolErr = err
		_ = db.Close()
		return m.Run()
	}
	_ = db.Close()

	pool, err := postgres.NewPool(ctx, dsn, 10)
	if err != nil {
		authPoolErr = err
		return m.Run()
	}
	defer pool.Close()
	sharedAuthPool = pool
	return m.Run()
}

// newUUID returns a fresh random UUID string, used here as a synthetic tenant
// id: the accounts and api_keys tables both just need a uuid column value,
// not a pre-existing tenant record (go-ledger has no tenants table).
func newUUID(t *testing.T) string {
	t.Helper()
	return uuid.NewString()
}

// jsonBody marshals v to a JSON reader for building an httptest.Request body.
func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return bytes.NewReader(b)
}

func TestV1CrossTenantIsolation_Postgres(t *testing.T) {
	if authPoolErr != nil {
		t.Skipf("skipping integration test: %v", authPoolErr)
	}

	repo := postgres.NewRepository(sharedAuthPool)

	tenantA := newUUID(t)
	tenantB := newUUID(t)
	const plaintextA = "glk_cross-tenant-test-key-a"
	const plaintextB = "glk_cross-tenant-test-key-b"

	ctx := context.Background()
	if err := repo.InsertAPIKey(ctx, domain.APIKey{TenantID: tenantA, Name: "tenant A"}, domain.HashAPIKey(plaintextA)); err != nil {
		t.Fatalf("insert tenant A key: %v", err)
	}
	if err := repo.InsertAPIKey(ctx, domain.APIKey{TenantID: tenantB, Name: "tenant B"}, domain.HashAPIKey(plaintextB)); err != nil {
		t.Fatalf("insert tenant B key: %v", err)
	}

	r := chi.NewRouter()
	New(r, Deps{
		Accounts:     ledger.NewAccountService(repo),
		Transactions: ledger.NewTransactionService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), nil),
		Audit:        ledger.NewAuditService(repo),
		Auth:         auth.NewResolver(repo, time.Minute),
	})

	// Tenant A creates an account with its own key.
	createReq := httptest.NewRequest(http.MethodPost, "/v1/accounts",
		jsonBody(t, map[string]string{"name": "Tenant A Cash", "type": "asset", "currency": "USD"}))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+plaintextA)
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("tenant A create status = %d, want 201 (%s)", createRec.Code, createRec.Body.String())
	}
	var created AccountBody
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created account: %v", err)
	}

	// Tenant A can see it.
	listAReq := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	listAReq.Header.Set("Authorization", "Bearer "+plaintextA)
	listARec := httptest.NewRecorder()
	r.ServeHTTP(listARec, listAReq)
	if listARec.Code != http.StatusOK {
		t.Fatalf("tenant A list status = %d, want 200 (%s)", listARec.Code, listARec.Body.String())
	}
	var outA struct {
		Accounts []AccountBody `json:"accounts"`
	}
	if err := json.Unmarshal(listARec.Body.Bytes(), &outA); err != nil {
		t.Fatalf("decode tenant A list: %v", err)
	}
	if len(outA.Accounts) != 1 || outA.Accounts[0].ID != created.ID {
		t.Fatalf("tenant A accounts = %+v, want exactly %+v", outA.Accounts, created)
	}

	// Tenant B, a different key, must not see it: neither in the list...
	listBReq := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	listBReq.Header.Set("Authorization", "Bearer "+plaintextB)
	listBRec := httptest.NewRecorder()
	r.ServeHTTP(listBRec, listBReq)
	if listBRec.Code != http.StatusOK {
		t.Fatalf("tenant B list status = %d, want 200 (%s)", listBRec.Code, listBRec.Body.String())
	}
	var outB struct {
		Accounts []AccountBody `json:"accounts"`
	}
	if err := json.Unmarshal(listBRec.Body.Bytes(), &outB); err != nil {
		t.Fatalf("decode tenant B list: %v", err)
	}
	if len(outB.Accounts) != 0 {
		t.Errorf("tenant B sees %d account(s) created by tenant A, want 0 (cross-tenant leak): %+v",
			len(outB.Accounts), outB.Accounts)
	}

	// ...nor by direct id lookup.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+created.ID, nil)
	getReq.Header.Set("Authorization", "Bearer "+plaintextB)
	getRec := httptest.NewRecorder()
	r.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusNotFound {
		t.Errorf("tenant B get-by-id status = %d, want 404 (cross-tenant leak): %s", getRec.Code, getRec.Body.String())
	}
}

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// adminKeyPlaintext and postOnlyKeyPlaintext are provisioned by
// newAdminTestRouter against adminTestTenant: the first carries
// domain.ScopeAdmin (so it can call anything under /v1/admin/), the second
// only domain.ScopePost (so it can call the rest of the API, but not admin
// routes), proving the scope gate is enforced rather than assumed.
const (
	adminTestTenant       = "00000000-0000-0000-0000-0000000000a1"
	adminKeyPlaintext     = "glk_admin-test-admin-scoped-key" //nolint:gosec // test fixture key, not a real credential
	postOnlyKeyPlaintext  = "glk_admin-test-post-only-key"    //nolint:gosec // test fixture key, not a real credential
	adminReadOnlyKeyPlain = "glk_admin-test-read-only-key"    //nolint:gosec // test fixture key, not a real credential
)

// newAdminTestRouter wires the full API over a fresh fakeRepo and provisions
// three keys against adminTestTenant: an admin-scoped one, a post-only one,
// and a read-only one, so admin tests can prove both "an admin key can call
// /v1/admin" and "a non-admin key gets 403" without touching Postgres.
func newAdminTestRouter(t *testing.T) *fakeRepo {
	t.Helper()
	repo := newFakeRepo()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, adminTestTenant, "admin test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := repo.InsertAPIKey(ctx,
		domain.APIKey{TenantID: adminTestTenant, Name: "admin", Scopes: []domain.Scope{domain.ScopeAdmin}},
		domain.HashAPIKey(adminKeyPlaintext),
	); err != nil {
		t.Fatalf("provision admin key: %v", err)
	}
	if err := repo.InsertAPIKey(ctx,
		domain.APIKey{TenantID: adminTestTenant, Name: "post-only", Scopes: []domain.Scope{domain.ScopePost}},
		domain.HashAPIKey(postOnlyKeyPlaintext),
	); err != nil {
		t.Fatalf("provision post-only key: %v", err)
	}
	if err := repo.InsertAPIKey(ctx,
		domain.APIKey{TenantID: adminTestTenant, Name: "read-only", Scopes: []domain.Scope{domain.ScopeRead}},
		domain.HashAPIKey(adminReadOnlyKeyPlain),
	); err != nil {
		t.Fatalf("provision read-only key: %v", err)
	}
	return repo
}

// doAs issues a request authenticated as bearer, distinct from do() (which
// always authenticates as testAPIKeyPlaintext / testTenant): the admin tests
// need to run as several different keys against the same router.
func doAs(t *testing.T, r http.Handler, bearer, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestAdminIssueKeyRequiresAdminScope is the REST scope-gate proof the brief
// calls out: POST /v1/admin/keys with an admin-scoped key succeeds and the
// same call with a non-admin (post-only) key is rejected with 403, before
// the handler body (and thus admin.Service.IssueKey) ever runs.
func TestAdminIssueKeyRequiresAdminScope(t *testing.T) {
	repo := newAdminTestRouter(t)
	r := newAPIRouter(repo)

	reqBody := map[string]any{
		"tenant_id": adminTestTenant,
		"name":      "issued via rest",
		"scopes":    []string{"read", "post"},
	}

	forbidden := doAs(t, r, postOnlyKeyPlaintext, http.MethodPost, "/v1/admin/keys", reqBody)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("post-only key issuing a key: status = %d, want 403 (%s)", forbidden.Code, forbidden.Body.String())
	}

	forbiddenRead := doAs(t, r, adminReadOnlyKeyPlain, http.MethodPost, "/v1/admin/keys", reqBody)
	if forbiddenRead.Code != http.StatusForbidden {
		t.Fatalf("read-only key issuing a key: status = %d, want 403 (%s)", forbiddenRead.Code, forbiddenRead.Body.String())
	}

	allowed := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/keys", reqBody)
	if allowed.Code != http.StatusCreated {
		t.Fatalf("admin key issuing a key: status = %d, want 201 (%s)", allowed.Code, allowed.Body.String())
	}

	var issued IssuedKeyBody
	if err := json.Unmarshal(allowed.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode issued key: %v", err)
	}
	if issued.Plaintext == "" {
		t.Fatal("issued key response has an empty plaintext")
	}
	if issued.TenantID != adminTestTenant {
		t.Errorf("issued key TenantID = %q, want %q", issued.TenantID, adminTestTenant)
	}

	// The issued key's own plaintext must then authenticate a normal /v1 call.
	listRec := doAs(t, r, issued.Plaintext, http.MethodGet, "/v1/accounts", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("newly issued key list-accounts: status = %d, want 200 (%s)", listRec.Code, listRec.Body.String())
	}

	// And it must NOT be able to call the admin surface itself: it was
	// issued with read+post, not admin.
	selfEscalate := doAs(t, r, issued.Plaintext, http.MethodGet, "/v1/admin/tenants", nil)
	if selfEscalate.Code != http.StatusForbidden {
		t.Errorf("newly issued (non-admin) key listing tenants: status = %d, want 403 (%s)", selfEscalate.Code, selfEscalate.Body.String())
	}
}

// TestAdminCreateTenantAndLifecycle exercises the tenant onboarding surface
// end to end over REST: create, list, suspend, and the invalid-status 422.
func TestAdminCreateTenantAndLifecycle(t *testing.T) {
	repo := newAdminTestRouter(t)
	r := newAPIRouter(repo)

	createRec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/tenants", map[string]string{"name": "Acme"})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create tenant: status = %d, want 201 (%s)", createRec.Code, createRec.Body.String())
	}
	var created TenantBody
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created tenant: %v", err)
	}
	if created.Status != string(domain.TenantActive) {
		t.Errorf("created tenant Status = %q, want %q", created.Status, domain.TenantActive)
	}

	listRec := doAs(t, r, adminKeyPlaintext, http.MethodGet, "/v1/admin/tenants", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list tenants: status = %d, want 200 (%s)", listRec.Code, listRec.Body.String())
	}
	var listed ListTenantsOutput
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed.Body); err != nil {
		t.Fatalf("decode tenant list: %v", err)
	}
	found := false
	for _, tn := range listed.Body.Tenants {
		if tn.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Error("list-tenants did not include the just-created tenant")
	}

	suspendRec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/tenants/"+created.ID+"/status", map[string]string{"status": "suspended"})
	if suspendRec.Code != http.StatusNoContent {
		t.Fatalf("suspend tenant: status = %d, want 204 (%s)", suspendRec.Code, suspendRec.Body.String())
	}

	badStatusRec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/tenants/"+created.ID+"/status", map[string]string{"status": "pending"})
	if badStatusRec.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid status: status = %d, want 422 (%s)", badStatusRec.Code, badStatusRec.Body.String())
	}
}

// TestAdminIssueKeyIntoClosedTenantIs403 proves the tenant-active gate
// surfaces as 403 over REST, naming the reason, the same shape the auth
// resolver already uses for a suspended/closed tenant's own requests.
func TestAdminIssueKeyIntoClosedTenantIs403(t *testing.T) {
	repo := newAdminTestRouter(t)
	r := newAPIRouter(repo)

	createRec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/tenants", map[string]string{"name": "Soon Closed"})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create tenant: status = %d (%s)", createRec.Code, createRec.Body.String())
	}
	var created TenantBody
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created tenant: %v", err)
	}
	closeRec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/tenants/"+created.ID+"/status", map[string]string{"status": "closed"})
	if closeRec.Code != http.StatusNoContent {
		t.Fatalf("close tenant: status = %d (%s)", closeRec.Code, closeRec.Body.String())
	}

	issueRec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/keys", map[string]any{
		"tenant_id": created.ID,
		"name":      "into closed",
		"scopes":    []string{"read"},
	})
	if issueRec.Code != http.StatusForbidden {
		t.Errorf("issue key into closed tenant: status = %d, want 403 (%s)", issueRec.Code, issueRec.Body.String())
	}
}

// TestAdminRotateAndRevokeKey exercises the rotate/revoke/list surface over
// REST: rotate mints a working replacement while the original stays active,
// revoke then kills the original, and list-keys shows both with revoked_at
// set on the right one and no plaintext field on any entry.
func TestAdminRotateAndRevokeKey(t *testing.T) {
	repo := newAdminTestRouter(t)
	r := newAPIRouter(repo)

	issueRec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/keys", map[string]any{
		"tenant_id": adminTestTenant,
		"name":      "rotate me",
		"scopes":    []string{"read"},
	})
	if issueRec.Code != http.StatusCreated {
		t.Fatalf("issue key: status = %d (%s)", issueRec.Code, issueRec.Body.String())
	}
	var issued IssuedKeyBody
	if err := json.Unmarshal(issueRec.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode issued key: %v", err)
	}

	rotateRec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/keys/"+issued.ID+"/rotate", nil)
	if rotateRec.Code != http.StatusCreated {
		t.Fatalf("rotate key: status = %d (%s)", rotateRec.Code, rotateRec.Body.String())
	}
	var rotated IssuedKeyBody
	if err := json.Unmarshal(rotateRec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotated key: %v", err)
	}
	if rotated.Plaintext == issued.Plaintext {
		t.Fatal("rotated plaintext must differ from the original")
	}

	// Old key still works right after rotation (the overlap window).
	oldStillWorks := doAs(t, r, issued.Plaintext, http.MethodGet, "/v1/accounts", nil)
	if oldStillWorks.Code != http.StatusOK {
		t.Errorf("old key right after rotation: status = %d, want 200 (%s)", oldStillWorks.Code, oldStillWorks.Body.String())
	}

	revokeRec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/keys/"+issued.ID+"/revoke", nil)
	if revokeRec.Code != http.StatusNoContent {
		t.Fatalf("revoke key: status = %d (%s)", revokeRec.Code, revokeRec.Body.String())
	}

	// A fresh router (and thus a fresh auth.Resolver with no warm cache
	// entry) over the same repo: r's own resolver already cached the old
	// key as good from oldStillWorks above, and Resolve does not invalidate
	// a cached hit on a later revocation within its TTL (see
	// auth.Resolver's doc comment) - the same real-world lag an operator
	// sees for up to AUTH_CACHE_TTL after revoking a key. A resolver with no
	// warm entry hits the repository directly and sees the revocation
	// immediately.
	r2 := newAPIRouter(repo)
	oldNowFails := doAs(t, r2, issued.Plaintext, http.MethodGet, "/v1/accounts", nil)
	if oldNowFails.Code != http.StatusUnauthorized {
		t.Errorf("old key after explicit revoke: status = %d, want 401 (%s)", oldNowFails.Code, oldNowFails.Body.String())
	}

	listRec := doAs(t, r, adminKeyPlaintext, http.MethodGet, "/v1/admin/keys?tenant_id="+adminTestTenant, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list keys: status = %d (%s)", listRec.Code, listRec.Body.String())
	}
	if !json.Valid(listRec.Body.Bytes()) {
		t.Fatal("list keys response is not valid JSON")
	}
	var listed ListKeysOutput
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed.Body); err != nil {
		t.Fatalf("decode key list: %v", err)
	}
	var sawRevoked, sawRotated bool
	for _, k := range listed.Body.Keys {
		if k.ID == issued.ID {
			sawRevoked = true
			if k.RevokedAt == nil {
				t.Error("original key in list-keys has RevokedAt = nil, want a timestamp")
			}
		}
		if k.ID == rotated.ID {
			sawRotated = true
			if k.RevokedAt != nil {
				t.Errorf("rotated key in list-keys has RevokedAt = %v, want nil", *k.RevokedAt)
			}
		}
	}
	if !sawRevoked || !sawRotated {
		t.Errorf("list-keys missing entries: sawRevoked=%v sawRotated=%v, body=%s", sawRevoked, sawRotated, listRec.Body.String())
	}

	// The raw response never carries a "plaintext" field at all (KeyBody has
	// no such field): confirm none of the marshaled entries decode one, as a
	// belt-and-suspenders check on top of the type system.
	var raw struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw key list: %v", err)
	}
	for _, k := range raw.Keys {
		if _, ok := k["plaintext"]; ok {
			t.Errorf("list-keys entry unexpectedly has a plaintext field: %v", k)
		}
	}
}

// TestAdminIssueKeyInvalidScopesIs422 proves an empty scopes list is
// rejected over REST with 422.
func TestAdminIssueKeyInvalidScopesIs422(t *testing.T) {
	repo := newAdminTestRouter(t)
	r := newAPIRouter(repo)

	rec := doAs(t, r, adminKeyPlaintext, http.MethodPost, "/v1/admin/keys", map[string]any{
		"tenant_id": adminTestTenant,
		"name":      "no scopes",
		"scopes":    []string{},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("issue key with empty scopes: status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// verifyAuditBody mirrors VerifyAuditOutput.Body for decoding responses in
// these tests.
type verifyAuditBody struct {
	Valid        bool              `json:"valid"`
	Checked      int               `json:"checked"`
	FirstBreakID *string           `json:"first_break_id"`
	Pending      int               `json:"pending"`
	Head         *verifyHeadBody   `json:"head"`
	Anchor       *verifyAnchorBody `json:"anchor"`
}

type verifyHeadBody struct {
	ChainSeq int64  `json:"chain_seq"`
	RowHash  string `json:"row_hash"`
}

type verifyAnchorBody struct {
	ChainSeq int64  `json:"chain_seq"`
	RowHash  string `json:"row_hash"`
}

// TestVerifyAuditChain_EmptyChain checks a fresh tenant with no audit rows
// verifies as valid with nothing checked, and that the shape is exactly what
// ADR-012 specifies: valid, checked, and a null (not missing, not "") first_break_id.
func TestVerifyAuditChain_EmptyChain(t *testing.T) {
	router := newAPIRouter(newFakeRepo())

	rec := getJSON(t, router, "/v1/audit/verify")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var out verifyAuditBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !out.Valid {
		t.Errorf("valid = false, want true for an empty chain")
	}
	if out.Checked != 0 {
		t.Errorf("checked = %d, want 0", out.Checked)
	}
	if out.FirstBreakID != nil {
		t.Errorf("first_break_id = %v, want null", *out.FirstBreakID)
	}
	if out.Head != nil {
		t.Errorf("head = %+v, want null for an empty chain", out.Head)
	}
	if out.Anchor != nil {
		t.Errorf("anchor = %+v, want null (fakeRepo never records one)", out.Anchor)
	}
}

// TestVerifyAuditChain_ValidChain posts a couple of real transactions through
// the API (each one appends an audit row via the real hash-chaining fakeRepo
// path) and checks the endpoint reports a valid chain with every row checked.
func TestVerifyAuditChain_ValidChain(t *testing.T) {
	repo := newFakeRepo()
	a := &domain.Account{Name: "A", Type: domain.Asset, Currency: "USD"}
	b := &domain.Account{Name: "B", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(context.Background(), "t", a); err != nil {
		t.Fatalf("create account a: %v", err)
	}
	if err := repo.CreateAccount(context.Background(), "t", b); err != nil {
		t.Fatalf("create account b: %v", err)
	}
	router := newAPIRouter(repo)

	body := `{"currency":"USD","postings":[` +
		`{"account_id":"` + a.ID + `","amount":100},` +
		`{"account_id":"` + b.ID + `","amount":-100}]}`
	for i, key := range []string{"verify-chain-1", "verify-chain-2"} {
		rec := postJSON(t, router, "/v1/transactions", body, map[string]string{"Idempotency-Key": key})
		if rec.Code != http.StatusCreated {
			t.Fatalf("post %d status = %d, want 201 (%s)", i, rec.Code, rec.Body.String())
		}
	}

	rec := getJSON(t, router, "/v1/audit/verify")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out verifyAuditBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !out.Valid {
		t.Errorf("valid = false, want true")
	}
	if out.Checked != 2 {
		t.Errorf("checked = %d, want 2", out.Checked)
	}
	if out.FirstBreakID != nil {
		t.Errorf("first_break_id = %v, want null on a valid chain", *out.FirstBreakID)
	}
	if out.Head == nil || out.Head.ChainSeq != 2 || out.Head.RowHash == "" {
		t.Errorf("head = %+v, want a non-empty head at chain_seq 2", out.Head)
	}
	if out.Anchor != nil {
		t.Errorf("anchor = %+v, want null (fakeRepo never records one, Task 5.3)", out.Anchor)
	}
}

// TestVerifyAuditChain_Fast_FallsBackWithNoAnchor checks the fast=true query
// flag (Task 5.3, the from-anchor path) still verifies the whole chain when
// the tenant has no anchor recorded yet: fakeRepo never records one, so this
// exercises VerifyFromLatestAnchor's fallback to a full verify through the
// live HTTP surface, not just the ledger package's own unit tests.
func TestVerifyAuditChain_Fast_FallsBackWithNoAnchor(t *testing.T) {
	repo := newFakeRepo()
	a := &domain.Account{Name: "A", Type: domain.Asset, Currency: "USD"}
	b := &domain.Account{Name: "B", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(context.Background(), "t", a); err != nil {
		t.Fatalf("create account a: %v", err)
	}
	if err := repo.CreateAccount(context.Background(), "t", b); err != nil {
		t.Fatalf("create account b: %v", err)
	}
	router := newAPIRouter(repo)

	body := `{"currency":"USD","postings":[` +
		`{"account_id":"` + a.ID + `","amount":100},` +
		`{"account_id":"` + b.ID + `","amount":-100}]}`
	for i, key := range []string{"verify-fast-1", "verify-fast-2"} {
		rec := postJSON(t, router, "/v1/transactions", body, map[string]string{"Idempotency-Key": key})
		if rec.Code != http.StatusCreated {
			t.Fatalf("post %d status = %d, want 201 (%s)", i, rec.Code, rec.Body.String())
		}
	}

	rec := getJSON(t, router, "/v1/audit/verify?fast=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out verifyAuditBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !out.Valid {
		t.Errorf("valid = false, want true")
	}
	if out.Checked != 2 {
		t.Errorf("checked = %d, want 2 (no anchor recorded, so fast=true falls back to a full verify)", out.Checked)
	}
	if out.FirstBreakID != nil {
		t.Errorf("first_break_id = %v, want null on a valid chain", *out.FirstBreakID)
	}
}

// TestVerifyAuditChain_NoKeyIs401 checks the route is a normal authenticated
// /v1 route: no Authorization header at all is rejected the same way every
// other /v1 route is (ADR-012).
func TestVerifyAuditChain_NoKeyIs401(t *testing.T) {
	router := newAPIRouter(newFakeRepo())

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/verify", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
}

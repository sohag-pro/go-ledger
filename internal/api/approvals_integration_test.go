package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// Three keys, three scopes, one tenant (testTenant, the same fixture the rest
// of this package's handler tests use): a post-only key creates transactions
// and cancels its own pendings, an approve-scoped key decides them, and an
// admin key satisfies any scope (ScopeAdmin is a documented superset, see
// domain.APIKey.HasScope).
const (
	approvalsPostKeyPlaintext    = "glk_approvals-test-post-key"    //nolint:gosec // test fixture key, not a real credential
	approvalsApproveKeyPlaintext = "glk_approvals-test-approve-key" //nolint:gosec // test fixture key, not a real credential
)

// approvalsThresholdUSD is the low USD threshold every test router below
// gates on: chosen well under the fixture transfer amounts used throughout
// this file, so every transfer they post trips the gate deterministically.
const approvalsThresholdUSD = int64(500)

// newApprovalsRouter wires the API with the approval gate enabled (ADR-025)
// and a low USD threshold, over a fresh fake repo, and returns the router. It
// mirrors newAPIRouterWithOptions but additionally provisions
// approvalsPostKeyPlaintext (scopes read, post) and approvalsApproveKeyPlaintext
// (scopes read, approve) against testTenant, and wires ledger.WithApproval plus
// deps.Approvals, neither of which newAPIRouterWithOptions does.
func newApprovalsRouter(t *testing.T, requireDifferentActor bool) chi.Router {
	t.Helper()
	repo := newFakeRepo()

	keys := map[string][]domain.Scope{
		approvalsPostKeyPlaintext:    {domain.ScopeRead, domain.ScopePost},
		approvalsApproveKeyPlaintext: {domain.ScopeRead, domain.ScopeApprove},
	}
	for plaintext, scopes := range keys {
		if err := repo.InsertAPIKey(context.Background(),
			domain.APIKey{TenantID: testTenant, Name: plaintext, Scopes: scopes},
			domain.HashAPIKey(plaintext),
		); err != nil {
			t.Fatalf("provision key: %v", err)
		}
	}

	approvalCfg := ledger.ApprovalConfig{
		Enabled:               true,
		Thresholds:            map[string]int64{"USD": approvalsThresholdUSD},
		RequireDifferentActor: requireDifferentActor,
		TTL:                   72 * time.Hour,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	transactions := ledger.NewTransactionService(repo, logger, nil, ledger.WithApproval(approvalCfg))
	approvals := ledger.NewApprovalService(repo, transactions, approvalCfg, logger)

	r := chi.NewRouter()
	New(r, Deps{
		Accounts:     ledger.NewAccountService(repo),
		Transactions: transactions,
		Audit:        ledger.NewAuditService(repo),
		Admin:        admin.NewService(repo),
		Reports:      ledger.NewReportService(repo),
		Disputes:     ledger.NewDisputeService(repo, transactions),
		Approvals:    approvals,
		Auth:         auth.NewResolver(repo, time.Minute),
	})
	return r
}

// doAsHeaders is admin_test.go's doAs plus extra headers (e.g.
// Idempotency-Key), needed here for postOverThreshold's transaction posts.
// Every other call in this file that needs no extra header uses the shared
// doAs directly.
func doAsHeaders(t *testing.T, r chi.Router, apiKey, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
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
	req.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// createAccountAs always creates through approvalsPostKeyPlaintext in this
// file's tests today (account creation is not itself gated), but takes
// apiKey explicitly rather than hardcoding it, matching postJSON's own
// general-purpose-helper convention elsewhere in this package.
//
//nolint:unparam // apiKey is a general test-helper parameter; only one literal is in use today.
func createAccountAs(t *testing.T, r chi.Router, apiKey, name, typ string) string {
	t.Helper()
	rec := doAs(t, r, apiKey, http.MethodPost, "/v1/accounts",
		map[string]string{"name": name, "type": typ, "currency": "USD"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create account %s: status %d (%s)", name, rec.Code, rec.Body.String())
	}
	var out AccountBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode account: %v", err)
	}
	return out.ID
}

// heldResponse decodes the shape a create/convert/reverse endpoint returns on
// 202: only the pending field is populated, so this ignores every other
// field the underlying output types also carry (TransactionBody's fields,
// left zero on this path).
type heldResponse struct {
	Pending *PendingBody `json:"pending"`
}

// postOverThreshold posts a two-leg USD transfer of amount (which callers
// pass over approvalsThresholdUSD) from debit to credit, using
// approvalsPostKeyPlaintext, and asserts it comes back 202 with a pending
// body whose kind is "post" and status "pending". Returns the pending's id.
func postOverThreshold(t *testing.T, r chi.Router, debit, credit string, amount int64, idemKey string) string {
	t.Helper()
	rec := doAsHeaders(t, r, approvalsPostKeyPlaintext, http.MethodPost, "/v1/transactions", map[string]any{
		"currency": "USD",
		"postings": []map[string]any{
			{"account_id": debit, "amount": amount},
			{"account_id": credit, "amount": -amount},
		},
	}, map[string]string{"Idempotency-Key": idemKey})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post over-threshold transaction: status %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	var held heldResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode held response: %v", err)
	}
	if held.Pending == nil {
		t.Fatal("held response has no pending body")
	}
	if held.Pending.ID == "" {
		t.Error("pending.id is empty")
	}
	if held.Pending.Kind != string(domain.PendingKindPost) {
		t.Errorf("pending.kind = %q, want %q", held.Pending.Kind, domain.PendingKindPost)
	}
	if held.Pending.Status != string(domain.PendingStatusPending) {
		t.Errorf("pending.status = %q, want %q", held.Pending.Status, domain.PendingStatusPending)
	}
	if held.Pending.ThresholdCurrency != "USD" {
		t.Errorf("pending.threshold_currency = %q, want USD", held.Pending.ThresholdCurrency)
	}
	return held.Pending.ID
}

// TestApprovals_HoldListApproveIdempotent covers the core happy path: an
// over-threshold POST /v1/transactions is held (202) instead of posted, shows
// up in GET /v1/pending, and an approve-scoped key's POST .../approve posts
// it for real (200 + the transaction) and is idempotent on a second call.
func TestApprovals_HoldListApproveIdempotent(t *testing.T) {
	r := newApprovalsRouter(t, false)
	cash := createAccountAs(t, r, approvalsPostKeyPlaintext, "Cash", "asset")
	revenue := createAccountAs(t, r, approvalsPostKeyPlaintext, "Revenue", "income")

	pendingID := postOverThreshold(t, r, cash, revenue, 1000, "approvals-hold-1")

	// The pending shows up in the list, status pending.
	listRec := doAs(t, r, approvalsApproveKeyPlaintext, http.MethodGet, "/v1/pending", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list pending: status %d (%s)", listRec.Code, listRec.Body.String())
	}
	var list PendingListOutput
	if err := json.Unmarshal(listRec.Body.Bytes(), &list.Body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, p := range list.Body.Pending {
		if p.ID == pendingID {
			found = true
			if p.Status != string(domain.PendingStatusPending) {
				t.Errorf("listed pending status = %q, want pending", p.Status)
			}
		}
	}
	if !found {
		t.Fatalf("pending %s not found in list %+v", pendingID, list.Body.Pending)
	}

	// GET the single pending directly too.
	getRec := doAs(t, r, approvalsApproveKeyPlaintext, http.MethodGet, "/v1/pending/"+pendingID, nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get pending: status %d (%s)", getRec.Code, getRec.Body.String())
	}

	// Approve with the approve-scoped key: 200 and the posted transaction.
	approveRec := doAs(t, r, approvalsApproveKeyPlaintext, http.MethodPost, "/v1/pending/"+pendingID+"/approve", nil)
	if approveRec.Code != http.StatusOK {
		t.Fatalf("approve: status %d, want 200 (%s)", approveRec.Code, approveRec.Body.String())
	}
	var posted TransactionBody
	if err := json.Unmarshal(approveRec.Body.Bytes(), &posted); err != nil {
		t.Fatalf("decode approved transaction: %v", err)
	}
	if posted.ID == "" {
		t.Fatal("approved transaction id is empty")
	}
	if len(posted.Postings) != 2 {
		t.Errorf("approved transaction has %d postings, want 2", len(posted.Postings))
	}

	// The transaction is really posted: fetchable, and the balance moved.
	balRec := doAs(t, r, approvalsPostKeyPlaintext, http.MethodGet, "/v1/accounts/"+cash+"/balance", nil)
	var bal BalanceOutput
	if err := json.Unmarshal(balRec.Body.Bytes(), &bal.Body); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	if bal.Body.Amount != 1000 {
		t.Errorf("cash balance after approval = %d, want 1000", bal.Body.Amount)
	}

	// Approving again is idempotent: same transaction id, still 200, and the
	// balance does not move a second time.
	approveAgainRec := doAs(t, r, approvalsApproveKeyPlaintext, http.MethodPost, "/v1/pending/"+pendingID+"/approve", nil)
	if approveAgainRec.Code != http.StatusOK {
		t.Fatalf("second approve: status %d, want 200 (%s)", approveAgainRec.Code, approveAgainRec.Body.String())
	}
	var postedAgain TransactionBody
	if err := json.Unmarshal(approveAgainRec.Body.Bytes(), &postedAgain); err != nil {
		t.Fatalf("decode second approve response: %v", err)
	}
	if postedAgain.ID != posted.ID {
		t.Errorf("second approve posted a different transaction: %q, want the same %q", postedAgain.ID, posted.ID)
	}
	balRec2 := doAs(t, r, approvalsPostKeyPlaintext, http.MethodGet, "/v1/accounts/"+cash+"/balance", nil)
	var bal2 BalanceOutput
	if err := json.Unmarshal(balRec2.Body.Bytes(), &bal2.Body); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	if bal2.Body.Amount != 1000 {
		t.Errorf("cash balance after second approve = %d, want 1000 (unchanged)", bal2.Body.Amount)
	}
}

// TestApprovals_Reject checks that an approve-scoped key can reject a
// pending, that no money ever moves for it, and that the pending's status
// lands on "rejected".
func TestApprovals_Reject(t *testing.T) {
	r := newApprovalsRouter(t, false)
	cash := createAccountAs(t, r, approvalsPostKeyPlaintext, "Cash", "asset")
	revenue := createAccountAs(t, r, approvalsPostKeyPlaintext, "Revenue", "income")

	pendingID := postOverThreshold(t, r, cash, revenue, 900, "approvals-reject-1")

	rejectRec := doAs(t, r, approvalsApproveKeyPlaintext, http.MethodPost, "/v1/pending/"+pendingID+"/reject",
		map[string]any{"reason": "looks wrong"})
	if rejectRec.Code != http.StatusOK {
		t.Fatalf("reject: status %d, want 200 (%s)", rejectRec.Code, rejectRec.Body.String())
	}
	var rejected PendingBody
	if err := json.Unmarshal(rejectRec.Body.Bytes(), &rejected); err != nil {
		t.Fatalf("decode rejected pending: %v", err)
	}
	if rejected.Status != string(domain.PendingStatusRejected) {
		t.Errorf("status = %q, want rejected", rejected.Status)
	}
	if rejected.Reason == nil || *rejected.Reason != "looks wrong" {
		t.Errorf("reason = %v, want %q", rejected.Reason, "looks wrong")
	}

	balRec := doAs(t, r, approvalsPostKeyPlaintext, http.MethodGet, "/v1/accounts/"+cash+"/balance", nil)
	var bal BalanceOutput
	if err := json.Unmarshal(balRec.Body.Bytes(), &bal.Body); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	if bal.Body.Amount != 0 {
		t.Errorf("cash balance after reject = %d, want 0 (no money moved)", bal.Body.Amount)
	}

	// Rejecting again is a conflict, not a silent no-op.
	rejectAgainRec := doAs(t, r, approvalsApproveKeyPlaintext, http.MethodPost, "/v1/pending/"+pendingID+"/reject", map[string]any{})
	if rejectAgainRec.Code != http.StatusConflict {
		t.Errorf("second reject: status %d, want 409 (%s)", rejectAgainRec.Code, rejectAgainRec.Body.String())
	}
}

// TestApprovals_Cancel checks that the creator's own (post-scoped) key can
// cancel its own pending, and that no money ever moves for it.
func TestApprovals_Cancel(t *testing.T) {
	r := newApprovalsRouter(t, false)
	cash := createAccountAs(t, r, approvalsPostKeyPlaintext, "Cash", "asset")
	revenue := createAccountAs(t, r, approvalsPostKeyPlaintext, "Revenue", "income")

	pendingID := postOverThreshold(t, r, cash, revenue, 750, "approvals-cancel-1")

	cancelRec := doAs(t, r, approvalsPostKeyPlaintext, http.MethodPost, "/v1/pending/"+pendingID+"/cancel", nil)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel: status %d, want 200 (%s)", cancelRec.Code, cancelRec.Body.String())
	}
	var cancelled PendingBody
	if err := json.Unmarshal(cancelRec.Body.Bytes(), &cancelled); err != nil {
		t.Fatalf("decode cancelled pending: %v", err)
	}
	if cancelled.Status != string(domain.PendingStatusCancelled) {
		t.Errorf("status = %q, want cancelled", cancelled.Status)
	}

	balRec := doAs(t, r, approvalsPostKeyPlaintext, http.MethodGet, "/v1/accounts/"+cash+"/balance", nil)
	var bal BalanceOutput
	if err := json.Unmarshal(balRec.Body.Bytes(), &bal.Body); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	if bal.Body.Amount != 0 {
		t.Errorf("cash balance after cancel = %d, want 0 (no money moved)", bal.Body.Amount)
	}
}

// TestApprovals_PostOnlyKeyForbiddenFromApprove checks that a post-only key
// (no approve scope) gets 403 on POST /v1/pending/{id}/approve, per
// auth.RequiredHTTPScope's routing of that path to domain.ScopeApprove.
func TestApprovals_PostOnlyKeyForbiddenFromApprove(t *testing.T) {
	r := newApprovalsRouter(t, false)
	cash := createAccountAs(t, r, approvalsPostKeyPlaintext, "Cash", "asset")
	revenue := createAccountAs(t, r, approvalsPostKeyPlaintext, "Revenue", "income")

	pendingID := postOverThreshold(t, r, cash, revenue, 600, "approvals-forbidden-1")

	rec := doAs(t, r, approvalsPostKeyPlaintext, http.MethodPost, "/v1/pending/"+pendingID+"/approve", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("approve with post-only key: status %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

// TestApprovals_RequireDifferentActorBlocksSelfApproval checks four-eyes
// (ApprovalConfig.RequireDifferentActor, ADR-025): go-ledger's actor
// granularity is per tenant, not per API key (every audit event's Actor
// field is stamped as the tenant id, see internal/ledger/service.go,
// convert.go, reverse.go, and approval_events.go), and a pending's CreatedBy
// is likewise stamped as the tenant at hold time. The HTTP layer passes that
// same tenant as the approve actor (see registerApprovals' own comment), so
// with RequireDifferentActor on, approving a pending created by that SAME
// tenant always trips ErrCannotApproveOwn, regardless of which of the
// tenant's own keys calls approve.
func TestApprovals_RequireDifferentActorBlocksSelfApproval(t *testing.T) {
	r := newApprovalsRouter(t, true)
	cash := createAccountAs(t, r, approvalsPostKeyPlaintext, "Cash", "asset")
	revenue := createAccountAs(t, r, approvalsPostKeyPlaintext, "Revenue", "income")

	pendingID := postOverThreshold(t, r, cash, revenue, 800, "approvals-four-eyes-1")

	rec := doAs(t, r, approvalsApproveKeyPlaintext, http.MethodPost, "/v1/pending/"+pendingID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("approve own tenant's pending with require-different-actor: status %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

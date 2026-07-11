package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// postTxn posts a balanced two-leg transaction through the real HTTP handler
// and returns its decoded body, failing the test on anything but 201. It
// exists so dispute tests do not have to repeat the same fixture-posting
// boilerplate reverse_test.go's own tests already inline.
func postTxn(t *testing.T, r chi.Router, debit, credit string, amount int64, idemKey string) TransactionBody {
	t.Helper()
	rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
		"currency": "USD",
		"postings": []map[string]any{
			{"account_id": debit, "amount": amount},
			{"account_id": credit, "amount": -amount},
		},
	}, map[string]string{"Idempotency-Key": idemKey})
	if rec.Code != http.StatusCreated {
		t.Fatalf("post transaction: status %d (%s)", rec.Code, rec.Body.String())
	}
	var body TransactionBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode transaction: %v", err)
	}
	return body
}

// TestOpenDispute_HappyPath opens a dispute on a real transaction and checks
// the response shape: status open, no resolution transaction, no
// resolved_at.
func TestOpenDispute_HappyPath(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	rev := createAccount(t, r, "Revenue", "income")
	txn := postTxn(t, r, cash, rev, 10000, "dispute-open-1")

	rec := do(t, r, http.MethodPost, "/v1/disputes", map[string]any{
		"transaction_id": txn.ID,
		"reason":         "customer claims non-delivery",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var d DisputeBody
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode dispute: %v", err)
	}
	if d.TransactionID != txn.ID {
		t.Errorf("transaction_id = %q, want %q", d.TransactionID, txn.ID)
	}
	if d.Status != "open" {
		t.Errorf("status = %q, want %q", d.Status, "open")
	}
	if d.ResolutionTransactionID != nil {
		t.Errorf("resolution_transaction_id = %v, want nil", d.ResolutionTransactionID)
	}
	if d.ResolvedAt != nil {
		t.Errorf("resolved_at = %v, want nil", d.ResolvedAt)
	}
}

// TestOpenDispute_UnknownTransaction404 checks that opening a dispute
// against a transaction id that names nothing at all is rejected.
func TestOpenDispute_UnknownTransaction404(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	rec := do(t, r, http.MethodPost, "/v1/disputes", map[string]any{
		"transaction_id": uuid.NewString(),
		"reason":         "test",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

// TestResolveDispute_Reverse checks that resolving a dispute with
// action=reverse posts a real reversal (visible via GET
// /v1/transactions/{id}), links resolution_transaction_id to it, and moves
// status to resolved_reversed.
func TestResolveDispute_Reverse(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	rev := createAccount(t, r, "Revenue", "income")
	txn := postTxn(t, r, cash, rev, 5000, "dispute-resolve-reverse-1")

	openRec := do(t, r, http.MethodPost, "/v1/disputes", map[string]any{
		"transaction_id": txn.ID,
		"reason":         "chargeback",
	})
	if openRec.Code != http.StatusCreated {
		t.Fatalf("open dispute: status %d (%s)", openRec.Code, openRec.Body.String())
	}
	var opened DisputeBody
	if err := json.Unmarshal(openRec.Body.Bytes(), &opened); err != nil {
		t.Fatalf("decode opened dispute: %v", err)
	}

	resolveRec := do(t, r, http.MethodPost, "/v1/disputes/"+opened.ID+"/resolve", map[string]any{
		"action": "reverse",
	})
	if resolveRec.Code != http.StatusOK {
		t.Fatalf("resolve dispute: status %d, want 200 (%s)", resolveRec.Code, resolveRec.Body.String())
	}
	var resolved DisputeBody
	if err := json.Unmarshal(resolveRec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolved dispute: %v", err)
	}
	if resolved.Status != "resolved_reversed" {
		t.Errorf("status = %q, want %q", resolved.Status, "resolved_reversed")
	}
	if resolved.ResolutionTransactionID == nil {
		t.Fatal("resolution_transaction_id = nil, want the reversal's id")
	}
	if resolved.ResolvedAt == nil {
		t.Error("resolved_at = nil, want a timestamp")
	}
	if *resolved.ResolutionTransactionID == txn.ID {
		t.Error("resolution_transaction_id equals the original transaction id, want the reversal's distinct id")
	}

	// The reversal must be a real, independently retrievable transaction
	// that reverses the original: the resolve action went through the
	// normal posting path, not a raw insert.
	getRec := do(t, r, http.MethodGet, "/v1/transactions/"+*resolved.ResolutionTransactionID, nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get reversal: status %d (%s)", getRec.Code, getRec.Body.String())
	}
	var reversal TransactionBody
	if err := json.Unmarshal(getRec.Body.Bytes(), &reversal); err != nil {
		t.Fatalf("decode reversal: %v", err)
	}
	if reversal.ReversesTransactionID == nil || *reversal.ReversesTransactionID != txn.ID {
		t.Errorf("reversal reverses_transaction_id = %v, want pointer to %q", reversal.ReversesTransactionID, txn.ID)
	}

	// Balances are back to zero: the reversal's negated legs cancel the
	// original's.
	balRec := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/balance", nil)
	var bal BalanceOutput
	if err := json.Unmarshal(balRec.Body.Bytes(), &bal.Body); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	if bal.Body.Amount != 0 {
		t.Errorf("cash balance after reversal = %d, want 0", bal.Body.Amount)
	}
}

// TestResolveDispute_Reject checks that resolving with action=reject moves
// no money and lands status resolved_rejected.
func TestResolveDispute_Reject(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	rev := createAccount(t, r, "Revenue", "income")
	txn := postTxn(t, r, cash, rev, 5000, "dispute-resolve-reject-1")

	openRec := do(t, r, http.MethodPost, "/v1/disputes", map[string]any{
		"transaction_id": txn.ID,
		"reason":         "chargeback",
	})
	var opened DisputeBody
	if err := json.Unmarshal(openRec.Body.Bytes(), &opened); err != nil {
		t.Fatalf("decode opened dispute: %v", err)
	}

	resolveRec := do(t, r, http.MethodPost, "/v1/disputes/"+opened.ID+"/resolve", map[string]any{
		"action": "reject",
	})
	if resolveRec.Code != http.StatusOK {
		t.Fatalf("resolve dispute: status %d, want 200 (%s)", resolveRec.Code, resolveRec.Body.String())
	}
	var resolved DisputeBody
	if err := json.Unmarshal(resolveRec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolved dispute: %v", err)
	}
	if resolved.Status != "resolved_rejected" {
		t.Errorf("status = %q, want %q", resolved.Status, "resolved_rejected")
	}
	if resolved.ResolutionTransactionID != nil {
		t.Errorf("resolution_transaction_id = %v, want nil (no money moved)", resolved.ResolutionTransactionID)
	}

	balRec := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/balance", nil)
	var bal BalanceOutput
	if err := json.Unmarshal(balRec.Body.Bytes(), &bal.Body); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	if bal.Body.Amount != 5000 {
		t.Errorf("cash balance after reject = %d, want 5000 (unchanged)", bal.Body.Amount)
	}
}

// TestResolveDispute_TwiceIsRejected checks that resolving an
// already-resolved dispute returns 409, not a silent overwrite.
func TestResolveDispute_TwiceIsRejected(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	rev := createAccount(t, r, "Revenue", "income")
	txn := postTxn(t, r, cash, rev, 1000, "dispute-resolve-twice-1")

	openRec := do(t, r, http.MethodPost, "/v1/disputes", map[string]any{
		"transaction_id": txn.ID,
		"reason":         "chargeback",
	})
	var opened DisputeBody
	if err := json.Unmarshal(openRec.Body.Bytes(), &opened); err != nil {
		t.Fatalf("decode opened dispute: %v", err)
	}

	first := do(t, r, http.MethodPost, "/v1/disputes/"+opened.ID+"/resolve", map[string]any{"action": "reject"})
	if first.Code != http.StatusOK {
		t.Fatalf("first resolve: status %d, want 200 (%s)", first.Code, first.Body.String())
	}
	second := do(t, r, http.MethodPost, "/v1/disputes/"+opened.ID+"/resolve", map[string]any{"action": "reverse"})
	if second.Code != http.StatusConflict {
		t.Errorf("second resolve: status %d, want 409 (%s)", second.Code, second.Body.String())
	}
}

// TestListDisputes_FilterByStatus checks the status query filter and that
// GET /v1/disputes/{id} round-trips a single dispute.
func TestListDisputes_FilterByStatus(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	rev := createAccount(t, r, "Revenue", "income")
	txnA := postTxn(t, r, cash, rev, 1000, "dispute-list-1")
	txnB := postTxn(t, r, cash, rev, 2000, "dispute-list-2")

	openA := do(t, r, http.MethodPost, "/v1/disputes", map[string]any{"transaction_id": txnA.ID, "reason": "a"})
	var dA DisputeBody
	_ = json.Unmarshal(openA.Body.Bytes(), &dA)
	openB := do(t, r, http.MethodPost, "/v1/disputes", map[string]any{"transaction_id": txnB.ID, "reason": "b"})
	var dB DisputeBody
	_ = json.Unmarshal(openB.Body.Bytes(), &dB)

	resolveRec := do(t, r, http.MethodPost, "/v1/disputes/"+dB.ID+"/resolve", map[string]any{"action": "reject"})
	if resolveRec.Code != http.StatusOK {
		t.Fatalf("resolve dispute B: status %d (%s)", resolveRec.Code, resolveRec.Body.String())
	}

	openList := do(t, r, http.MethodGet, "/v1/disputes?status=open", nil)
	var openOut ListDisputesOutput
	if err := json.Unmarshal(openList.Body.Bytes(), &openOut.Body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(openOut.Body.Disputes) != 1 || openOut.Body.Disputes[0].ID != dA.ID {
		t.Errorf("status=open list = %+v, want exactly dispute %q", openOut.Body.Disputes, dA.ID)
	}

	getRec := do(t, r, http.MethodGet, "/v1/disputes/"+dA.ID, nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get dispute: status %d (%s)", getRec.Code, getRec.Body.String())
	}

	badStatus := do(t, r, http.MethodGet, "/v1/disputes?status=bogus", nil)
	if badStatus.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=bogus: status %d, want 422 (%s)", badStatus.Code, badStatus.Body.String())
	}
}

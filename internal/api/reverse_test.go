package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestReverseTransaction covers POST /v1/transactions/{id}/reverse end to
// end against the fake repo: a first reversal returns 201 with the negated
// legs and Already-Reversed: false, a second call for the same original
// returns the SAME reversal with Already-Reversed: true, reversing that
// reversal is rejected 422, and reversing an unknown id is 404.
func TestReverseTransaction(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	rev := createAccount(t, r, "Revenue", "income")

	postRec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
		"currency": "USD",
		"postings": []map[string]any{
			{"account_id": cash, "amount": 10000, "description": "sale"},
			{"account_id": rev, "amount": -10000},
		},
	}, map[string]string{"Idempotency-Key": "reverse-handler-post-1"})
	if postRec.Code != http.StatusCreated {
		t.Fatalf("post original: status %d (%s)", postRec.Code, postRec.Body.String())
	}
	var original TransactionBody
	if err := json.Unmarshal(postRec.Body.Bytes(), &original); err != nil {
		t.Fatalf("decode original: %v", err)
	}

	t.Run("first reversal 201", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/transactions/"+original.ID+"/reverse", nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Already-Reversed"); got != "false" {
			t.Errorf("Already-Reversed = %q, want %q", got, "false")
		}
		var body TransactionBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode reversal: %v", err)
		}
		if body.ID == original.ID {
			t.Error("reversal id equals the original id, want a distinct new transaction")
		}
		if body.ReversesTransactionID == nil || *body.ReversesTransactionID != original.ID {
			t.Errorf("reverses_transaction_id = %v, want pointer to %q", body.ReversesTransactionID, original.ID)
		}
		if len(body.Postings) != 2 {
			t.Fatalf("postings = %d, want 2", len(body.Postings))
		}
		for _, p := range body.Postings {
			switch p.AccountID {
			case cash:
				if p.Amount != -10000 {
					t.Errorf("cash posting amount = %d, want -10000", p.Amount)
				}
			case rev:
				if p.Amount != 10000 {
					t.Errorf("revenue posting amount = %d, want 10000", p.Amount)
				}
			}
		}
	})

	var firstReversalID string
	t.Run("second reversal 201 already-reversed true, same id", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/transactions/"+original.ID+"/reverse", nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Already-Reversed"); got != "true" {
			t.Errorf("Already-Reversed = %q, want %q", got, "true")
		}
		var body TransactionBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode reversal: %v", err)
		}
		firstReversalID = body.ID
	})

	t.Run("reversing a reversal 422", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/transactions/"+firstReversalID+"/reverse", nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown id 404", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/transactions/"+uuid.NewString()+"/reverse", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
	})
}

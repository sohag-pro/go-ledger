package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestStatementExport_CSV checks the header, row count, and content type for
// the default (csv) format.
func TestStatementExport_CSV(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	other := createAccount(t, r, "Other", "asset")

	for i := 0; i < 3; i++ {
		rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
			"currency": "USD",
			"postings": []map[string]any{
				{"account_id": cash, "amount": 100, "description": "deposit"},
				{"account_id": other, "amount": -100},
			},
		}, map[string]string{"Idempotency-Key": fmt.Sprintf("statement-export-csv-%d", i)})
		if rec.Code != http.StatusCreated {
			t.Fatalf("post %d: %d (%s)", i, rec.Code, rec.Body.String())
		}
	}

	rec := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/statement/export", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv" {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	if trunc := rec.Header().Get("Export-Truncated"); trunc != "false" {
		t.Errorf("Export-Truncated = %q, want false", trunc)
	}

	reader := csv.NewReader(strings.NewReader(rec.Body.String()))
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	wantHeader := []string{"posting_id", "transaction_id", "created_at", "amount", "currency", "running_balance", "description"}
	if len(rows) == 0 {
		t.Fatal("csv has no rows at all, want a header")
	}
	for i, col := range wantHeader {
		if rows[0][i] != col {
			t.Errorf("header[%d] = %q, want %q", i, rows[0][i], col)
		}
	}
	if len(rows)-1 != 3 {
		t.Fatalf("data rows = %d, want 3", len(rows)-1)
	}
	// Newest first: the last posting's running balance (300) is the first
	// data row.
	if rows[1][5] != "300" {
		t.Errorf("first data row running_balance = %q, want 300", rows[1][5])
	}
	if rows[1][6] != "deposit" {
		t.Errorf("first data row description = %q, want %q", rows[1][6], "deposit")
	}
}

// TestStatementExport_JSON checks the json format returns the same entries
// as an array with matching fields.
func TestStatementExport_JSON(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	other := createAccount(t, r, "Other", "asset")

	rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
		"currency": "USD",
		"postings": []map[string]any{
			{"account_id": cash, "amount": 500, "description": "sale"},
			{"account_id": other, "amount": -500},
		},
	}, map[string]string{"Idempotency-Key": "statement-export-json-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d (%s)", rec.Code, rec.Body.String())
	}

	exportRec := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/statement/export?format=json", nil)
	if exportRec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", exportRec.Code, exportRec.Body.String())
	}
	if ct := exportRec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var entries []struct {
		PostingID      string `json:"posting_id"`
		TransactionID  string `json:"transaction_id"`
		Amount         int64  `json:"amount"`
		Currency       string `json:"currency"`
		RunningBalance int64  `json:"running_balance"`
		Description    string `json:"description"`
	}
	if err := json.Unmarshal(exportRec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode json export: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Amount != 500 || e.RunningBalance != 500 || e.Currency != "USD" || e.Description != "sale" {
		t.Errorf("entry = %+v, want amount=500 running_balance=500 currency=USD description=sale", e)
	}
}

// TestStatementExport_DateFilter checks that from/to bound the export to the
// requested window.
func TestStatementExport_DateFilter(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	other := createAccount(t, r, "Other", "asset")

	rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
		"currency": "USD",
		"postings": []map[string]any{
			{"account_id": cash, "amount": 100},
			{"account_id": other, "amount": -100},
		},
	}, map[string]string{"Idempotency-Key": "statement-export-date-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d (%s)", rec.Code, rec.Body.String())
	}

	// A window strictly in the future excludes everything.
	future := "2999-01-01T00:00:00Z"
	emptyRec := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/statement/export?format=json&from="+future, nil)
	if emptyRec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", emptyRec.Code, emptyRec.Body.String())
	}
	var empty []json.RawMessage
	if err := json.Unmarshal(emptyRec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("entries with a future from= filter = %d, want 0", len(empty))
	}

	// A window strictly BEFORE the posting (to=) also excludes everything.
	// fakeRepo's clock is a plain counter stamped as Unix seconds
	// (time.Unix(f.clock, 0)), so its very first ticks land at the 1970
	// epoch: a to= right at the epoch itself is guaranteed to be earlier
	// than any posting this test creates.
	epoch := "1970-01-01T00:00:00Z"
	emptyRec2 := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/statement/export?format=json&to="+epoch, nil)
	var empty2 []json.RawMessage
	if err := json.Unmarshal(emptyRec2.Body.Bytes(), &empty2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(empty2) != 0 {
		t.Errorf("entries with a past to= filter = %d, want 0", len(empty2))
	}

	// An unbounded export sees the one posting.
	allRec := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/statement/export?format=json", nil)
	var all []json.RawMessage
	if err := json.Unmarshal(allRec.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("entries with no filter = %d, want 1", len(all))
	}

	// A malformed timestamp is a 422, not a silent ignore.
	badRec := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/statement/export?from=not-a-date", nil)
	if badRec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad from=: status %d, want 422 (%s)", badRec.Code, badRec.Body.String())
	}
}

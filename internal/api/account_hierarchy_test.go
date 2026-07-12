package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestAccountHierarchyAPI exercises the ADR-023 account hierarchy surface end
// to end over HTTP: create with parent_id, set-parent (including clearing a
// parent and rejecting a cycle), the rollup balance flag, the tree listing,
// the trial balance's rolled_up_balance column, and creating with an unknown
// parent.
func TestAccountHierarchyAPI(t *testing.T) {
	r := newAPIRouter(newFakeRepo())

	// create A(USD); create B(USD, parent_id: A) -> 201, body.parent_id == A.
	a := createAccount(t, r, "A", "asset")
	var bBody AccountBody
	t.Run("create with parent_id", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/accounts",
			map[string]any{"name": "B", "type": "asset", "currency": "USD", "parent_id": a})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &bBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if bBody.ParentID == nil || *bBody.ParentID != a {
			t.Errorf("B.parent_id = %v, want %q", bBody.ParentID, a)
		}
	})
	b := bBody.ID

	// POST /v1/accounts/{B}/parent with parent_id omitted (null) -> clears;
	// body.parent_id is null.
	t.Run("set-parent clears with omitted/null parent_id", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/accounts/"+b+"/parent", map[string]any{"parent_id": nil})
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var out AccountBody
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.ParentID != nil {
			t.Errorf("parent_id = %v, want nil after clearing", out.ParentID)
		}
	})

	// Re-parent B under A, then try to set A's parent to B: a cycle, 422.
	t.Run("set-parent rejects a cycle", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/accounts/"+b+"/parent", map[string]any{"parent_id": a})
		if rec.Code != http.StatusOK {
			t.Fatalf("re-parent B under A: status %d, want 200 (%s)", rec.Code, rec.Body.String())
		}

		rec = do(t, r, http.MethodPost, "/v1/accounts/"+a+"/parent", map[string]any{"parent_id": b})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("set A's parent to B (cycle): status %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})

	// GET /v1/accounts/{A}/balance?rollup=true -> sum of subtree; without ->
	// own only. Post to A and B independently so their own balances differ.
	contra := createAccount(t, r, "Contra", "asset")
	postTxn(t, r, a, contra, 1000, "hierarchy-own-a")
	postTxn(t, r, b, contra, 250, "hierarchy-own-b")

	t.Run("balance without rollup is own only", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/accounts/"+a+"/balance", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var out struct {
			Amount int64 `json:"amount"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Amount != 1000 {
			t.Errorf("A own balance = %d, want 1000", out.Amount)
		}
	})

	t.Run("balance with rollup=true sums the subtree", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/accounts/"+a+"/balance?rollup=true", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var out struct {
			Amount int64 `json:"amount"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Amount != 1250 {
			t.Errorf("A rolled up balance = %d, want 1250 (1000 own + 250 from B)", out.Amount)
		}
	})

	// GET /v1/accounts/tree -> rows with parent_id, depth, own_balance,
	// rolled_up_balance, parent before child.
	t.Run("list-account-tree", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/accounts/tree", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var out AccountTreeOutput
		if err := json.Unmarshal(rec.Body.Bytes(), &out.Body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		pos := make(map[string]int, len(out.Body.Accounts))
		byID := make(map[string]AccountTreeNode, len(out.Body.Accounts))
		for i, n := range out.Body.Accounts {
			pos[n.ID] = i
			byID[n.ID] = n
		}
		if len(out.Body.Accounts) != 3 {
			t.Fatalf("tree len = %d, want 3 (A, B, Contra)", len(out.Body.Accounts))
		}
		if pos[a] > pos[b] {
			t.Errorf("A (parent) must come before B (child) in tree order")
		}
		nodeA, nodeB := byID[a], byID[b]
		if nodeA.ParentID != nil {
			t.Errorf("A.parent_id = %v, want nil (root)", nodeA.ParentID)
		}
		if nodeA.Depth != 0 {
			t.Errorf("A.depth = %d, want 0", nodeA.Depth)
		}
		if nodeA.OwnBalance != 1000 {
			t.Errorf("A.own_balance = %d, want 1000", nodeA.OwnBalance)
		}
		if nodeA.RolledUpBalance != 1250 {
			t.Errorf("A.rolled_up_balance = %d, want 1250", nodeA.RolledUpBalance)
		}
		if nodeB.ParentID == nil || *nodeB.ParentID != a {
			t.Errorf("B.parent_id = %v, want %q", nodeB.ParentID, a)
		}
		if nodeB.Depth != 1 {
			t.Errorf("B.depth = %d, want 1", nodeB.Depth)
		}
		if nodeB.OwnBalance != 250 {
			t.Errorf("B.own_balance = %d, want 250", nodeB.OwnBalance)
		}
		if nodeB.RolledUpBalance != 250 {
			t.Errorf("B.rolled_up_balance = %d, want 250 (leaf: same as own)", nodeB.RolledUpBalance)
		}
	})

	// GET /v1/reports/trial-balance -> account rows carry rolled_up_balance;
	// currency nets stay zero (the balance proof is unaffected by rollups).
	t.Run("trial balance carries rolled_up_balance and still nets zero", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/reports/trial-balance", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var out TrialBalanceOutput
		if err := json.Unmarshal(rec.Body.Bytes(), &out.Body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, c := range out.Body.Currencies {
			if c.Net != 0 || c.Imbalance {
				t.Errorf("currency %s: net=%d imbalance=%v, want 0/false (rollups must not affect the proof)", c.Currency, c.Net, c.Imbalance)
			}
		}
		rolled := make(map[string]int64, len(out.Body.Accounts))
		for _, acc := range out.Body.Accounts {
			rolled[acc.AccountID] = acc.RolledUpBalance
		}
		if rolled[a] != 1250 {
			t.Errorf("trial balance A.rolled_up_balance = %d, want 1250", rolled[a])
		}
		if rolled[b] != 250 {
			t.Errorf("trial balance B.rolled_up_balance = %d, want 250", rolled[b])
		}
	})

	// Create with a nonexistent parent_id -> 422.
	t.Run("create with nonexistent parent_id is 422", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/accounts",
			map[string]any{"name": "Orphan", "type": "asset", "currency": "USD", "parent_id": "00000000-0000-0000-0000-000000000099"})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})
}

// TestAccountTreePathDoesNotHitGetAccount proves the static
// "/v1/accounts/tree" path is never mistaken for get-account's "/v1/accounts/{id}"
// wildcard: a request for "tree" must reach list-account-tree (a JSON object
// with an "accounts" array), not get-account with id="tree" (which would 404
// or 422 depending on how the id parses).
func TestAccountTreePathDoesNotHitGetAccount(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	createAccount(t, r, "Solo", "asset")

	rec := do(t, r, http.MethodGet, "/v1/accounts/tree", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/accounts/tree: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out AccountTreeOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &out.Body); err != nil {
		t.Fatalf("decode as AccountTreeOutput: %v (body: %s)", err, rec.Body.String())
	}
	if len(out.Body.Accounts) != 1 {
		t.Errorf("tree accounts len = %d, want 1", len(out.Body.Accounts))
	}
}

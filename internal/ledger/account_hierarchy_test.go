package ledger_test

import (
	"context"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// fakeHierarchyRepo is a minimal domain.Repository test double for exercising
// AccountService's tree rollup and set-parent methods without a database
// (ADR-023). It embeds a nil domain.Repository so it satisfies the full
// interface by delegation, mirroring fakeSchemeRepo in
// fingerprint_scheme_test.go: any method not overridden below panics if
// called, turning an unexpected extra repo call into a hard test failure.
type fakeHierarchyRepo struct {
	domain.Repository

	rows []domain.AccountBalanceRow

	setParentN   int64
	setParentErr error
	gotAccount   domain.Account
	getAccErr    error
}

func (f *fakeHierarchyRepo) AllAccountBalances(_ context.Context, _ string) ([]domain.AccountBalanceRow, error) {
	return f.rows, nil
}

func (f *fakeHierarchyRepo) SetAccountParent(_ context.Context, _, _ string, _ *string) (int64, error) {
	return f.setParentN, f.setParentErr
}

func (f *fakeHierarchyRepo) GetAccount(_ context.Context, _, _ string) (domain.Account, error) {
	return f.gotAccount, f.getAccErr
}

func strPtr(s string) *string { return &s }

// TestServiceTreeRollup covers a single three-level chain A(root) -> B -> C
// with own balances 100, 20, 5: Tree must return nodes in parent-before-child
// order with the right Depth and a RolledUpBalance that is own plus every
// descendant's own balance.
func TestServiceTreeRollup(t *testing.T) {
	t.Parallel()
	repo := &fakeHierarchyRepo{
		rows: []domain.AccountBalanceRow{
			{Account: domain.Account{ID: "A", Name: "A", ParentID: nil}, Balance: 100},
			{Account: domain.Account{ID: "B", Name: "B", ParentID: strPtr("A")}, Balance: 20},
			{Account: domain.Account{ID: "C", Name: "C", ParentID: strPtr("B")}, Balance: 5},
		},
	}
	svc := ledger.NewAccountService(repo)

	nodes, err := svc.Tree(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}

	wantIDs := []string{"A", "B", "C"}
	wantDepth := []int{0, 1, 2}
	wantOwn := []int64{100, 20, 5}
	wantRolled := []int64{125, 25, 5}

	if len(nodes) != len(wantIDs) {
		t.Fatalf("Tree: got %d nodes, want %d", len(nodes), len(wantIDs))
	}
	for i, n := range nodes {
		if n.Account.ID != wantIDs[i] {
			t.Errorf("node[%d].Account.ID = %q, want %q (order must be parent-before-child)", i, n.Account.ID, wantIDs[i])
		}
		if n.Depth != wantDepth[i] {
			t.Errorf("node[%d].Depth = %d, want %d", i, n.Depth, wantDepth[i])
		}
		if n.OwnBalance != wantOwn[i] {
			t.Errorf("node[%d].OwnBalance = %d, want %d", i, n.OwnBalance, wantOwn[i])
		}
		if n.RolledUpBalance != wantRolled[i] {
			t.Errorf("node[%d].RolledUpBalance = %d, want %d", i, n.RolledUpBalance, wantRolled[i])
		}
	}
}

// TestServiceTreeMultipleRoots covers two independent root subtrees: each
// root's rollup must only include its own descendants, never leaking into
// the other root's subtree.
func TestServiceTreeMultipleRoots(t *testing.T) {
	t.Parallel()
	repo := &fakeHierarchyRepo{
		rows: []domain.AccountBalanceRow{
			{Account: domain.Account{ID: "R1", Name: "R1", ParentID: nil}, Balance: 10},
			{Account: domain.Account{ID: "R1C1", Name: "R1C1", ParentID: strPtr("R1")}, Balance: 3},
			{Account: domain.Account{ID: "R2", Name: "R2", ParentID: nil}, Balance: 50},
			{Account: domain.Account{ID: "R2C1", Name: "R2C1", ParentID: strPtr("R2")}, Balance: 7},
			{Account: domain.Account{ID: "R2C2", Name: "R2C2", ParentID: strPtr("R2")}, Balance: 1},
		},
	}
	svc := ledger.NewAccountService(repo)

	nodes, err := svc.Tree(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}

	rolled := make(map[string]int64, len(nodes))
	depth := make(map[string]int, len(nodes))
	var order []string
	for _, n := range nodes {
		rolled[n.Account.ID] = n.RolledUpBalance
		depth[n.Account.ID] = n.Depth
		order = append(order, n.Account.ID)
	}

	wantRolled := map[string]int64{
		"R1": 13, "R1C1": 3,
		"R2": 58, "R2C1": 7, "R2C2": 1,
	}
	for id, want := range wantRolled {
		if got := rolled[id]; got != want {
			t.Errorf("RolledUpBalance[%s] = %d, want %d", id, got, want)
		}
	}
	if depth["R1"] != 0 || depth["R2"] != 0 {
		t.Errorf("root depths = R1:%d R2:%d, want both 0", depth["R1"], depth["R2"])
	}
	if depth["R1C1"] != 1 || depth["R2C1"] != 1 || depth["R2C2"] != 1 {
		t.Errorf("child depths = R1C1:%d R2C1:%d R2C2:%d, want all 1", depth["R1C1"], depth["R2C1"], depth["R2C2"])
	}

	// Each root must appear before its own children (parent-before-child),
	// independent of the other subtree's position.
	pos := make(map[string]int, len(order))
	for i, id := range order {
		pos[id] = i
	}
	if pos["R1"] > pos["R1C1"] {
		t.Errorf("R1 (parent) must come before R1C1 (child); order = %v", order)
	}
	if pos["R2"] > pos["R2C1"] || pos["R2"] > pos["R2C2"] {
		t.Errorf("R2 (parent) must come before its children; order = %v", order)
	}
}

// TestServiceSetParentClear proves SetParent(nil) clears an account's parent
// and returns the updated account read back from the repo.
func TestServiceSetParentClear(t *testing.T) {
	t.Parallel()
	repo := &fakeHierarchyRepo{
		setParentN: 1,
		gotAccount: domain.Account{ID: "acct-1", Name: "Cleared", ParentID: nil},
	}
	svc := ledger.NewAccountService(repo)

	got, err := svc.SetParent(context.Background(), "tenant-1", "acct-1", nil)
	if err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if got.ParentID != nil {
		t.Errorf("SetParent(nil): got.ParentID = %v, want nil", got.ParentID)
	}
	if got.ID != "acct-1" {
		t.Errorf("SetParent(nil): got.ID = %q, want %q", got.ID, "acct-1")
	}
}

// TestServiceSetParentNotFound proves a zero rows-updated count (no such
// account) surfaces as domain.ErrAccountNotFound, not a raw repo call that
// happens to return a zero-value account.
func TestServiceSetParentNotFound(t *testing.T) {
	t.Parallel()
	repo := &fakeHierarchyRepo{setParentN: 0}
	svc := ledger.NewAccountService(repo)

	_, err := svc.SetParent(context.Background(), "tenant-1", "missing", strPtr("some-parent"))
	if err != domain.ErrAccountNotFound {
		t.Fatalf("SetParent for missing account: err = %v, want %v", err, domain.ErrAccountNotFound)
	}
}

// TestServiceSetParentRepoError proves a repo-surfaced error (cycle, currency
// mismatch, or unknown parent) is returned as-is, without ever reaching
// GetAccount.
func TestServiceSetParentRepoError(t *testing.T) {
	t.Parallel()
	repo := &fakeHierarchyRepo{setParentErr: domain.ErrInvalidHierarchy}
	svc := ledger.NewAccountService(repo)

	_, err := svc.SetParent(context.Background(), "tenant-1", "acct-1", strPtr("acct-1"))
	if err != domain.ErrInvalidHierarchy {
		t.Fatalf("SetParent with a cycle: err = %v, want %v", err, domain.ErrInvalidHierarchy)
	}
}

// TestServiceRolledUpBalance proves RolledUpBalance is a thin passthrough to
// the repo method of the same name.
func TestServiceRolledUpBalance(t *testing.T) {
	t.Parallel()
	want, err := domain.NewMoney(125, "USD")
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	repo := &fakeRolledUpRepo{balance: want}
	svc := ledger.NewAccountService(repo)

	got, err := svc.RolledUpBalance(context.Background(), "tenant-1", "acct-1")
	if err != nil {
		t.Fatalf("RolledUpBalance: %v", err)
	}
	if got != want {
		t.Errorf("RolledUpBalance = %v, want %v", got, want)
	}
}

type fakeRolledUpRepo struct {
	domain.Repository
	balance domain.Money
}

func (f *fakeRolledUpRepo) RolledUpBalance(_ context.Context, _, _ string) (domain.Money, error) {
	return f.balance, nil
}

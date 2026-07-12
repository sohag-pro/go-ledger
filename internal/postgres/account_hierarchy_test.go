package postgres_test

// ADR-023 account hierarchy: parent_id, the accounts_hierarchy_guard trigger
// (self-parent, cycle, and currency-mismatch rejection), and the two rollup
// queries (RolledUpBalance, AllAccountBalances). These are integration tests
// against a real Postgres, reusing the package's shared testcontainer
// (newTestPool, TestMain in repository_test.go): the trigger's plpgsql walk
// and the recursive CTE cannot be exercised meaningfully against a fake.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// createHierarchyTestAccount is a small helper for these tests: creates an
// account of the given currency for tenant and returns it. Every test in this
// file needs several plain accounts to wire into a hierarchy, so this avoids
// repeating the CreateAccount + error-check boilerplate at each call site.
func createHierarchyTestAccount(t *testing.T, repo *postgres.Repository, tenant, name, currency string) domain.Account {
	t.Helper()
	a := &domain.Account{Name: name, Type: domain.Asset, Currency: domain.Currency(currency)}
	if err := repo.CreateAccount(context.Background(), tenant, a); err != nil {
		t.Fatalf("create account %s: %v", name, err)
	}
	return *a
}

// TestAccountHierarchyGuard covers the accounts_hierarchy_guard trigger
// (migration 0032, ADR-023): a self-parent, a 2-node cycle, a deeper cycle,
// and a cross-currency child are all rejected with domain.ErrInvalidHierarchy;
// an unknown parent id is rejected with domain.ErrParentNotFound; and a valid
// nesting succeeds.
func TestAccountHierarchyGuard(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "account hierarchy guard test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	a := createHierarchyTestAccount(t, repo, tenant, "A", "USD")
	b := createHierarchyTestAccount(t, repo, tenant, "B", "USD")

	// Valid nesting: B's parent is A.
	rows, err := repo.SetAccountParent(ctx, tenant, b.ID, &a.ID)
	if err != nil {
		t.Fatalf("SetAccountParent(B, A) = %v, want nil", err)
	}
	if rows != 1 {
		t.Errorf("SetAccountParent(B, A) rows = %d, want 1", rows)
	}

	// Cycle: A's parent is B, but B's parent is already A.
	if _, err := repo.SetAccountParent(ctx, tenant, a.ID, &b.ID); !errors.Is(err, domain.ErrInvalidHierarchy) {
		t.Errorf("SetAccountParent(A, B) (cycle A->B->A) = %v, want ErrInvalidHierarchy", err)
	}

	// Self-parent.
	s := createHierarchyTestAccount(t, repo, tenant, "S", "USD")
	if _, err := repo.SetAccountParent(ctx, tenant, s.ID, &s.ID); !errors.Is(err, domain.ErrInvalidHierarchy) {
		t.Errorf("SetAccountParent(S, S) (self-parent) = %v, want ErrInvalidHierarchy", err)
	}

	// Currency mismatch: E is EUR, A is USD.
	e := createHierarchyTestAccount(t, repo, tenant, "E", "EUR")
	if _, err := repo.SetAccountParent(ctx, tenant, e.ID, &a.ID); !errors.Is(err, domain.ErrInvalidHierarchy) {
		t.Errorf("SetAccountParent(E, A) (currency mismatch) = %v, want ErrInvalidHierarchy", err)
	}

	// Unknown parent id.
	unknown := uuid.NewString()
	if _, err := repo.SetAccountParent(ctx, tenant, s.ID, &unknown); !errors.Is(err, domain.ErrParentNotFound) {
		t.Errorf("SetAccountParent(S, unknown) = %v, want ErrParentNotFound", err)
	}

	// A deeper cycle: C -> B -> A (already wired), then try to set A's
	// parent to C, which would close a 3-node cycle A->C->B->A.
	c := createHierarchyTestAccount(t, repo, tenant, "C", "USD")
	if _, err := repo.SetAccountParent(ctx, tenant, c.ID, &b.ID); err != nil {
		t.Fatalf("SetAccountParent(C, B) = %v, want nil", err)
	}
	if _, err := repo.SetAccountParent(ctx, tenant, a.ID, &c.ID); !errors.Is(err, domain.ErrInvalidHierarchy) {
		t.Errorf("SetAccountParent(A, C) (deeper cycle A->C->B->A) = %v, want ErrInvalidHierarchy", err)
	}
}

// postHierarchyTestTransaction posts a balanced two-leg transaction crediting
// contra and debiting accountID by amount, so accountID's own derived balance
// increases by amount. contra is a throwaway offsetting account: these tests
// only care about the debit side's balance, not double-entry realism beyond
// staying balanced.
func postHierarchyTestTransaction(t *testing.T, repo *postgres.Repository, tenant, accountID, contraID string, amount int64, currency string) { //nolint:unparam // currency is a real, reusable parameter even though every current caller passes "USD"
	t.Helper()
	debit, err := domain.NewMoney(amount, domain.Currency(currency))
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	credit, err := domain.NewMoney(-amount, domain.Currency(currency))
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	txn := &domain.Transaction{
		Postings: []domain.Posting{
			{AccountID: accountID, Amount: debit},
			{AccountID: contraID, Amount: credit},
		},
	}
	if err := repo.CreateTransaction(context.Background(), tenant, txn); err != nil {
		t.Fatalf("post transaction (account %s, amount %d): %v", accountID, amount, err)
	}
}

// TestRolledUpBalance builds A -> B -> C (A is the root, C the leaf), posts a
// distinct amount to each of A, B, and C, and checks RolledUpBalance sums
// exactly the subtree at each level: the root sees everything, the middle
// node sees itself and the leaf, and the leaf sees only itself.
func TestRolledUpBalance(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "rolled up balance test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	contra := createHierarchyTestAccount(t, repo, tenant, "Contra", "USD")
	a := createHierarchyTestAccount(t, repo, tenant, "A", "USD")
	b := createHierarchyTestAccount(t, repo, tenant, "B", "USD")
	c := createHierarchyTestAccount(t, repo, tenant, "C", "USD")

	if _, err := repo.SetAccountParent(ctx, tenant, b.ID, &a.ID); err != nil {
		t.Fatalf("SetAccountParent(B, A): %v", err)
	}
	if _, err := repo.SetAccountParent(ctx, tenant, c.ID, &b.ID); err != nil {
		t.Fatalf("SetAccountParent(C, B): %v", err)
	}

	const ownA, ownB, ownC = int64(1000), int64(200), int64(30)
	postHierarchyTestTransaction(t, repo, tenant, a.ID, contra.ID, ownA, "USD")
	postHierarchyTestTransaction(t, repo, tenant, b.ID, contra.ID, ownB, "USD")
	postHierarchyTestTransaction(t, repo, tenant, c.ID, contra.ID, ownC, "USD")

	tests := []struct {
		name      string
		accountID string
		want      int64
	}{
		{"root A sees the whole subtree", a.ID, ownA + ownB + ownC},
		{"middle B sees itself and its leaf", b.ID, ownB + ownC},
		{"leaf C sees only itself", c.ID, ownC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.RolledUpBalance(ctx, tenant, tt.accountID)
			if err != nil {
				t.Fatalf("RolledUpBalance(%s): %v", tt.name, err)
			}
			if got.Amount() != tt.want {
				t.Errorf("RolledUpBalance(%s) = %d, want %d", tt.name, got.Amount(), tt.want)
			}
			if got.Currency() != "USD" {
				t.Errorf("RolledUpBalance(%s) currency = %s, want USD", tt.name, got.Currency())
			}
		})
	}
}

// TestAllAccountBalances checks that AllAccountBalances returns every account
// for the tenant with its own (non-rolled-up) derived balance and its
// parent_id, so a caller can build the tree and roll up in memory.
func TestAllAccountBalances(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "all account balances test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	contra := createHierarchyTestAccount(t, repo, tenant, "Contra", "USD")
	parent := createHierarchyTestAccount(t, repo, tenant, "Parent", "USD")
	child := createHierarchyTestAccount(t, repo, tenant, "Child", "USD")
	if _, err := repo.SetAccountParent(ctx, tenant, child.ID, &parent.ID); err != nil {
		t.Fatalf("SetAccountParent(Child, Parent): %v", err)
	}

	postHierarchyTestTransaction(t, repo, tenant, parent.ID, contra.ID, 500, "USD")
	postHierarchyTestTransaction(t, repo, tenant, child.ID, contra.ID, 75, "USD")

	rows, err := repo.AllAccountBalances(ctx, tenant)
	if err != nil {
		t.Fatalf("AllAccountBalances: %v", err)
	}
	byID := make(map[string]domain.AccountBalanceRow, len(rows))
	for _, row := range rows {
		byID[row.Account.ID] = row
	}
	if len(rows) != 3 {
		t.Fatalf("AllAccountBalances len = %d, want 3 (contra, parent, child)", len(rows))
	}

	parentRow, ok := byID[parent.ID]
	if !ok {
		t.Fatalf("AllAccountBalances missing parent account %s", parent.ID)
	}
	if parentRow.Balance != 500 {
		t.Errorf("parent own balance = %d, want 500 (not rolled up)", parentRow.Balance)
	}
	if parentRow.Account.ParentID != nil {
		t.Errorf("parent ParentID = %v, want nil (it is a root)", parentRow.Account.ParentID)
	}

	childRow, ok := byID[child.ID]
	if !ok {
		t.Fatalf("AllAccountBalances missing child account %s", child.ID)
	}
	if childRow.Balance != 75 {
		t.Errorf("child own balance = %d, want 75", childRow.Balance)
	}
	if childRow.Account.ParentID == nil || *childRow.Account.ParentID != parent.ID {
		t.Errorf("child ParentID = %v, want %s", childRow.Account.ParentID, parent.ID)
	}

	contraRow, ok := byID[contra.ID]
	if !ok {
		t.Fatalf("AllAccountBalances missing contra account %s", contra.ID)
	}
	if contraRow.Balance != -575 {
		t.Errorf("contra own balance = %d, want -575", contraRow.Balance)
	}
}

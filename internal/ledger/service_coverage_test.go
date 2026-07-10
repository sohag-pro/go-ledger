package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestPost_UnknownAccountsFailsAndLeavesNothingPersisted exercises the "genuine
// failure" branch of Post: idem is nil so the duplicate-key replay path never
// runs, and the posting insert fails on the foreign key check because neither
// account was ever created. That is a real, non-conflict persistence failure,
// distinct from both an idempotency conflict and a serialization conflict, and
// the whole write must roll back.
func TestPost_UnknownAccountsFailsAndLeavesNothingPersisted(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()

	txn := mkTxn(t, uuid.NewString(), uuid.NewString())
	replayed, err := svc.Post(ctx, tenant, txn, nil)
	if err == nil {
		t.Fatal("expected an error posting against nonexistent accounts")
	}
	if replayed {
		t.Error("replayed = true, want false on a genuine failure")
	}
	if errors.Is(err, domain.ErrIdempotencyConflict) || errors.Is(err, domain.ErrConflict) {
		t.Errorf("got a conflict-shaped error %v, want a plain persistence failure", err)
	}

	// The write rolled back: nothing was persisted under this id.
	if _, err := svc.Get(ctx, tenant, txn.ID); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Fatalf("get after failed post: got %v, want ErrTransactionNotFound", err)
	}
}

// TestGet_ReturnsPostedTransaction covers the found path of Get, the mirror of
// the not-found path exercised above: a committed transaction and its postings
// round-trip back out unchanged.
func TestGet_ReturnsPostedTransaction(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "service coverage test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	debit := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	credit := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, debit); err != nil {
		t.Fatalf("create debit: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, credit); err != nil {
		t.Fatalf("create credit: %v", err)
	}

	txn := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post: %v", err)
	}

	got, err := svc.Get(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != txn.ID {
		t.Errorf("get returned id %s, want %s", got.ID, txn.ID)
	}
	if len(got.Postings) != 2 {
		t.Fatalf("get returned %d postings, want 2", len(got.Postings))
	}
}

// TestAccountService_NotFound table-drives the "no such account" branch shared
// by Get, Balance, and Statement: all three resolve the account first (Balance
// and Statement both call GetAccount before touching postings) and must all
// surface domain.ErrAccountNotFound for an id nothing was ever created under.
func TestAccountService_NotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewAccountService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	missing := uuid.NewString()

	tests := []struct {
		name string
		call func() error
	}{
		{"Get", func() error {
			_, err := svc.Get(ctx, tenant, missing)
			return err
		}},
		{"Balance", func() error {
			_, err := svc.Balance(ctx, tenant, missing)
			return err
		}},
		{"Statement", func() error {
			_, _, err := svc.Statement(ctx, tenant, missing, nil, 10)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(); !errors.Is(err, domain.ErrAccountNotFound) {
				t.Fatalf("%s: got %v, want ErrAccountNotFound", tc.name, err)
			}
		})
	}
}

// TestAccountService_CreateGetListBalanceStatement covers the whole
// AccountService surface end to end: Create assigns an id, Get and List read
// it back, Balance starts at zero and reflects a posted transaction, and
// Statement returns the posting with its running balance.
func TestAccountService_CreateGetListBalanceStatement(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "service coverage test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	revenue := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := accounts.Create(ctx, tenant, cash); err != nil {
		t.Fatalf("create cash: %v", err)
	}
	if err := accounts.Create(ctx, tenant, revenue); err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	if cash.ID == "" || revenue.ID == "" {
		t.Fatal("Create did not assign an id")
	}

	got, err := accounts.Get(ctx, tenant, cash.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Cash" || got.Type != domain.Asset {
		t.Errorf("get returned %+v, want name Cash, type Asset", got)
	}

	list, err := accounts.List(ctx, tenant, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list returned %d accounts, want 2", len(list))
	}

	// Before any postings, the derived balance is zero.
	zero, err := accounts.Balance(ctx, tenant, cash.ID)
	if err != nil {
		t.Fatalf("balance (empty): %v", err)
	}
	if zero.Amount() != 0 {
		t.Errorf("balance before any postings = %d, want 0", zero.Amount())
	}

	if _, err := txns.Post(ctx, tenant, mkTxn(t, cash.ID, revenue.ID), nil); err != nil {
		t.Fatalf("post: %v", err)
	}

	bal, err := accounts.Balance(ctx, tenant, cash.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Amount() != 250 {
		t.Errorf("balance = %d, want 250", bal.Amount())
	}

	acct, entries, err := accounts.Statement(ctx, tenant, cash.ID, nil, 10)
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	if acct.ID != cash.ID {
		t.Errorf("statement returned account %s, want %s", acct.ID, cash.ID)
	}
	if len(entries) != 1 {
		t.Fatalf("statement returned %d entries, want 1", len(entries))
	}
	if entries[0].Amount.Amount() != 250 || entries[0].RunningBalance.Amount() != 250 {
		t.Errorf("statement entry = %+v, want amount 250 and running balance 250", entries[0])
	}
}

// TestAuditService_ByTransactionAndByAccount covers both AuditService reads: a
// posted transaction produces exactly one audit row, visible both by its
// transaction id and by either account it touched, while an unknown id of
// either kind yields no rows and no error.
func TestAuditService_ByTransactionAndByAccount(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	audits := ledger.NewAuditService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "service coverage test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	revenue := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := accounts.Create(ctx, tenant, cash); err != nil {
		t.Fatalf("create cash: %v", err)
	}
	if err := accounts.Create(ctx, tenant, revenue); err != nil {
		t.Fatalf("create revenue: %v", err)
	}

	txn := mkTxn(t, cash.ID, revenue.ID)
	if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post: %v", err)
	}

	rows, err := audits.ByTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("by transaction: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows for transaction = %d, want 1", len(rows))
	}
	if rows[0].Action != domain.ActionTransactionCreated {
		t.Errorf("action = %q, want %q", rows[0].Action, domain.ActionTransactionCreated)
	}
	if rows[0].TransactionID != txn.ID {
		t.Errorf("audit transaction id = %s, want %s", rows[0].TransactionID, txn.ID)
	}

	// An unknown transaction yields no rows and no error.
	none, err := audits.ByTransaction(ctx, tenant, uuid.NewString())
	if err != nil {
		t.Fatalf("by transaction (unknown): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("audit rows for unknown transaction = %d, want 0", len(none))
	}

	byAcct, err := audits.ByAccount(ctx, tenant, cash.ID, nil, 10)
	if err != nil {
		t.Fatalf("by account: %v", err)
	}
	if len(byAcct) != 1 {
		t.Fatalf("audit rows for account = %d, want 1", len(byAcct))
	}

	noneAcct, err := audits.ByAccount(ctx, tenant, uuid.NewString(), nil, 10)
	if err != nil {
		t.Fatalf("by account (unknown): %v", err)
	}
	if len(noneAcct) != 0 {
		t.Errorf("audit rows for unknown account = %d, want 0", len(noneAcct))
	}
}

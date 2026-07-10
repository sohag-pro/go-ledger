package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestCreateAccountDefaultsActiveNoFloor covers Task 5.5 (audit A1.5): an
// account created without an explicit MinBalance comes back active, with no
// floor, both on the object CreateAccount handed back and on a fresh
// GetAccount read.
func TestCreateAccountDefaultsActiveNoFloor(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "account status test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	acct := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if acct.Status != domain.AccountActive {
		t.Errorf("CreateAccount status = %q, want %q", acct.Status, domain.AccountActive)
	}
	if acct.MinBalance != nil {
		t.Errorf("CreateAccount min_balance = %v, want nil", acct.MinBalance)
	}

	got, err := repo.GetAccount(ctx, tenant, acct.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Status != domain.AccountActive {
		t.Errorf("GetAccount status = %q, want %q", got.Status, domain.AccountActive)
	}
	if got.MinBalance != nil {
		t.Errorf("GetAccount min_balance = %v, want nil", got.MinBalance)
	}
}

// TestCreateAccountWithMinBalance covers Task 5.5 (audit A1.5): an account
// created with a MinBalance (including a negative one, a legitimate
// overdraft allowance) round-trips through both CreateAccount's own return
// value and a fresh GetAccount read.
func TestCreateAccountWithMinBalance(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "account min balance test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	floor := int64(-50000)
	acct := &domain.Account{Name: "Checking", Type: domain.Asset, Currency: "USD", MinBalance: &floor}
	if err := repo.CreateAccount(ctx, tenant, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if acct.MinBalance == nil || *acct.MinBalance != floor {
		t.Fatalf("CreateAccount min_balance = %v, want %d", acct.MinBalance, floor)
	}

	got, err := repo.GetAccount(ctx, tenant, acct.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.MinBalance == nil || *got.MinBalance != floor {
		t.Fatalf("GetAccount min_balance = %v, want %d", got.MinBalance, floor)
	}

	// ListAccounts surfaces the same fields.
	list, err := repo.ListAccounts(ctx, tenant, 10)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d accounts, want 1", len(list))
	}
	if list[0].MinBalance == nil || *list[0].MinBalance != floor {
		t.Errorf("ListAccounts min_balance = %v, want %d", list[0].MinBalance, floor)
	}
	if list[0].Status != domain.AccountActive {
		t.Errorf("ListAccounts status = %q, want %q", list[0].Status, domain.AccountActive)
	}
}

// TestSetAccountStatus covers Task 5.5 (audit A1.5): a valid status updates
// the account and is visible on a fresh read; an invalid status is rejected
// without changing anything; an unknown account id is ErrAccountNotFound.
func TestSetAccountStatus(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "set account status test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	acct := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}

	if err := repo.SetAccountStatus(ctx, tenant, acct.ID, domain.AccountFrozen); err != nil {
		t.Fatalf("set account status: %v", err)
	}
	got, err := repo.GetAccount(ctx, tenant, acct.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Status != domain.AccountFrozen {
		t.Errorf("status = %q, want %q", got.Status, domain.AccountFrozen)
	}

	// Reactivate.
	if err := repo.SetAccountStatus(ctx, tenant, acct.ID, domain.AccountActive); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	got, err = repo.GetAccount(ctx, tenant, acct.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Status != domain.AccountActive {
		t.Errorf("status = %q, want %q", got.Status, domain.AccountActive)
	}

	if err := repo.SetAccountStatus(ctx, tenant, acct.ID, domain.AccountStatus("bogus")); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Errorf("SetAccountStatus(bogus) = %v, want ErrInvalidAccount", err)
	}

	if err := repo.SetAccountStatus(ctx, tenant, uuid.NewString(), domain.AccountClosed); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("SetAccountStatus(unknown id) = %v, want ErrAccountNotFound", err)
	}
}

// TestAccountPostingStates covers Task 5.5 (audit A1.5)'s tx-scoped read
// directly against Postgres: an account with posted history reports its
// correct derived balance alongside its status, min_balance, and is_system
// flag; an account with no postings yet reports balance 0 (COALESCE, not a
// missing row); and an account id with no matching row is simply absent
// from the returned map.
func TestAccountPostingStates(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "account posting states test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	floor := int64(-1000)
	withHistory := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD", MinBalance: &floor}
	if err := repo.CreateAccount(ctx, tenant, withHistory); err != nil {
		t.Fatalf("create account: %v", err)
	}
	counterparty := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, counterparty); err != nil {
		t.Fatalf("create counterparty account: %v", err)
	}
	noHistory := &domain.Account{Name: "Untouched", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, noHistory); err != nil {
		t.Fatalf("create no-history account: %v", err)
	}
	if err := repo.SetAccountStatus(ctx, tenant, withHistory.ID, domain.AccountFrozen); err != nil {
		t.Fatalf("freeze account: %v", err)
	}

	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: withHistory.ID, Amount: money(t, 5000, "USD")},
		{AccountID: counterparty.ID, Amount: money(t, -5000, "USD")},
	}}
	if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
		t.Fatalf("create transaction: %v", err)
	}

	missingID := uuid.NewString()
	var states map[string]domain.AccountPostingState
	err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		var err error
		states, err = tx.AccountPostingStates(ctx, tenant, []string{withHistory.ID, noHistory.ID, missingID})
		return err
	})
	if err != nil {
		t.Fatalf("run in tx: %v", err)
	}

	got, ok := states[withHistory.ID]
	if !ok {
		t.Fatalf("states missing entry for withHistory account")
	}
	if got.Status != domain.AccountFrozen {
		t.Errorf("withHistory status = %q, want %q", got.Status, domain.AccountFrozen)
	}
	if got.MinBalance == nil || *got.MinBalance != floor {
		t.Errorf("withHistory min_balance = %v, want %d", got.MinBalance, floor)
	}
	if got.IsSystem {
		t.Error("withHistory is_system = true, want false")
	}
	if got.Balance != 5000 {
		t.Errorf("withHistory balance = %d, want 5000", got.Balance)
	}

	noHist, ok := states[noHistory.ID]
	if !ok {
		t.Fatalf("states missing entry for noHistory account")
	}
	if noHist.Balance != 0 {
		t.Errorf("noHistory balance = %d, want 0", noHist.Balance)
	}
	if noHist.Status != domain.AccountActive {
		t.Errorf("noHistory status = %q, want %q", noHist.Status, domain.AccountActive)
	}

	if _, ok := states[missingID]; ok {
		t.Error("states has an entry for an id that was never created, want absent")
	}
}

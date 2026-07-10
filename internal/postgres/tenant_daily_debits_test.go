package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// dailyDebitsAccount creates and returns an account of the given currency
// for tenant, creating the tenant row first if it does not already exist
// (accounts_tenant_fk, migration 0011).
func dailyDebitsAccount(t *testing.T, repo *postgres.Repository, tenant string, currency domain.Currency) domain.Account {
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, tenant, "daily debits test tenant"); err != nil && !errors.Is(err, domain.ErrTenantAlreadyExists) {
		t.Fatalf("create tenant: %v", err)
	}
	a := &domain.Account{Name: "acct-" + uuid.NewString(), Type: domain.Asset, Currency: currency}
	if err := repo.CreateAccount(ctx, tenant, a); err != nil {
		t.Fatalf("create %s account: %v", currency, err)
	}
	return *a
}

// mustPostTxn posts a balanced two-posting transaction of amount in currency
// (debit into acctA, credit out of acctB), failing the test on any error.
func mustPostTxn(t *testing.T, repo *postgres.Repository, tenant, acctA, acctB string, amount int64, currency domain.Currency) {
	t.Helper()
	d, err := domain.NewMoney(amount, currency)
	if err != nil {
		t.Fatalf("NewMoney debit: %v", err)
	}
	c, err := domain.NewMoney(-amount, currency)
	if err != nil {
		t.Fatalf("NewMoney credit: %v", err)
	}
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: acctA, Amount: d},
		{AccountID: acctB, Amount: c},
	}}
	if err := repo.CreateTransaction(context.Background(), tenant, txn); err != nil {
		t.Fatalf("post transaction: %v", err)
	}
}

// tenantDailyDebits is a small helper that opens a RunInTx purely to reach
// the domain.Tx method under test (Task 2.4b, audit A3.4): TenantDailyDebits
// lives on domain.Tx, not domain.Repository, since it must be read from
// inside the caller's own SERIALIZABLE transaction (see the interface's doc
// comment).
func tenantDailyDebits(t *testing.T, repo *postgres.Repository, tenant string) map[string]int64 {
	t.Helper()
	var got map[string]int64
	err := repo.RunInTx(context.Background(), tenant, func(ctx context.Context, tx domain.Tx) error {
		var err error
		got, err = tx.TenantDailyDebits(ctx, tenant)
		return err
	})
	if err != nil {
		t.Fatalf("TenantDailyDebits: %v", err)
	}
	return got
}

// TestTenantDailyDebits_SumsPositiveAmountsPerCurrency proves
// TenantDailyDebits sums only DEBIT (positive) amounts, grouped by currency,
// and that a currency with no postings today is simply absent from the
// returned map rather than present with a zero value.
func TestTenantDailyDebits_SumsPositiveAmountsPerCurrency(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	tenant := uuid.NewString()

	usdA := dailyDebitsAccount(t, repo, tenant, "USD")
	usdB := dailyDebitsAccount(t, repo, tenant, "USD")
	eurA := dailyDebitsAccount(t, repo, tenant, "EUR")
	eurB := dailyDebitsAccount(t, repo, tenant, "EUR")

	mustPostTxn(t, repo, tenant, usdA.ID, usdB.ID, 700, "USD")
	mustPostTxn(t, repo, tenant, usdA.ID, usdB.ID, 200, "USD")
	mustPostTxn(t, repo, tenant, eurA.ID, eurB.ID, 50, "EUR")

	got := tenantDailyDebits(t, repo, tenant)
	if got["USD"] != 900 {
		t.Errorf("USD total = %d, want 900 (700+200 debits; credits excluded)", got["USD"])
	}
	if got["EUR"] != 50 {
		t.Errorf("EUR total = %d, want 50", got["EUR"])
	}
	if _, ok := got["GBP"]; ok {
		t.Error("GBP present in map with no postings today; want it absent, not a zero entry")
	}
}

// TestTenantDailyDebits_NoPostingsIsEmptyMap proves a tenant with no
// postings at all gets an empty (not nil-panicking, not error) map back.
func TestTenantDailyDebits_NoPostingsIsEmptyMap(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	tenant := uuid.NewString()
	if err := repo.CreateTenant(context.Background(), tenant, "empty daily debits tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	got := tenantDailyDebits(t, repo, tenant)
	if len(got) != 0 {
		t.Errorf("got %v, want an empty map", got)
	}
}

// TestTenantDailyDebits_ExcludesYesterday proves the "today" boundary is
// real: a posting backdated to yesterday (created_at stamped directly, the
// same raw-SQL fixture technique the seeder and other tests in this repo use
// to backdate rows) does not count toward today's total, while a posting
// from today does.
func TestTenantDailyDebits_ExcludesYesterday(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	usdA := dailyDebitsAccount(t, repo, tenant, "USD")
	usdB := dailyDebitsAccount(t, repo, tenant, "USD")

	// A posting backdated to yesterday, written directly (bypassing
	// CreateTransaction, which always stamps created_at via the column's
	// own now() default): a transaction row is required first for the
	// posting's foreign key. Both legs are inserted in a single explicit
	// transaction: the balance-invariant trigger is deferred to commit, and
	// each leg alone is unbalanced (a single nonzero posting), so committing
	// after only one leg would trip it (postings_balanced).
	txnID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`, txnID, tenant); err != nil {
		t.Fatalf("seed backdated transaction: %v", err)
	}
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	backdateTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin backdate tx: %v", err)
	}
	if _, err := backdateTx.Exec(ctx,
		`INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount, currency, description, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, '', $7)`,
		uuid.NewString(), tenant, txnID, usdA.ID, 100_000, "USD", yesterday); err != nil {
		t.Fatalf("seed backdated debit posting: %v", err)
	}
	if _, err := backdateTx.Exec(ctx,
		`INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount, currency, description, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, '', $7)`,
		uuid.NewString(), tenant, txnID, usdB.ID, -100_000, "USD", yesterday); err != nil {
		t.Fatalf("seed backdated credit posting: %v", err)
	}
	if err := backdateTx.Commit(ctx); err != nil {
		t.Fatalf("commit backdate tx: %v", err)
	}

	// A real post, today.
	mustPostTxn(t, repo, tenant, usdA.ID, usdB.ID, 300, "USD")

	got := tenantDailyDebits(t, repo, tenant)
	if got["USD"] != 300 {
		t.Errorf("USD total = %d, want 300 (yesterday's 100000 debit must not count toward today)", got["USD"])
	}
}

// TestTenantDailyDebits_MalformedTenantIDErrors proves the id-parsing branch
// of txRepo.TenantDailyDebits fails cleanly on a syntactically invalid
// tenant id, the same "malformed ids return errors" defense-in-depth every
// other repository/tx method in this package has (see
// TestMalformedIDsReturnErrors in coverage_test.go).
func TestTenantDailyDebits_MalformedTenantIDErrors(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	// RunInTx itself only keys its in-process mutex off the string (no
	// parsing), so a bad id reaches TenantDailyDebits's own uuid.Parse.
	err := repo.RunInTx(context.Background(), "not-a-uuid", func(ctx context.Context, tx domain.Tx) error {
		_, err := tx.TenantDailyDebits(ctx, "not-a-uuid")
		return err
	})
	if err == nil {
		t.Fatal("TenantDailyDebits with a malformed tenant id: expected an error")
	}
}

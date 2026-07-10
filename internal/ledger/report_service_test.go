package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestTrialBalance_NetsToZeroPerCurrency is the balance-proof test (Task
// 6.3, audit A9.2): post several USD transactions plus a cross-currency
// convert (which brings the FX clearing accounts into play, ADR-014), then
// check the trial balance's per-currency net is exactly zero for every
// currency involved, and that every account's reported balance matches an
// independent Balance() read.
func TestTrialBalance_NetsToZeroPerCurrency(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	reports := ledger.NewReportService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	rev := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateTenant(ctx, tenant, "trial balance tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, cash); err != nil {
		t.Fatalf("create cash: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, rev); err != nil {
		t.Fatalf("create revenue: %v", err)
	}

	// Three plain USD transactions of different sizes, so the net is not
	// trivially the same posting repeated.
	for i, amount := range []int64{10000, 25000, 4500} {
		txn := mkTxn(t, cash.ID, rev.ID)
		txn.Postings[0].Amount, _ = domain.NewMoney(amount, "USD")
		txn.Postings[1].Amount, _ = domain.NewMoney(-amount, "USD")
		if _, err := svc.Post(ctx, tenant, txn, &domain.Idempotency{Key: "trial-balance-usd-" + uuid.NewString()}); err != nil {
			t.Fatalf("post USD txn %d: %v", i, err)
		}
	}

	// A cross-currency convert brings EUR and both clearing accounts into
	// play (ADR-014): the trial balance must include them and still net to
	// zero in both currencies.
	const quote = domain.Currency("EUR")
	seedConvertRate(t, pool, quote, 92_000_000, 50)
	convertSvc := newConvertService(pool)
	eurAcct := newConvertAccount(t, repo, tenant, quote)
	req := ledger.ConvertRequest{FromAccountID: cash.ID, ToAccountID: eurAcct.ID, SourceAmount: 5000}
	if _, _, err := convertSvc.Convert(ctx, tenant, req, &domain.Idempotency{Key: "trial-balance-convert-1"}); err != nil {
		t.Fatalf("convert: %v", err)
	}

	tb, err := reports.TrialBalance(ctx, tenant)
	if err != nil {
		t.Fatalf("TrialBalance: %v", err)
	}

	if len(tb.Currencies) != 2 {
		t.Fatalf("currencies = %+v, want exactly 2 (USD and EUR)", tb.Currencies)
	}
	for _, c := range tb.Currencies {
		if c.Net != 0 {
			t.Errorf("currency %s net = %d, want 0 (the balance proof)", c.Currency, c.Net)
		}
		if c.Imbalance {
			t.Errorf("currency %s reported as imbalanced, want false", c.Currency)
		}
	}

	// Every account's reported balance in the report matches an independent
	// Balance() read, and the FX clearing accounts are present and marked
	// is_system.
	sawSystem := false
	for _, a := range tb.Accounts {
		bal, err := repo.Balance(ctx, tenant, a.AccountID)
		if err != nil {
			t.Fatalf("balance %s: %v", a.AccountID, err)
		}
		if bal.Amount() != a.Balance {
			t.Errorf("account %s (%s) report balance = %d, want %d (independent Balance() read)", a.AccountID, a.Name, a.Balance, bal.Amount())
		}
		if a.IsSystem {
			sawSystem = true
		}
	}
	if !sawSystem {
		t.Error("trial balance accounts: no system (FX clearing) account seen, want at least one")
	}
	// Not a hard assertion on the exact count: a convert creates clearing
	// accounts lazily, one per currency actually touched, an implementation
	// detail of GetOrCreateClearingAccount this test should not pin.
	t.Logf("trial balance accounts count = %d (cash, revenue, eur account, plus clearing accounts)", len(tb.Accounts))

	// Tenant isolation: a second, untouched tenant's trial balance is empty.
	otherTenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, otherTenant, "other trial balance tenant"); err != nil {
		t.Fatalf("create other tenant: %v", err)
	}
	otherTB, err := reports.TrialBalance(ctx, otherTenant)
	if err != nil {
		t.Fatalf("TrialBalance other tenant: %v", err)
	}
	if len(otherTB.Currencies) != 0 {
		t.Errorf("other tenant currencies = %+v, want none", otherTB.Currencies)
	}
	if len(otherTB.Accounts) != 0 {
		t.Errorf("other tenant accounts = %+v, want none", otherTB.Accounts)
	}
}

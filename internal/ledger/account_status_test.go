package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// newAccountStatusTenant creates a fresh tenant for the account
// status/min-balance tests below (Task 5.5, audit A1.5).
func newAccountStatusTenant(t *testing.T, repo *postgres.Repository) string {
	t.Helper()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(context.Background(), tenant, "account status test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return tenant
}

// TestPost_AccountStatusGating covers the core of Task 5.5 (audit A1.5): a
// post into a frozen or closed non-system account is rejected with
// *domain.AccountNotActiveError (matching domain.ErrAccountNotActive); an
// active account posts normally, and the balance actually moves.
func TestPost_AccountStatusGating(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tests := []struct {
		name   string
		status domain.AccountStatus
		reject bool
	}{
		{"active posts", domain.AccountActive, false},
		{"frozen rejected", domain.AccountFrozen, true},
		{"closed rejected", domain.AccountClosed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tenant := newAccountStatusTenant(t, repo)
			debit := mustCreateAccount(t, repo, tenant, "USD")
			credit := mustCreateAccount(t, repo, tenant, "USD")
			if tt.status != domain.AccountActive {
				if err := repo.SetAccountStatus(ctx, tenant, debit.ID, tt.status); err != nil {
					t.Fatalf("set account status: %v", err)
				}
			}

			txn := txnOf(t, debit.ID, credit.ID, 1000, "USD")
			_, err := svc.Post(ctx, tenant, txn, nil)

			if tt.reject {
				var notActive *domain.AccountNotActiveError
				if !errors.As(err, &notActive) {
					t.Fatalf("Post() error = %v, want *domain.AccountNotActiveError", err)
				}
				if !errors.Is(err, domain.ErrAccountNotActive) {
					t.Errorf("errors.Is(err, ErrAccountNotActive) = false, want true")
				}
				if notActive.AccountID != debit.ID || notActive.Status != tt.status {
					t.Errorf("AccountNotActiveError = %+v, want AccountID=%s Status=%s", notActive, debit.ID, tt.status)
				}
				// Rejected atomically: no partial posting landed.
				bal, balErr := repo.Balance(ctx, tenant, debit.ID)
				if balErr != nil {
					t.Fatalf("balance: %v", balErr)
				}
				if bal.Amount() != 0 {
					t.Errorf("balance after rejected post = %d, want 0", bal.Amount())
				}
				return
			}

			if err != nil {
				t.Fatalf("Post() error = %v, want nil", err)
			}
			bal, err := repo.Balance(ctx, tenant, debit.ID)
			if err != nil {
				t.Fatalf("balance: %v", err)
			}
			if bal.Amount() != 1000 {
				t.Errorf("balance = %d, want 1000", bal.Amount())
			}
		})
	}
}

// TestPost_AccountStatusGating_CreditSide covers the credit leg too: a
// posting's OTHER (credit) account being frozen also rejects the whole
// transaction, not just the debit side (Task 5.5, audit A1.5). Every
// touched account is checked, not just postings[0].
func TestPost_AccountStatusGating_CreditSide(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := newAccountStatusTenant(t, repo)
	debit := mustCreateAccount(t, repo, tenant, "USD")
	credit := mustCreateAccount(t, repo, tenant, "USD")
	if err := repo.SetAccountStatus(ctx, tenant, credit.ID, domain.AccountClosed); err != nil {
		t.Fatalf("close credit account: %v", err)
	}

	txn := txnOf(t, debit.ID, credit.ID, 1000, "USD")
	_, err := svc.Post(ctx, tenant, txn, nil)
	var notActive *domain.AccountNotActiveError
	if !errors.As(err, &notActive) {
		t.Fatalf("Post() error = %v, want *domain.AccountNotActiveError", err)
	}
	if notActive.AccountID != credit.ID {
		t.Errorf("AccountNotActiveError.AccountID = %s, want %s", notActive.AccountID, credit.ID)
	}
}

// TestPost_MinBalanceGating covers Task 5.5 (audit A1.5)'s floor: a post
// that would take an account below its min_balance is rejected with
// *domain.MinBalanceBreachError; one that lands exactly at the floor posts;
// an account with no min_balance configured is unconstrained even for a
// large debit.
func TestPost_MinBalanceGating(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	t.Run("breach rejected", func(t *testing.T) {
		t.Parallel()
		tenant := newAccountStatusTenant(t, repo)
		floor := int64(-1000)
		debit := &domain.Account{Name: "Checking", Type: domain.Asset, Currency: "USD", MinBalance: &floor}
		if err := repo.CreateAccount(ctx, tenant, debit); err != nil {
			t.Fatalf("create account: %v", err)
		}
		credit := mustCreateAccount(t, repo, tenant, "USD")

		// Debiting -1500 would take the account to -1500, below the -1000 floor.
		txn := txnOf(t, credit.ID, debit.ID, 1500, "USD")
		_, err := svc.Post(ctx, tenant, txn, nil)
		var breach *domain.MinBalanceBreachError
		if !errors.As(err, &breach) {
			t.Fatalf("Post() error = %v, want *domain.MinBalanceBreachError", err)
		}
		if !errors.Is(err, domain.ErrMinBalanceBreach) {
			t.Errorf("errors.Is(err, ErrMinBalanceBreach) = false, want true")
		}
		if breach.AccountID != debit.ID || breach.MinBalance != floor || breach.NewBalance != -1500 {
			t.Errorf("MinBalanceBreachError = %+v, want AccountID=%s MinBalance=%d NewBalance=-1500", breach, debit.ID, floor)
		}
	})

	t.Run("held exactly passes", func(t *testing.T) {
		t.Parallel()
		tenant := newAccountStatusTenant(t, repo)
		floor := int64(-1000)
		debit := &domain.Account{Name: "Checking", Type: domain.Asset, Currency: "USD", MinBalance: &floor}
		if err := repo.CreateAccount(ctx, tenant, debit); err != nil {
			t.Fatalf("create account: %v", err)
		}
		credit := mustCreateAccount(t, repo, tenant, "USD")

		// Debiting -1000 lands exactly at the floor: allowed, not rejected.
		txn := txnOf(t, credit.ID, debit.ID, 1000, "USD")
		if _, err := svc.Post(ctx, tenant, txn, nil); err != nil {
			t.Fatalf("Post() error = %v, want nil", err)
		}
		bal, err := repo.Balance(ctx, tenant, debit.ID)
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
		if bal.Amount() != -1000 {
			t.Errorf("balance = %d, want -1000", bal.Amount())
		}
	})

	t.Run("no floor unconstrained", func(t *testing.T) {
		t.Parallel()
		tenant := newAccountStatusTenant(t, repo)
		debit := mustCreateAccount(t, repo, tenant, "USD")
		credit := mustCreateAccount(t, repo, tenant, "USD")

		// A large debit with no min_balance configured is never rejected on
		// that basis.
		txn := txnOf(t, credit.ID, debit.ID, 1_000_000, "USD")
		if _, err := svc.Post(ctx, tenant, txn, nil); err != nil {
			t.Fatalf("Post() error = %v, want nil", err)
		}
		bal, err := repo.Balance(ctx, tenant, debit.ID)
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
		if bal.Amount() != -1_000_000 {
			t.Errorf("balance = %d, want -1000000", bal.Amount())
		}
	})
}

// TestConvert_SystemClearingAccountExempt covers the system-account
// exemption (Task 5.5, audit A1.5): the FX clearing accounts are expected to
// carry a permanent, often negative, open position, and must never be
// blocked by status or min_balance even when both are set directly on the
// row (something the public API never lets a caller do, but this proves the
// exemption is unconditional, not merely "nobody happens to configure
// this").
func TestConvert_SystemClearingAccountExempt(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()
	tenant := newAccountStatusTenant(t, repo)

	const (
		base, quote = domain.Currency("USD"), domain.Currency("EUR")
		midE8       = 92_000_000
		spreadBps   = 50
	)
	seedConvertRate(t, pool, quote, midE8, spreadBps)

	usd := newConvertAccount(t, repo, tenant, base)
	eur := newConvertAccount(t, repo, tenant, quote)

	// First conversion creates the clearing accounts (GetOrCreateClearingAccount).
	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: eur.ID, SourceAmount: 10_000}
	if _, _, err := svc.Convert(ctx, tenant, req, &domain.Idempotency{Key: "clearing-exempt-1"}); err != nil {
		t.Fatalf("first Convert() error = %v", err)
	}

	clearingUSD, err := repo.GetOrCreateClearingAccount(ctx, tenant, base)
	if err != nil {
		t.Fatalf("get clearing account: %v", err)
	}
	if !clearingUSD.System {
		t.Fatalf("clearing account System = false, want true")
	}

	// Directly set status=frozen and a min_balance no real balance could
	// ever satisfy, on the system account's own row: neither the REST nor
	// gRPC surface lets a caller do this (both go through
	// AccountService.SetStatus and CreateAccount, never a raw UPDATE), but
	// doing it at the repository layer proves the exemption checks
	// IsSystem, not "this account happens to have no constraints set".
	if err := repo.SetAccountStatus(ctx, tenant, clearingUSD.ID, domain.AccountFrozen); err != nil {
		t.Fatalf("freeze clearing account: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE accounts SET min_balance = 1000000000 WHERE id = $1`, clearingUSD.ID); err != nil {
		t.Fatalf("set clearing account min_balance: %v", err)
	}

	// A second conversion drives the USD clearing account further into its
	// (large, expected) negative open position. It must NOT be blocked by
	// either the frozen status or the impossible min_balance just set.
	if _, _, err := svc.Convert(ctx, tenant, req, &domain.Idempotency{Key: "clearing-exempt-2"}); err != nil {
		t.Fatalf("second Convert() error = %v, want nil (system account must be exempt)", err)
	}
}

// TestConvert_FrozenDestinationBlocked covers the non-exempt side of the
// same feature (Task 5.5, audit A1.5): converting INTO a frozen destination
// account is blocked exactly like an ordinary post would be, rolling back
// the whole four-leg transaction (including the clearing legs).
func TestConvert_FrozenDestinationBlocked(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()
	tenant := newAccountStatusTenant(t, repo)

	const (
		base, quote = domain.Currency("USD"), domain.Currency("EUR")
		midE8       = 92_000_000
		spreadBps   = 50
	)
	seedConvertRate(t, pool, quote, midE8, spreadBps)

	usd := newConvertAccount(t, repo, tenant, base)
	eur := newConvertAccount(t, repo, tenant, quote)
	if err := repo.SetAccountStatus(ctx, tenant, eur.ID, domain.AccountFrozen); err != nil {
		t.Fatalf("freeze destination account: %v", err)
	}

	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: eur.ID, SourceAmount: 10_000}
	_, _, err := svc.Convert(ctx, tenant, req, &domain.Idempotency{Key: "frozen-destination"})
	var notActive *domain.AccountNotActiveError
	if !errors.As(err, &notActive) {
		t.Fatalf("Convert() error = %v, want *domain.AccountNotActiveError", err)
	}
	if notActive.AccountID != eur.ID {
		t.Errorf("AccountNotActiveError.AccountID = %s, want %s", notActive.AccountID, eur.ID)
	}

	// Rolled back atomically: the source account never moved either.
	bal, err := repo.Balance(ctx, tenant, usd.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Amount() != 0 {
		t.Errorf("source balance after rejected convert = %d, want 0", bal.Amount())
	}
}

// TestPost_MinBalanceConcurrentBreach is the concurrency proof Task 5.5
// (audit A1.5) exists for: two concurrent posts that would EACH
// individually keep an account within its floor, but TOGETHER would breach
// it, must resolve to exactly one winner. The account starts at 0 with a
// -1000 floor; each goroutine debits -700 (individually landing at -700,
// comfortably above -1000), but both together would land at -1400. RunInTx's
// SERIALIZABLE isolation plus its own retry loop is what makes this
// deterministic: whichever attempt commits first, the other either sees a
// serialization conflict (retried) or, after retrying, reads the
// already-committed -700 balance and correctly rejects with
// *domain.MinBalanceBreachError, never a stale read that lets both through.
func TestPost_MinBalanceConcurrentBreach(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := newAccountStatusTenant(t, repo)

	floor := int64(-1000)
	debit := &domain.Account{Name: "Checking", Type: domain.Asset, Currency: "USD", MinBalance: &floor}
	if err := repo.CreateAccount(ctx, tenant, debit); err != nil {
		t.Fatalf("create account: %v", err)
	}
	creditA := mustCreateAccount(t, repo, tenant, "USD")
	creditB := mustCreateAccount(t, repo, tenant, "USD")

	const attempts = 2
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	wg.Add(attempts)
	credits := []string{creditA.ID, creditB.ID}
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			txn := txnOf(t, credits[i], debit.ID, 700, "USD")
			_, err := svc.Post(ctx, tenant, txn, nil)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	var successes, breaches int
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrMinBalanceBreach):
			breaches++
		default:
			t.Fatalf("unexpected error from concurrent post: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1 (errs = %v)", successes, errs)
	}
	if breaches != 1 {
		t.Errorf("min-balance breaches = %d, want 1 (errs = %v)", breaches, errs)
	}

	bal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Amount() != -700 {
		t.Errorf("final balance = %d, want -700 (exactly one post landed)", bal.Amount())
	}
}

// TestAccountService_SetStatus covers ledger.AccountService.SetStatus
// directly (Task 5.5, audit A1.5): unlike TestSetAccountStatus
// (internal/postgres), which calls the repository method straight, this
// exercises the service-layer wrapper that composes SetAccountStatus with a
// fresh GetAccount to hand back the updated account, both the success path
// and the not-found path.
func TestAccountService_SetStatus(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	ctx := context.Background()
	tenant := newAccountStatusTenant(t, repo)

	acct := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	if err := accounts.Create(ctx, tenant, acct, nil); err != nil {
		t.Fatalf("create account: %v", err)
	}

	got, err := accounts.SetStatus(ctx, tenant, acct.ID, domain.AccountFrozen)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if got.ID != acct.ID {
		t.Errorf("SetStatus returned account %s, want %s", got.ID, acct.ID)
	}
	if got.Status != domain.AccountFrozen {
		t.Errorf("SetStatus returned status %q, want %q", got.Status, domain.AccountFrozen)
	}

	// Not found: no account with this id exists.
	if _, err := accounts.SetStatus(ctx, tenant, uuid.NewString(), domain.AccountClosed); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("SetStatus(unknown id): err = %v, want ErrAccountNotFound", err)
	}
}

// newConvertService and newConvertAccount are defined in convert_test.go;
// seedConvertRate is defined there too. discardLogger and newTestPool are
// defined in stress_test.go. mustCreateAccount and txnOf are defined in
// policy_test.go. All are reused here rather than redeclared.

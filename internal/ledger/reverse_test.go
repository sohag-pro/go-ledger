package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// newReverseAccounts creates a tenant and a debit/credit account pair for the
// reversal tests, mirroring mkTxn's fixed 250 USD shape.
func newReverseAccounts(t *testing.T, repo *postgres.Repository, tenant string) (debit, credit domain.Account) {
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, tenant, "reverse test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	d := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	c := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, d); err != nil {
		t.Fatalf("create debit account: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, c); err != nil {
		t.Fatalf("create credit account: %v", err)
	}
	return *d, *c
}

// TestReverseTransaction_RestoresBalances posts a transaction, reverses it,
// and checks both accounts' derived balances are back to zero: the defining
// behavior of a reversal (Task 4.2, audit A1.2). It also checks the reversal
// links back to the original and the original itself is untouched (postings
// are append-only, ADR-001: the original's own postings never change).
func TestReverseTransaction_RestoresBalances(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReverseAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-restore-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}

	debitBalAfterPost, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance after post: %v", err)
	}
	if debitBalAfterPost.Amount() != 250 {
		t.Fatalf("debit balance after post = %d, want 250", debitBalAfterPost.Amount())
	}

	reversal, alreadyReversed, err := svc.ReverseTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("ReverseTransaction() error = %v", err)
	}
	if alreadyReversed {
		t.Error("alreadyReversed = true on the first reversal, want false")
	}
	if reversal.ID == original.ID {
		t.Fatal("reversal has the same id as the original, want a distinct new transaction")
	}
	if reversal.ReversesTransactionID == nil || *reversal.ReversesTransactionID != original.ID {
		t.Errorf("ReversesTransactionID = %v, want pointer to %q", reversal.ReversesTransactionID, original.ID)
	}
	if len(reversal.Postings) != len(original.Postings) {
		t.Fatalf("reversal postings = %d, want %d", len(reversal.Postings), len(original.Postings))
	}

	// Both accounts' balances must be back to zero: the reversal's negated
	// legs exactly cancel the original's.
	debitBal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance after reversal: %v", err)
	}
	if debitBal.Amount() != 0 {
		t.Errorf("debit balance after reversal = %d, want 0", debitBal.Amount())
	}
	creditBal, err := repo.Balance(ctx, tenant, credit.ID)
	if err != nil {
		t.Fatalf("credit balance after reversal: %v", err)
	}
	if creditBal.Amount() != 0 {
		t.Errorf("credit balance after reversal = %d, want 0", creditBal.Amount())
	}

	// The original's own postings are untouched: re-reading it must still
	// show the original amounts, not the reversal's negated ones.
	reread, err := repo.GetTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("re-read original: %v", err)
	}
	for _, p := range reread.Postings {
		if p.AccountID == debit.ID && p.Amount.Amount() != 250 {
			t.Errorf("original debit posting amount = %d, want 250 (unchanged)", p.Amount.Amount())
		}
		if p.AccountID == credit.ID && p.Amount.Amount() != -250 {
			t.Errorf("original credit posting amount = %d, want -250 (unchanged)", p.Amount.Amount())
		}
	}
	if reread.ReversesTransactionID != nil {
		t.Errorf("original ReversesTransactionID = %v, want nil (the original is not itself a reversal)", reread.ReversesTransactionID)
	}
}

// TestReverseTransaction_MultiCurrency reverses a convert-shaped transaction
// (four legs across two currencies) and checks every account, including both
// clearing accounts, nets back to its pre-convert balance: negation
// preserves the per-currency zero sum (ADR-014) regardless of how many
// currencies are involved.
func TestReverseTransaction_MultiCurrency(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	const quote = domain.Currency("EUR")
	seedConvertRate(t, pool, quote, 92_000_000, 50)
	usd := newConvertAccount(t, repo, tenant, "USD")
	eur := newConvertAccount(t, repo, tenant, quote)

	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: eur.ID, SourceAmount: 10_000}
	converted, _, err := svc.Convert(ctx, tenant, req, &domain.Idempotency{Key: "reverse-convert-1"})
	if err != nil {
		t.Fatalf("Convert() error = %v", err)
	}

	clearingUSD, err := repo.GetOrCreateClearingAccount(ctx, tenant, "USD")
	if err != nil {
		t.Fatalf("get clearing USD: %v", err)
	}
	clearingEUR, err := repo.GetOrCreateClearingAccount(ctx, tenant, quote)
	if err != nil {
		t.Fatalf("get clearing EUR: %v", err)
	}

	reversal, alreadyReversed, err := svc.ReverseTransaction(ctx, tenant, converted.ID)
	if err != nil {
		t.Fatalf("ReverseTransaction() error = %v", err)
	}
	if alreadyReversed {
		t.Error("alreadyReversed = true on the first reversal, want false")
	}
	if len(reversal.Postings) != 4 {
		t.Fatalf("reversal postings = %d, want 4", len(reversal.Postings))
	}

	for _, acct := range []domain.Account{usd, eur, clearingUSD, clearingEUR} {
		bal, err := repo.Balance(ctx, tenant, acct.ID)
		if err != nil {
			t.Fatalf("balance %s: %v", acct.ID, err)
		}
		if bal.Amount() != 0 {
			t.Errorf("account %s (%s) balance after reversal = %d, want 0", acct.ID, acct.Currency, bal.Amount())
		}
	}
}

// TestReverseTransaction_Idempotent checks that reversing the same
// transaction twice returns the SAME reversal both times, with
// alreadyReversed = false then true, and never writes a second reversal row.
func TestReverseTransaction_Idempotent(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReverseAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-idem-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}

	first, alreadyReversed, err := svc.ReverseTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("first ReverseTransaction() error = %v", err)
	}
	if alreadyReversed {
		t.Error("first call: alreadyReversed = true, want false")
	}

	second, alreadyReversed, err := svc.ReverseTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("second ReverseTransaction() error = %v", err)
	}
	if !alreadyReversed {
		t.Error("second call: alreadyReversed = false, want true")
	}
	if second.ID != first.ID {
		t.Errorf("second reversal id = %s, want %s (the same reversal)", second.ID, first.ID)
	}

	// Draining the chainer and reading the audit trail directly confirms
	// exactly one reversal was ever posted: the second call wrote nothing.
	drainChainer(t, pool, tenant)
	entries, err := repo.ListAuditByTransaction(ctx, tenant, first.ID)
	if err != nil {
		t.Fatalf("list audit for reversal: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("audit rows for reversal = %d, want 1 (no second reversal posted)", len(entries))
	}
}

// TestReverseTransaction_CannotReverseAReversal posts a transaction, reverses
// it once, then tries to reverse the reversal itself: this must be rejected
// with domain.ErrCannotReverseReversal, not silently accepted as a
// double-negation back to the original's shape.
func TestReverseTransaction_CannotReverseAReversal(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReverseAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-of-reversal-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}
	reversal, _, err := svc.ReverseTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("reverse original: %v", err)
	}

	if _, _, err := svc.ReverseTransaction(ctx, tenant, reversal.ID); !errors.Is(err, domain.ErrCannotReverseReversal) {
		t.Errorf("ReverseTransaction() on a reversal: err = %v, want ErrCannotReverseReversal", err)
	}
}

// TestReverseTransaction_NotFound checks the not-found path: reversing an id
// that names no transaction at all.
func TestReverseTransaction_NotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "reverse not found tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	if _, _, err := svc.ReverseTransaction(ctx, tenant, uuid.NewString()); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("ReverseTransaction() on unknown id: err = %v, want ErrTransactionNotFound", err)
	}
}

// TestReverseTransaction_ConcurrentDoubleReverseYieldsOne fires many
// concurrent ReverseTransaction calls at the same original. The idempotency
// precheck (GetReversalOf) runs before RunInTx, so more than one goroutine
// can miss it and proceed toward a real reversal; only one wins the
// database's transactions_one_reversal_idx unique index, and every other one
// must observe domain.ErrTransactionAlreadyReversed inside RunInTx and read
// back the winner's reversal instead of posting a second one. This is the
// same hammer pattern TestConvert_ConcurrentIdempotentHammer uses for
// Convert's idempotency key, applied to the unique-index race guard instead.
func TestReverseTransaction_ConcurrentDoubleReverseYieldsOne(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReverseAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-hammer-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}

	const n = 25
	var wg sync.WaitGroup
	ids := make([]string, n)
	already := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rev, alreadyReversed, err := svc.ReverseTransaction(ctx, tenant, original.ID)
			if rev != nil {
				ids[i] = rev.ID
			}
			already[i], errs[i] = alreadyReversed, err
		}(i)
	}
	wg.Wait()

	var first string
	alreadyCount := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if first == "" {
			first = ids[i]
		} else if ids[i] != first {
			t.Fatalf("call %d returned reversal id %s, want %s", i, ids[i], first)
		}
		if already[i] {
			alreadyCount++
		}
	}
	if alreadyCount != n-1 {
		t.Errorf("alreadyReversed count = %d, want %d (exactly one real reversal)", alreadyCount, n-1)
	}

	// Balances must reflect exactly one reversal having landed, not a
	// double reversal (which would leave the accounts at -250/+250 again).
	debitBal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance: %v", err)
	}
	if debitBal.Amount() != 0 {
		t.Errorf("debit balance = %d, want 0 (exactly one reversal)", debitBal.Amount())
	}

	drainChainer(t, pool, tenant)
	entries, err := repo.ListAuditByTransaction(ctx, tenant, first)
	if err != nil {
		t.Fatalf("list audit for reversal: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("audit rows for the reversal = %d, want 1 (no double reversal under concurrency)", len(entries))
	}
}

// TestReverseTransaction_AuditChainIncludesReversal drains the chainer after
// a reversal and checks the resulting audit_log row carries
// domain.ActionTransactionReversed and the original transaction's id in its
// snapshot: the reversal is a first-class, chained audit event, not just a
// side effect invisible to the audit trail.
func TestReverseTransaction_AuditChainIncludesReversal(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReverseAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-audit-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}
	reversal, _, err := svc.ReverseTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("ReverseTransaction() error = %v", err)
	}

	// Reverse only writes an audit_outbox row (ADR-017); drain the chainer so
	// there is a chained audit_log row to check.
	drainChainer(t, pool, tenant)

	entries, err := repo.ListAuditByTransaction(ctx, tenant, reversal.ID)
	if err != nil {
		t.Fatalf("list audit for reversal: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit rows for reversal = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Action != domain.ActionTransactionReversed {
		t.Errorf("action = %q, want %q", entry.Action, domain.ActionTransactionReversed)
	}
	if entry.RowHash == "" {
		t.Error("row hash is empty after the chainer ran, want a computed hash")
	}
	var snapshot struct {
		ID                    string `json:"id"`
		ReversesTransactionID string `json:"reverses_transaction_id"`
	}
	if err := json.Unmarshal(entry.After, &snapshot); err != nil {
		t.Fatalf("unmarshal audit snapshot: %v", err)
	}
	if snapshot.ID != reversal.ID {
		t.Errorf("audit snapshot id = %q, want the reversal's own id %q", snapshot.ID, reversal.ID)
	}
	// The reversal's audit snapshot must be self-contained: it names the
	// original transaction it reverses, so an auditor reading this row alone
	// (without joining back to the transactions table) can tell what was
	// undone.
	if snapshot.ReversesTransactionID != original.ID {
		t.Errorf("audit snapshot reverses_transaction_id = %q, want the original transaction's id %q",
			snapshot.ReversesTransactionID, original.ID)
	}

	// Verify still walks clean end to end with the reversal's audit row in
	// the chain.
	audits := ledger.NewAuditService(repo)
	result, err := audits.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid {
		t.Errorf("chain valid = false after a reversal, want true")
	}
}

// reverseScreeningCounts snapshots the transactions/postings/audit_outbox row
// counts for tenant, so a screening test can assert a rejected or
// infra-failed reversal attempt leaves them EXACTLY where they were
// (including the original post's own rows), rather than requiring all-zero
// the way screening_test.go's assertNoRowsForTenant does for a fresh tenant
// with no prior post.
func reverseScreeningCounts(t *testing.T, pool *pgxpool.Pool, tenant string) map[string]int {
	t.Helper()
	ctx := context.Background()
	tid, err := uuid.Parse(tenant)
	if err != nil {
		t.Fatalf("parse tenant id: %v", err)
	}
	counts := make(map[string]int)
	for _, table := range []string{"transactions", "postings", "audit_outbox"} {
		var count int
		if err := pool.QueryRow(ctx, "select count(*) from "+table+" where tenant_id = $1", tid).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

// TestReverseTransaction_ScreeningRejectWritesNothing closes the Task 6.1
// compliance gap (audit A9.1 follow-up): ReverseTransaction posts real money
// (the negated legs plus its own audit row) exactly like Post and Convert, so
// a party a screening system would block must not be able to receive funds
// through a reversal either. An explicit veto (*ledger.ScreeningRejectedError)
// must block the reversal and leave NOTHING new persisted: not the reversal's
// transaction, postings, or audit_outbox row, and the original transaction
// must be untouched.
func TestReverseTransaction_ScreeningRejectWritesNothing(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	hook := &stubPrePostHook{}
	svc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithPrePostHook(hook))
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReverseAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-screen-reject-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("hook.calls after posting the original = %d, want 1", len(hook.calls))
	}

	before := reverseScreeningCounts(t, pool, tenant)

	// Switch the hook to reject, then attempt the reversal.
	hook.err = &ledger.ScreeningRejectedError{Reason: "sanctions list match"}
	_, _, err := svc.ReverseTransaction(ctx, tenant, original.ID)

	if !errors.Is(err, ledger.ErrScreeningRejected) {
		t.Fatalf("ReverseTransaction() error = %v, want errors.Is match on ledger.ErrScreeningRejected", err)
	}
	var rejected *ledger.ScreeningRejectedError
	if !errors.As(err, &rejected) || rejected.Reason != "sanctions list match" {
		t.Fatalf("ReverseTransaction() error = %v, want *ledger.ScreeningRejectedError{Reason: %q}", err, "sanctions list match")
	}
	if len(hook.calls) != 2 || hook.calls[1] != tenant {
		t.Errorf("hook.calls = %v, want a second call for tenant %s (the reversal attempt)", hook.calls, tenant)
	}

	after := reverseScreeningCounts(t, pool, tenant)
	for table, want := range before {
		if got := after[table]; got != want {
			t.Errorf("%s row count after rejected reversal = %d, want %d (unchanged)", table, got, want)
		}
	}

	// The original transaction's own postings must be untouched: postings are
	// append-only, and a rejected reversal must not have mutated them.
	reread, err := repo.GetTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("re-read original: %v", err)
	}
	for _, p := range reread.Postings {
		if p.AccountID == debit.ID && p.Amount.Amount() != 250 {
			t.Errorf("original debit posting amount = %d, want 250 (unchanged)", p.Amount.Amount())
		}
		if p.AccountID == credit.ID && p.Amount.Amount() != -250 {
			t.Errorf("original credit posting amount = %d, want -250 (unchanged)", p.Amount.Amount())
		}
	}
	debitBal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance: %v", err)
	}
	if debitBal.Amount() != 250 {
		t.Errorf("debit balance after rejected reversal = %d, want 250 (the original's effect, unreversed)", debitBal.Amount())
	}
}

// TestReverseTransaction_ScreeningInfraErrorWritesNothing is the reversal
// counterpart of TestPost_ScreeningInfraErrorWritesNothing: a hook failure
// that is NOT an explicit veto (an "infrastructure" error) also blocks the
// reversal, fails closed (mapping to ledger.ErrScreeningUnavailable), and
// writes nothing.
func TestReverseTransaction_ScreeningInfraErrorWritesNothing(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	underlying := errors.New("screening service: connection refused")
	hook := &stubPrePostHook{err: underlying}
	svc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithPrePostHook(hook))
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReverseAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	// Post the original with the noop-friendly hook temporarily disabled by
	// posting through a plain service, so the original's own post is not
	// itself affected by the infra-error hook under test.
	plainSvc := ledger.NewTransactionService(repo, discardLogger(), nil)
	if _, err := plainSvc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-screen-infra-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}

	before := reverseScreeningCounts(t, pool, tenant)

	_, _, err := svc.ReverseTransaction(ctx, tenant, original.ID)

	if !errors.Is(err, ledger.ErrScreeningUnavailable) {
		t.Fatalf("ReverseTransaction() error = %v, want errors.Is match on ledger.ErrScreeningUnavailable", err)
	}
	if errors.Is(err, ledger.ErrScreeningRejected) {
		t.Errorf("ReverseTransaction() error = %v, must NOT match ledger.ErrScreeningRejected (this is an infra failure, not an explicit veto)", err)
	}
	if !errors.Is(err, underlying) {
		t.Errorf("ReverseTransaction() error = %v, want the underlying hook error still reachable via errors.Is", err)
	}
	if len(hook.calls) != 1 || hook.calls[0] != tenant {
		t.Errorf("hook.calls = %v, want exactly one call for tenant %s", hook.calls, tenant)
	}

	after := reverseScreeningCounts(t, pool, tenant)
	for table, want := range before {
		if got := after[table]; got != want {
			t.Errorf("%s row count after infra-failed reversal = %d, want %d (unchanged)", table, got, want)
		}
	}
}

// TestReverseTransaction_DefaultNoopHookAllows checks that reversal behaves
// exactly as it did before Task 6.1's screening hook existed when the service
// is wired with the explicit no-op hook, mirroring
// TestPost_ExplicitNoopHookAllows: the reversal succeeds and balances net to
// zero, same as TestReverseTransaction_RestoresBalances.
func TestReverseTransaction_DefaultNoopHookAllows(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithPrePostHook(ledger.NoopPrePostHook{}))
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReverseAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-noop-hook-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}

	reversal, alreadyReversed, err := svc.ReverseTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("ReverseTransaction() error = %v, want nil (no-op hook allows everything)", err)
	}
	if alreadyReversed {
		t.Error("alreadyReversed = true, want false")
	}
	if reversal.ReversesTransactionID == nil || *reversal.ReversesTransactionID != original.ID {
		t.Errorf("ReversesTransactionID = %v, want pointer to %q", reversal.ReversesTransactionID, original.ID)
	}

	debitBal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance: %v", err)
	}
	if debitBal.Amount() != 0 {
		t.Errorf("debit balance after reversal = %d, want 0", debitBal.Amount())
	}
}

// TestReverseTransaction_ScreeningNotCalledOnReplay is the reversal
// counterpart of TestPost_ScreeningNotCalledOnReplay: the idempotent
// "already reversed" replay path (a second ReverseTransaction call for an
// original that already has one) posts nothing new, so it must not call the
// hook again.
func TestReverseTransaction_ScreeningNotCalledOnReplay(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	hook := &stubPrePostHook{}
	svc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithPrePostHook(hook))
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReverseAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-screen-replay-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("hook.calls after posting the original = %d, want 1", len(hook.calls))
	}

	first, alreadyReversed, err := svc.ReverseTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("first ReverseTransaction() error = %v", err)
	}
	if alreadyReversed {
		t.Error("first call: alreadyReversed = true, want false")
	}
	if len(hook.calls) != 2 {
		t.Fatalf("hook.calls after first reversal = %d, want 2 (one for the post, one for the reversal)", len(hook.calls))
	}

	second, alreadyReversed, err := svc.ReverseTransaction(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("second ReverseTransaction() error = %v", err)
	}
	if !alreadyReversed {
		t.Error("second call: alreadyReversed = false, want true")
	}
	if second.ID != first.ID {
		t.Errorf("second reversal id = %s, want %s (the same reversal)", second.ID, first.ID)
	}
	if len(hook.calls) != 2 {
		t.Errorf("hook.calls after the already-reversed replay = %d, want still 2 (the replay must not call the hook again)", len(hook.calls))
	}
}

package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// stubPrePostHook is a test double for ledger.PrePostHook (Task 6.1, audit
// A9.1): it always returns err (nil, a *ledger.ScreeningRejectedError, or a
// plain "infrastructure" error), and records every tenant/transaction it was
// asked to review so a test can assert it was (or was not) called.
type stubPrePostHook struct {
	err   error
	calls []string // tenant ids ReviewPost was called with
}

func (h *stubPrePostHook) ReviewPost(_ context.Context, tenantID string, _ *domain.Transaction) error {
	h.calls = append(h.calls, tenantID)
	return h.err
}

// newScreeningTenant creates a fresh tenant with two USD accounts, enough to
// post (or attempt to post) a balanced two-leg transaction.
func newScreeningTenant(t *testing.T, repo *postgres.Repository) (tenant string, debit, credit domain.Account) {
	t.Helper()
	ctx := context.Background()
	tenant = uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "screening test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	debit = mustCreateAccount(t, repo, tenant, "USD")
	credit = mustCreateAccount(t, repo, tenant, "USD")
	return tenant, debit, credit
}

// assertNoRowsForTenant fails the test unless transactions, postings, and
// audit_outbox all have zero rows for tenant: the core guarantee Task 6.1
// (audit A9.1) requires of a rejected or failed screening call, on both Post
// and Convert.
func assertNoRowsForTenant(t *testing.T, pool *pgxpool.Pool, tenant string) {
	t.Helper()
	ctx := context.Background()
	tid, err := uuid.Parse(tenant)
	if err != nil {
		t.Fatalf("parse tenant id: %v", err)
	}
	for _, table := range []string{"transactions", "postings", "audit_outbox"} {
		var count int
		if err := pool.QueryRow(ctx, "select count(*) from "+table+" where tenant_id = $1", tid).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s rows for tenant %s = %d, want 0 (screening should reject before any write)", table, tenant, count)
		}
	}
}

// TestPost_DefaultHookIsNoop covers the default-unchanged requirement (Task
// 6.1, audit A9.1): a TransactionService built without WithPrePostHook posts
// exactly as it did before this hook existed.
func TestPost_DefaultHookIsNoop(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant, debit, credit := newScreeningTenant(t, repo)

	txn := txnOf(t, debit.ID, credit.ID, 1000, "USD")
	if _, err := svc.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("Post() error = %v, want nil (default hook is a no-op)", err)
	}
	bal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Amount() != 1000 {
		t.Errorf("balance = %d, want 1000", bal.Amount())
	}
}

// TestPost_ExplicitNoopHookAllows covers the same default-unchanged
// guarantee, but wired explicitly via WithPrePostHook(NoopPrePostHook{}),
// the way cmd/server wires it: still a no-op.
func TestPost_ExplicitNoopHookAllows(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithPrePostHook(ledger.NoopPrePostHook{}))
	ctx := context.Background()
	tenant, debit, credit := newScreeningTenant(t, repo)

	txn := txnOf(t, debit.ID, credit.ID, 1000, "USD")
	if _, err := svc.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("Post() error = %v, want nil", err)
	}
}

// TestPost_ScreeningRejectWritesNothing is the critical test for Task 6.1
// (audit A9.1): a hook that explicitly rejects (*ledger.ScreeningRejectedError,
// wrapping ledger.ErrScreeningRejected) blocks the post, and NOTHING is
// persisted: no transaction, posting, or audit_outbox row for the tenant.
func TestPost_ScreeningRejectWritesNothing(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	hook := &stubPrePostHook{err: &ledger.ScreeningRejectedError{Reason: "sanctions list match"}}
	svc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithPrePostHook(hook))
	ctx := context.Background()
	tenant, debit, credit := newScreeningTenant(t, repo)

	txn := txnOf(t, debit.ID, credit.ID, 1000, "USD")
	_, err := svc.Post(ctx, tenant, txn, nil)

	if !errors.Is(err, ledger.ErrScreeningRejected) {
		t.Fatalf("Post() error = %v, want errors.Is match on ledger.ErrScreeningRejected", err)
	}
	var rejected *ledger.ScreeningRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Post() error = %v, want *ledger.ScreeningRejectedError", err)
	}
	if rejected.Reason != "sanctions list match" {
		t.Errorf("ScreeningRejectedError.Reason = %q, want %q", rejected.Reason, "sanctions list match")
	}
	if len(hook.calls) != 1 || hook.calls[0] != tenant {
		t.Errorf("hook.calls = %v, want exactly one call for tenant %s", hook.calls, tenant)
	}
	assertNoRowsForTenant(t, pool, tenant)

	// Also confirm the account balance never moved: no partial effect either.
	bal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Amount() != 0 {
		t.Errorf("balance after rejected post = %d, want 0", bal.Amount())
	}
}

// TestPost_ScreeningInfraErrorWritesNothing covers the fail-closed half of
// Task 6.1 (audit A9.1): a hook that fails for a reason OTHER than an
// explicit reject (an "infrastructure" error, not ErrScreeningRejected) also
// blocks the post, mapping to ledger.ErrScreeningUnavailable, and likewise
// writes nothing. An ambiguous "we don't know" must never be treated as an
// implicit allow.
func TestPost_ScreeningInfraErrorWritesNothing(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	underlying := errors.New("screening service: connection refused")
	hook := &stubPrePostHook{err: underlying}
	svc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithPrePostHook(hook))
	ctx := context.Background()
	tenant, debit, credit := newScreeningTenant(t, repo)

	txn := txnOf(t, debit.ID, credit.ID, 1000, "USD")
	_, err := svc.Post(ctx, tenant, txn, nil)

	if !errors.Is(err, ledger.ErrScreeningUnavailable) {
		t.Fatalf("Post() error = %v, want errors.Is match on ledger.ErrScreeningUnavailable", err)
	}
	if errors.Is(err, ledger.ErrScreeningRejected) {
		t.Errorf("Post() error = %v, must NOT match ledger.ErrScreeningRejected (this is an infra failure, not an explicit veto)", err)
	}
	if !errors.Is(err, underlying) {
		t.Errorf("Post() error = %v, want the underlying hook error still reachable via errors.Is", err)
	}
	assertNoRowsForTenant(t, pool, tenant)
}

// TestPost_ScreeningNotCalledOnReplay covers the idempotency interaction
// (Task 6.1, audit A9.1): once a post has been screened and committed, a
// retry under the SAME idempotency key replays the stored transaction
// without calling the hook again. Re-screening an already-approved,
// already-posted transaction would serve no purpose (nothing new is being
// written) and would let screening non-determinism turn a successful post
// into a spurious rejection on retry.
func TestPost_ScreeningNotCalledOnReplay(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	hook := &stubPrePostHook{}
	svc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithPrePostHook(hook))
	ctx := context.Background()
	tenant, debit, credit := newScreeningTenant(t, repo)

	idem := &domain.Idempotency{Key: "screening-replay-1"}
	txn := txnOf(t, debit.ID, credit.ID, 1000, "USD")
	if _, err := svc.Post(ctx, tenant, txn, idem); err != nil {
		t.Fatalf("first Post() error = %v, want nil", err)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("hook.calls after first post = %d, want 1", len(hook.calls))
	}

	replay := txnOf(t, debit.ID, credit.ID, 1000, "USD")
	replayed, err := svc.Post(ctx, tenant, replay, idem)
	if err != nil {
		t.Fatalf("replay Post() error = %v, want nil", err)
	}
	if !replayed {
		t.Errorf("replayed = false, want true")
	}
	if len(hook.calls) != 1 {
		t.Errorf("hook.calls after replay = %d, want still 1 (hook must not be called again)", len(hook.calls))
	}
}

// newScreeningConvertService returns a TransactionService wired with both a
// real fx.Provider (required for Convert, see ErrNoFXProvider) and hook.
func newScreeningConvertService(pool *pgxpool.Pool, hook ledger.PrePostHook) *ledger.TransactionService {
	repo := postgres.NewRepository(pool)
	return ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithFXProvider(fx.NewDBProvider(pool)),
		ledger.WithPrePostHook(hook))
}

// TestConvert_ScreeningRejectWritesNothing is Convert's counterpart to
// TestPost_ScreeningRejectWritesNothing (Task 6.1, audit A9.1): an explicit
// veto blocks the four-leg convert transaction and writes nothing.
func TestConvert_ScreeningRejectWritesNothing(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	hook := &stubPrePostHook{err: &ledger.ScreeningRejectedError{Reason: "velocity limit exceeded"}}
	svc := newScreeningConvertService(pool, hook)
	ctx := context.Background()
	tenant := uuid.NewString()

	const (
		quote     = domain.Currency("EUR")
		midE8     = 92_000_000
		spreadBps = 50
	)
	seedConvertRate(t, pool, quote, midE8, spreadBps)
	usd := newConvertAccount(t, repo, tenant, "USD")
	eur := newConvertAccount(t, repo, tenant, quote)

	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: eur.ID, SourceAmount: 10_000}
	_, _, err := svc.Convert(ctx, tenant, req, nil)

	if !errors.Is(err, ledger.ErrScreeningRejected) {
		t.Fatalf("Convert() error = %v, want errors.Is match on ledger.ErrScreeningRejected", err)
	}
	var rejected *ledger.ScreeningRejectedError
	if !errors.As(err, &rejected) || rejected.Reason != "velocity limit exceeded" {
		t.Fatalf("Convert() error = %v, want *ledger.ScreeningRejectedError{Reason: %q}", err, "velocity limit exceeded")
	}
	assertNoRowsForTenant(t, pool, tenant)

	bal, err := repo.Balance(ctx, tenant, usd.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Amount() != 0 {
		t.Errorf("source balance after rejected convert = %d, want 0", bal.Amount())
	}
}

// TestConvert_ScreeningInfraErrorWritesNothing is Convert's counterpart to
// TestPost_ScreeningInfraErrorWritesNothing (Task 6.1, audit A9.1): an
// ambiguous (non-veto) hook failure also blocks the convert and writes
// nothing, mapping to ledger.ErrScreeningUnavailable.
func TestConvert_ScreeningInfraErrorWritesNothing(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	underlying := errors.New("screening service: timeout")
	hook := &stubPrePostHook{err: underlying}
	svc := newScreeningConvertService(pool, hook)
	ctx := context.Background()
	tenant := uuid.NewString()

	const (
		quote     = domain.Currency("GBP")
		midE8     = 79_000_000
		spreadBps = 25
	)
	seedConvertRate(t, pool, quote, midE8, spreadBps)
	usd := newConvertAccount(t, repo, tenant, "USD")
	gbp := newConvertAccount(t, repo, tenant, quote)

	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: gbp.ID, SourceAmount: 10_000}
	_, _, err := svc.Convert(ctx, tenant, req, nil)

	if !errors.Is(err, ledger.ErrScreeningUnavailable) {
		t.Fatalf("Convert() error = %v, want errors.Is match on ledger.ErrScreeningUnavailable", err)
	}
	if errors.Is(err, ledger.ErrScreeningRejected) {
		t.Errorf("Convert() error = %v, must NOT match ledger.ErrScreeningRejected", err)
	}
	assertNoRowsForTenant(t, pool, tenant)
}

// TestConvert_ScreeningNotCalledOnReplay is Convert's counterpart to
// TestPost_ScreeningNotCalledOnReplay (Task 6.1, audit A9.1): a retried
// convert under the same idempotency key replays without re-screening.
func TestConvert_ScreeningNotCalledOnReplay(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	hook := &stubPrePostHook{}
	svc := newScreeningConvertService(pool, hook)
	ctx := context.Background()
	tenant := uuid.NewString()

	const (
		quote     = domain.Currency("JPY")
		midE8     = 150_000_000
		spreadBps = 10
	)
	seedConvertRate(t, pool, quote, midE8, spreadBps)
	usd := newConvertAccount(t, repo, tenant, "USD")
	jpy := newConvertAccount(t, repo, tenant, quote)

	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: jpy.ID, SourceAmount: 10_000}
	idem := &domain.Idempotency{Key: "screening-convert-replay-1"}
	if _, _, err := svc.Convert(ctx, tenant, req, idem); err != nil {
		t.Fatalf("first Convert() error = %v, want nil", err)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("hook.calls after first convert = %d, want 1", len(hook.calls))
	}

	_, replayed, err := svc.Convert(ctx, tenant, req, idem)
	if err != nil {
		t.Fatalf("replay Convert() error = %v, want nil", err)
	}
	if !replayed {
		t.Errorf("replayed = false, want true")
	}
	if len(hook.calls) != 1 {
		t.Errorf("hook.calls after replay = %d, want still 1", len(hook.calls))
	}
}

// TestScreeningRejectedError_Error covers both branches of
// (*ledger.ScreeningRejectedError).Error(): a reason present is appended
// after the generic message, and an empty reason falls back to the generic
// message alone, unlike a caller forgetting to set one and getting an
// empty-looking error string.
func TestScreeningRejectedError_Error(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{"with reason", "sanctions list match", "ledger: screening rejected post: sanctions list match"},
		{"empty reason", "", "ledger: screening rejected post"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := &ledger.ScreeningRejectedError{Reason: tt.reason}
			if got := err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
			if !errors.Is(err, ledger.ErrScreeningRejected) {
				t.Error("errors.Is(err, ErrScreeningRejected) = false, want true (Unwrap must match regardless of Reason)")
			}
		})
	}
}

package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// unreachableFXProvider is an fx.Provider that records whether Rate was ever
// called. Convert must resolve idempotency (and fail closed on an unknown
// stored scheme) before it ever needs a rate, so a passing test also proves
// Rate was never reached; WithFXProvider still needs a non-nil provider, or
// Convert returns ErrNoFXProvider before getting anywhere near the
// idempotency check.
type unreachableFXProvider struct {
	called bool
}

func (p *unreachableFXProvider) Rate(_ context.Context, _ string, _, _ domain.Currency) (domain.FXQuote, int32, error) {
	p.called = true
	return domain.FXQuote{}, 0, nil
}

// fakeSchemeRepo is a minimal domain.Repository test double for exercising
// the fail-closed unknown-fingerprint-scheme path (Task 2.3, audit A1.6)
// without a database. It embeds a nil domain.Repository so it satisfies the
// full interface by delegation; every method actually exercised by the tests
// in this file is overridden below, and any method this test does NOT expect
// to be called is left to the nil embed, which panics if invoked. That
// panic is deliberate: it turns "the fail-closed path secretly proceeded to
// touch the repository further" into a hard test failure instead of a silent
// pass.
type fakeSchemeRepo struct {
	domain.Repository

	idemRecord domain.IdempotencyRecord
	runInTxErr error

	getTransactionCalled bool
}

func (f *fakeSchemeRepo) GetIdempotencyKey(_ context.Context, _, _ string) (domain.IdempotencyRecord, error) {
	return f.idemRecord, nil
}

func (f *fakeSchemeRepo) RunInTx(_ context.Context, _ string, _ func(context.Context, domain.Tx) error) error {
	// The real adapter would run fn (CreateTransaction, InsertIdempotencyKey,
	// AppendAudit) inside a database transaction. This fake never calls fn at
	// all: every test in this file drives the duplicate-key replay branch
	// directly, so it needs RunInTx to report exactly the error the real
	// adapter would report once the idempotency key's primary key collides.
	return f.runInTxErr
}

func (f *fakeSchemeRepo) GetTransaction(_ context.Context, _, _ string) (domain.Transaction, error) {
	f.getTransactionCalled = true
	return domain.Transaction{}, nil
}

func mkFingerprintTxn() *domain.Transaction {
	d, _ := domain.NewMoney(100, "USD")
	c, _ := domain.NewMoney(-100, "USD")
	return &domain.Transaction{Postings: []domain.Posting{
		{AccountID: "acct-debit", Amount: d},
		{AccountID: "acct-credit", Amount: c},
	}}
}

// TestPostReplayUnknownSchemeFailsClosed proves that when a stored
// idempotency record carries a fingerprint scheme this binary does not know
// how to compute (domain.TransactionFingerprint returns ok=false), Post's
// replay path fails closed with ErrIdempotencyConflict instead of ever
// loading and replaying the stored transaction. This is the scenario a
// downgrade produces: a newer binary wrote the key under a scheme this older
// binary cannot recompute, so it cannot verify the retried body matches, and
// must refuse rather than guess.
func TestPostReplayUnknownSchemeFailsClosed(t *testing.T) {
	t.Parallel()
	repo := &fakeSchemeRepo{
		idemRecord: domain.IdempotencyRecord{
			Key:           "dup-key",
			Fingerprint:   "does-not-matter",
			Scheme:        "v99",
			TransactionID: "some-transaction-id",
		},
		runInTxErr: domain.ErrDuplicateIdempotencyKey,
	}
	svc := ledger.NewTransactionService(repo, nil, nil)

	txn := mkFingerprintTxn()
	replayed, err := svc.Post(context.Background(), "tenant-1", txn, &domain.Idempotency{Key: "dup-key"})

	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("Post with unknown stored scheme: err = %v, want ErrIdempotencyConflict", err)
	}
	if replayed {
		t.Error("Post with unknown stored scheme: replayed = true, want false")
	}
	if repo.getTransactionCalled {
		t.Error("Post with unknown stored scheme: GetTransaction was called, want it never reached (fail closed before replay)")
	}
}

// TestConvertReplayUnknownSchemeFailsClosed is the Convert-path counterpart
// of TestPostReplayUnknownSchemeFailsClosed: a stored convert idempotency
// record under an unrecognized scheme must fail closed rather than replay.
// Convert resolves idempotency from the request BEFORE calling the rate
// provider or looking up either account (see convert.go), so a repo that
// only implements GetIdempotencyKey and GetTransaction is enough here: any
// call to GetAccount, GetOrCreateClearingAccount, or the fx provider would
// mean the fail-closed short-circuit did not actually happen before the real
// conversion machinery, and would panic on the fake's nil embed.
func TestConvertReplayUnknownSchemeFailsClosed(t *testing.T) {
	t.Parallel()
	repo := &fakeSchemeRepo{
		idemRecord: domain.IdempotencyRecord{
			Key:           "dup-convert-key",
			Fingerprint:   "does-not-matter",
			Scheme:        "v99",
			TransactionID: "some-transaction-id",
		},
	}
	provider := &unreachableFXProvider{}
	svc := ledger.NewTransactionService(repo, nil, nil, ledger.WithFXProvider(provider))

	req := ledger.ConvertRequest{FromAccountID: "acct-from", ToAccountID: "acct-to", SourceAmount: 500}
	txn, replayed, err := svc.Convert(context.Background(), "tenant-1", req, &domain.Idempotency{Key: "dup-convert-key"})

	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("Convert with unknown stored scheme: err = %v, want ErrIdempotencyConflict", err)
	}
	if replayed {
		t.Error("Convert with unknown stored scheme: replayed = true, want false")
	}
	if txn != nil {
		t.Error("Convert with unknown stored scheme: transaction non-nil, want nil")
	}
	if repo.getTransactionCalled {
		t.Error("Convert with unknown stored scheme: GetTransaction was called, want it never reached (fail closed before replay)")
	}
	if provider.called {
		t.Error("Convert with unknown stored scheme: fx Rate was called, want it never reached (idempotency resolves before the rate lookup)")
	}
}

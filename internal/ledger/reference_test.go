package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// newReferenceAccounts creates a tenant and a debit/credit account pair for
// the reference and value-dating tests (Task 4.3, audit A1.3), mirroring
// newReverseAccounts / mkTxn's fixed 250 USD shape.
func newReferenceAccounts(t *testing.T, repo *postgres.Repository, tenant string) (debit, credit domain.Account) {
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, tenant, "reference test tenant"); err != nil {
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

// TestPost_ReferencePersists posts a transaction with a client-supplied
// reference and checks it round-trips through GetTransaction unchanged.
func TestPost_ReferencePersists(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	ref := "INV-1001"
	txn := mkTxn(t, debit.ID, credit.ID)
	txn.Reference = &ref
	if _, err := svc.Post(ctx, tenant, txn, &domain.Idempotency{Key: "reference-persists-1"}); err != nil {
		t.Fatalf("post: %v", err)
	}

	reread, err := repo.GetTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if reread.Reference == nil || *reread.Reference != ref {
		t.Errorf("reference = %v, want pointer to %q", reread.Reference, ref)
	}
}

// TestPost_DuplicateReferenceRejected posts two transactions with the same
// reference in the same tenant: the second must fail with
// domain.ErrDuplicateReference (transactions_tenant_reference_idx, migration
// 0018), and the same reference must be allowed again for a different tenant.
func TestPost_DuplicateReferenceRejected(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	ref := "INV-DUP-1"
	first := mkTxn(t, debit.ID, credit.ID)
	first.Reference = &ref
	if _, err := svc.Post(ctx, tenant, first, &domain.Idempotency{Key: "reference-dup-1"}); err != nil {
		t.Fatalf("post first: %v", err)
	}

	second := mkTxn(t, debit.ID, credit.ID)
	second.Reference = &ref
	if _, err := svc.Post(ctx, tenant, second, &domain.Idempotency{Key: "reference-dup-2"}); !errors.Is(err, domain.ErrDuplicateReference) {
		t.Errorf("post second with same reference: err = %v, want ErrDuplicateReference", err)
	}

	// The same reference must be allowed for a completely different tenant.
	otherTenant := uuid.NewString()
	otherDebit, otherCredit := newReferenceAccounts(t, repo, otherTenant)
	third := mkTxn(t, otherDebit.ID, otherCredit.ID)
	third.Reference = &ref
	if _, err := svc.Post(ctx, otherTenant, third, &domain.Idempotency{Key: "reference-dup-3"}); err != nil {
		t.Errorf("post with same reference in a different tenant: err = %v, want nil", err)
	}
}

// TestPost_EffectiveAtInPastPersists posts a transaction with an explicit
// effective_at a couple seconds in the past (the convention this package's
// fixtures use for backdated rows) and checks the exact value round-trips.
func TestPost_EffectiveAtInPastPersists(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	past := time.Now().Add(-2 * time.Second).UTC().Truncate(time.Microsecond)
	txn := mkTxn(t, debit.ID, credit.ID)
	txn.EffectiveAt = &past
	if _, err := svc.Post(ctx, tenant, txn, &domain.Idempotency{Key: "effective-at-past-1"}); err != nil {
		t.Fatalf("post: %v", err)
	}

	reread, err := repo.GetTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if reread.EffectiveAt == nil || !reread.EffectiveAt.Equal(past) {
		t.Errorf("effective_at = %v, want %v", reread.EffectiveAt, past)
	}
}

// TestPost_EffectiveAtDefaultsToCreatedAt posts a transaction with no
// effective_at at all and checks it reads back equal to when the row was
// actually posted, per the read-time fallback (Task 4.3, audit A1.3): the
// column itself stays NULL, only the read path resolves it.
func TestPost_EffectiveAtDefaultsToCreatedAt(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	before := time.Now().Add(-time.Second)
	txn := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, txn, &domain.Idempotency{Key: "effective-at-default-1"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	after := time.Now().Add(time.Second)

	// The value handed back on the same object Post populated, with no
	// round trip through GetTransaction, must already carry the fallback.
	if txn.EffectiveAt == nil {
		t.Fatal("txn.EffectiveAt is nil immediately after Post, want the created_at fallback")
	}
	if txn.EffectiveAt.Before(before) || txn.EffectiveAt.After(after) {
		t.Errorf("txn.EffectiveAt = %v, want between %v and %v", txn.EffectiveAt, before, after)
	}

	reread, err := repo.GetTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if reread.EffectiveAt == nil {
		t.Fatal("reread.EffectiveAt is nil, want the created_at fallback")
	}
	if !reread.EffectiveAt.Equal(*txn.EffectiveAt) {
		t.Errorf("reread.EffectiveAt = %v, want %v (equal to the value Post resolved)", reread.EffectiveAt, txn.EffectiveAt)
	}
	if reread.EffectiveAt.Before(before) || reread.EffectiveAt.After(after) {
		t.Errorf("reread.EffectiveAt = %v, want between %v and %v", reread.EffectiveAt, before, after)
	}
}

// TestPost_InvalidReferenceRejected checks Transaction.Validate's Task 4.3
// rules (an empty-but-present reference, and one over
// domain.MaxTransactionReferenceLen) are enforced before anything reaches
// storage: Post must return the domain validation error directly, without a
// database round trip.
func TestPost_InvalidReferenceRejected(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	empty := ""
	txn := mkTxn(t, debit.ID, credit.ID)
	txn.Reference = &empty
	if _, err := svc.Post(ctx, tenant, txn, &domain.Idempotency{Key: "invalid-reference-empty"}); !errors.Is(err, domain.ErrInvalidReference) {
		t.Errorf("post with empty reference: err = %v, want ErrInvalidReference", err)
	}

	tooLong := string(make([]byte, domain.MaxTransactionReferenceLen+1))
	txn2 := mkTxn(t, debit.ID, credit.ID)
	txn2.Reference = &tooLong
	if _, err := svc.Post(ctx, tenant, txn2, &domain.Idempotency{Key: "invalid-reference-too-long"}); !errors.Is(err, domain.ErrReferenceTooLong) {
		t.Errorf("post with over-length reference: err = %v, want ErrReferenceTooLong", err)
	}
}

// TestPost_SameKeyDifferentReferenceConflicts is the audit fix this file
// exists for (Task 4.3 review, fingerprint scheme "v2"): the "v1" fingerprint
// hashed only postings (account, amount, currency, description), so reusing
// an Idempotency-Key with the SAME postings but a DIFFERENT Reference used to
// sail through the replay gate and silently hand back the ORIGINAL
// transaction, with the ORIGINAL reference, instead of a 409. That is a real
// reconciliation hazard: a caller retrying against a different upstream
// external id would never learn its reference was discarded. "v2" folds
// Reference into the fingerprint (see domain.fingerprintV2), so this must now
// be a hard conflict.
func TestPost_SameKeyDifferentReferenceConflicts(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	key := &domain.Idempotency{Key: "reference-conflict-1"}
	refA := "INV-CONFLICT-A"
	first := mkTxn(t, debit.ID, credit.ID)
	first.Reference = &refA
	if _, err := svc.Post(ctx, tenant, first, key); err != nil {
		t.Fatalf("first post: %v", err)
	}

	// Same key, same postings, DIFFERENT reference: must 409, never replay.
	refB := "INV-CONFLICT-B"
	second := mkTxn(t, debit.ID, credit.ID)
	second.Reference = &refB
	replayed, err := svc.Post(ctx, tenant, second, key)
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("post with reused key and different reference: err = %v, want ErrIdempotencyConflict", err)
	}
	if replayed {
		t.Error("post with reused key and different reference: replayed = true, want false")
	}

	// The original transaction, and its original reference, must be
	// untouched: no silent substitution happened.
	reread, err := repo.GetTransaction(ctx, tenant, first.ID)
	if err != nil {
		t.Fatalf("get original transaction: %v", err)
	}
	if reread.Reference == nil || *reread.Reference != refA {
		t.Errorf("original transaction's reference = %v, want pointer to %q (untouched)", reread.Reference, refA)
	}
}

// TestPost_SameKeySameReferenceReplays is the companion to the conflict test
// above: retrying the SAME request (same postings, same reference) under the
// same Idempotency-Key must still replay cleanly under the "v2" scheme, not
// spuriously 409 now that Reference (and EffectiveAt) are part of the hash.
func TestPost_SameKeySameReferenceReplays(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	key := &domain.Idempotency{Key: "reference-replay-1"}
	ref := "INV-REPLAY-1"
	first := mkTxn(t, debit.ID, credit.ID)
	first.Reference = &ref
	if _, err := svc.Post(ctx, tenant, first, key); err != nil {
		t.Fatalf("first post: %v", err)
	}

	// Identical retry: same postings, same reference, no EffectiveAt either
	// time (the common case, and the one the fingerprint-scheme mutation
	// hazard documented on Post's fingerprint snapshot would have broken: see
	// service.go's "before" snapshot comment).
	second := mkTxn(t, debit.ID, credit.ID)
	second.Reference = &ref
	replayed, err := svc.Post(ctx, tenant, second, key)
	if err != nil {
		t.Fatalf("identical retry: err = %v, want nil", err)
	}
	if !replayed {
		t.Error("identical retry: replayed = false, want true")
	}
	if second.ID != first.ID {
		t.Errorf("identical retry: id = %s, want the original id %s", second.ID, first.ID)
	}
}

// TestPost_LegacyV1SchemeKeyStillReplays proves the Task 2.3 scheme dispatch
// still works after the "v2" bump: an idempotency key stored under "v1"
// (as every key written before this change was) must keep replaying against
// a "v1" recomputation, never get compared against "v2"'s content, even
// though CurrentFingerprintScheme is now "v2" for every NEW key this binary
// writes.
func TestPost_LegacyV1SchemeKeyStillReplays(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	// Post the original transaction without going through the service's own
	// idempotency bookkeeping (no idem key here), then manually insert a
	// "v1"-scheme idempotency key pointing at it, computed the way a
	// pre-4.3 binary would have (postings only, no reference).
	original := mkTxn(t, debit.ID, credit.ID)
	if err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.CreateTransaction(ctx, tenant, original)
	}); err != nil {
		t.Fatalf("create original transaction: %v", err)
	}

	legacyKey := "legacy-v1-key"
	legacyFingerprint := original.Fingerprint() // the "v1" scheme's own method, deliberately not fingerprintV2
	if err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.InsertIdempotencyKey(ctx, tenant, legacyKey, legacyFingerprint, "v1", original.ID)
	}); err != nil {
		t.Fatalf("insert legacy v1 idempotency key: %v", err)
	}

	// A retry under the same key, with the same postings the legacy
	// fingerprint was computed over: must replay via the "v1" dispatch case,
	// even though this binary's CurrentFingerprintScheme is "v2".
	retry := mkTxn(t, debit.ID, credit.ID)
	replayed, err := svc.Post(ctx, tenant, retry, &domain.Idempotency{Key: legacyKey})
	if err != nil {
		t.Fatalf("retry against legacy v1 key: err = %v, want nil", err)
	}
	if !replayed {
		t.Error("retry against legacy v1 key: replayed = false, want true")
	}
	if retry.ID != original.ID {
		t.Errorf("retry against legacy v1 key: id = %s, want original id %s", retry.ID, original.ID)
	}
}

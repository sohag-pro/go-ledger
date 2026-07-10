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

// newReferenceAccounts creates a tenant and a debit/credit account pair for
// the Task 4.3 (audit A1.3) reference and value-dating tests, mirroring
// TestHappyPath's fixture shape.
func newReferenceAccounts(t *testing.T, repo *postgres.Repository, tenant string) (debit, credit domain.Account) {
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, tenant, "postgres reference test tenant"); err != nil {
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

func refTxn(t *testing.T, debit, credit string) *domain.Transaction {
	t.Helper()
	return &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit, Amount: money(t, 250, "USD")},
		{AccountID: credit, Amount: money(t, -250, "USD")},
	}}
}

// TestCreateTransaction_ReferenceAndEffectiveAtRoundTrip exercises
// Repository.CreateTransaction and GetTransaction directly (Task 4.3, audit
// A1.3): a caller-supplied reference and a past effective_at both persist and
// read back unchanged, and CreateTransaction resolves EffectiveAt on the
// transaction it was handed even without a round trip through GetTransaction.
func TestCreateTransaction_ReferenceAndEffectiveAtRoundTrip(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	ref := "PG-INV-1001"
	past := time.Now().Add(-2 * time.Second).UTC().Truncate(time.Microsecond)
	txn := refTxn(t, debit.ID, credit.ID)
	txn.Reference = &ref
	txn.EffectiveAt = &past

	if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	if txn.Reference == nil || *txn.Reference != ref {
		t.Errorf("txn.Reference = %v, want pointer to %q", txn.Reference, ref)
	}
	if txn.EffectiveAt == nil || !txn.EffectiveAt.Equal(past) {
		t.Errorf("txn.EffectiveAt = %v, want %v", txn.EffectiveAt, past)
	}

	got, err := repo.GetTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if got.Reference == nil || *got.Reference != ref {
		t.Errorf("re-read reference = %v, want pointer to %q", got.Reference, ref)
	}
	if got.EffectiveAt == nil || !got.EffectiveAt.Equal(past) {
		t.Errorf("re-read effective_at = %v, want %v", got.EffectiveAt, past)
	}
}

// TestCreateTransaction_NoReferenceOrEffectiveAt checks the omitted-fields
// path directly against the repository: Reference stays nil, and
// EffectiveAt is resolved to a value close to now (the created_at fallback,
// Task 4.3, audit A1.3) both on the object CreateTransaction returns and on
// a later GetTransaction, and the two agree.
func TestCreateTransaction_NoReferenceOrEffectiveAt(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	before := time.Now().Add(-time.Second)
	txn := refTxn(t, debit.ID, credit.ID)
	if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	after := time.Now().Add(time.Second)

	if txn.Reference != nil {
		t.Errorf("txn.Reference = %v, want nil", txn.Reference)
	}
	if txn.EffectiveAt == nil {
		t.Fatal("txn.EffectiveAt is nil, want the created_at fallback")
	}
	if txn.EffectiveAt.Before(before) || txn.EffectiveAt.After(after) {
		t.Errorf("txn.EffectiveAt = %v, want between %v and %v", txn.EffectiveAt, before, after)
	}

	got, err := repo.GetTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if got.Reference != nil {
		t.Errorf("re-read reference = %v, want nil", got.Reference)
	}
	if got.EffectiveAt == nil || !got.EffectiveAt.Equal(*txn.EffectiveAt) {
		t.Errorf("re-read effective_at = %v, want %v (equal to what CreateTransaction resolved)", got.EffectiveAt, txn.EffectiveAt)
	}
}

// TestCreateTransaction_DuplicateReferenceRejected checks the
// transactions_tenant_reference_idx unique-violation mapping (migration
// 0018, Task 4.3, audit A1.3) directly: a second transaction reusing a
// reference already used in the same tenant is rejected with
// domain.ErrDuplicateReference, distinct from ErrDuplicateTransaction (an id
// collision) and ErrTransactionAlreadyReversed (a second reversal), and the
// same reference is allowed again for a different tenant.
func TestCreateTransaction_DuplicateReferenceRejected(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newReferenceAccounts(t, repo, tenant)

	ref := "PG-DUP-1"
	first := refTxn(t, debit.ID, credit.ID)
	first.Reference = &ref
	if err := repo.CreateTransaction(ctx, tenant, first); err != nil {
		t.Fatalf("create first transaction: %v", err)
	}

	second := refTxn(t, debit.ID, credit.ID)
	second.Reference = &ref
	if err := repo.CreateTransaction(ctx, tenant, second); !errors.Is(err, domain.ErrDuplicateReference) {
		t.Errorf("create second transaction with same reference: err = %v, want ErrDuplicateReference", err)
	}

	otherTenant := uuid.NewString()
	otherDebit, otherCredit := newReferenceAccounts(t, repo, otherTenant)
	third := refTxn(t, otherDebit.ID, otherCredit.ID)
	third.Reference = &ref
	if err := repo.CreateTransaction(ctx, otherTenant, third); err != nil {
		t.Errorf("create with same reference in a different tenant: err = %v, want nil", err)
	}
}

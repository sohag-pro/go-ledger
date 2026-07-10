package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestAccountService_StatementExport_DecryptsDescriptions is the Task 6.2
// integration proof for the period statement export (Task 6.3, audit A9.2):
// a posting description encrypted at post time comes back PLAINTEXT through
// StatementExport when a cipher is wired, even though the stored column
// itself holds ciphertext (checked directly via rawPostingDescription, the
// same helper crypto_shredding_test.go's own tests use).
func TestAccountService_StatementExport_DecryptsDescriptions(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	cipher := newTestCipher(t, postgres.NewRepository(pool))
	_, txns, accounts, tenant, debitID, creditID := setupCryptoTestTenant(t, cipher)
	ctx := context.Background()

	const plaintext = "invoice #4471, period export"
	txn := mkTxnWithDescription(t, debitID, creditID, plaintext)
	if _, err := txns.Post(ctx, tenant, txn, &domain.Idempotency{Key: "statement-export-decrypt-1"}); err != nil {
		t.Fatalf("post: %v", err)
	}

	stored := rawPostingDescription(t, pool, txn.ID, debitID)
	if stored == plaintext {
		t.Fatal("stored posting description equals the plaintext: the cipher is not actually encrypting at rest")
	}

	_, entries, truncated, err := accounts.StatementExport(ctx, tenant, debitID, nil, nil)
	if err != nil {
		t.Fatalf("StatementExport: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false")
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Description != plaintext {
		t.Errorf("exported description = %q, want the decrypted plaintext %q", entries[0].Description, plaintext)
	}
}

// TestAccountService_StatementExport_DateRange posts several transactions,
// sleeping between them so created_at is strictly increasing (the same
// margin TestListTransactions relies on), then checks that from/to bound the
// export to exactly the expected subset, with the boundary read back from an
// unfiltered export (the database's own clock, not a guess), mirroring
// TestListTransactions' own from/to test.
func TestAccountService_StatementExport_DateRange(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	accounts := ledger.NewAccountService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "statement export date range tenant"); err != nil {
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

	const n = 5
	for i := 0; i < n; i++ {
		txn := mkTxn(t, debit.ID, credit.ID)
		if _, err := txns.Post(ctx, tenant, txn, &domain.Idempotency{Key: uuid.NewString()}); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	_, all, _, err := accounts.StatementExport(ctx, tenant, debit.ID, nil, nil)
	if err != nil {
		t.Fatalf("StatementExport (unfiltered): %v", err)
	}
	if len(all) != n {
		t.Fatalf("unfiltered entries = %d, want %d", len(all), n)
	}
	// all is newest first (index 0 is the last posted). Bound the window to
	// [all[3].CreatedAt, all[1].CreatedAt): from inclusive keeps all[3] and
	// all[2]; to exclusive drops all[1] and all[0].
	from := all[3].CreatedAt
	to := all[1].CreatedAt
	_, ranged, _, err := accounts.StatementExport(ctx, tenant, debit.ID, &from, &to)
	if err != nil {
		t.Fatalf("StatementExport (ranged): %v", err)
	}
	if len(ranged) != 2 {
		t.Fatalf("ranged entries = %d, want 2", len(ranged))
	}
	if ranged[0].ID != all[2].ID || ranged[1].ID != all[3].ID {
		t.Errorf("ranged ids = [%s, %s], want [%s, %s] (newest first within the window)", ranged[0].ID, ranged[1].ID, all[2].ID, all[3].ID)
	}
}

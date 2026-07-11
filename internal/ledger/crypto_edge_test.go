package ledger_test

// Task 6.2 (audit A9.3): crypto.go's decryptSnapshotDescriptions is
// deliberately defensive (its own doc comment says so): an audit snapshot
// that does not look exactly like the shape auditSnapshot writes must come
// back UNCHANGED, never as an error, so a future snapshot shape (or a
// pre-6.2 row with no "postings" key) never breaks a read. audit_log itself
// is append-only (a trigger rejects UPDATE/DELETE, the whole point of the
// tamper-evident chain), so these tests cannot overwrite an existing row;
// instead they append a fresh audit_outbox event with a crafted, but still
// syntactically valid, after JSON payload (the chainer copies After verbatim
// into audit_log.after, see internal/audit.Chainer, and never validates its
// shape) and drain it, then read it back through AuditService.ByTransaction
// with a real cipher configured. The final test in this file drives
// decryptAuditEntries's own error-propagation branch: when the cipher's
// Decrypt call itself fails (a corrupt or unreachable key, not merely
// "nothing to decrypt"), that error must come back to the caller, not be
// swallowed.

import (
	"context"
	"errors"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// postWithCraftedAfter posts a real, balanced transaction between debitID
// and creditID, then appends ITS OWN audit_outbox event with after as the
// raw snapshot bytes, instead of the one Post itself would have written
// (Post's own audit_outbox append happens inside the same RunInTx as
// CreateTransaction; a second AppendAuditOutbox call for the same
// transaction is exactly what a caller bypassing the normal service path
// could do, and is exactly what this test needs: one real, chainable audit
// row carrying deliberately unusual after content).
func postWithCraftedAfter(t *testing.T, repo *postgres.Repository, tenant, debitID, creditID string, after []byte) string {
	t.Helper()
	ctx := context.Background()
	debit, err := domain.NewMoney(100, "USD")
	if err != nil {
		t.Fatalf("new money debit: %v", err)
	}
	credit, err := domain.NewMoney(-100, "USD")
	if err != nil {
		t.Fatalf("new money credit: %v", err)
	}
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debitID, Amount: debit},
		{AccountID: creditID, Amount: credit},
	}}
	err = repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		if err := tx.CreateTransaction(ctx, tenant, txn); err != nil {
			return err
		}
		return tx.AppendAuditOutbox(ctx, tenant, domain.AuditEvent{
			Action:        domain.ActionTransactionCreated,
			TransactionID: txn.ID,
			Actor:         "test",
			After:         after,
		})
	})
	if err != nil {
		t.Fatalf("post with crafted after: %v", err)
	}
	return txn.ID
}

// TestCrypto_DecryptSnapshotDescriptions_MalformedShapesPassThroughUnchanged
// covers three of decryptSnapshotDescriptions's "this does not look like our
// shape" early returns: valid JSON with no "postings" key, a "postings"
// value that is not an array, and a postings array element that is not an
// object. (A fourth branch, invalid JSON syntax, is not reachable through any
// real write path: audit_log.after and audit_outbox.after are both
// Postgres json-typed columns, so the database itself rejects
// non-well-formed JSON at INSERT, before this function would ever see it.)
// In every case tested, ByTransaction with a cipher configured must return
// the exact bytes unchanged and no error.
func TestCrypto_DecryptSnapshotDescriptions_MalformedShapesPassThroughUnchanged(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	cipher := newTestCipher(t, repo)
	_, _, _, tenant, debitID, creditID := setupCryptoTestTenant(t, cipher)
	audits := ledger.NewAuditService(repo, ledger.WithAuditCipher(cipher))
	ctx := context.Background()

	tests := []struct {
		name string
		raw  string
	}{
		{"no postings key", `{"action":"transaction_created"}`},
		{"postings not an array", `{"postings":"not-an-array"}`},
		{"postings element not an object", `{"postings":["not-an-object"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			txnID := postWithCraftedAfter(t, repo, tenant, debitID, creditID, []byte(tt.raw))
			drainChainer(t, pool, tenant)

			rows, err := audits.ByTransaction(ctx, tenant, txnID)
			if err != nil {
				t.Fatalf("ByTransaction with malformed after (%s): %v", tt.name, err)
			}
			if len(rows) != 1 {
				t.Fatalf("ByTransaction with malformed after (%s) = %d rows, want 1", tt.name, len(rows))
			}
			if string(rows[0].After) != tt.raw {
				t.Errorf("ByTransaction with malformed after (%s): after = %s, want unchanged %s", tt.name, rows[0].After, tt.raw)
			}
		})
	}
}

// decryptErrorCipher is a ledger.DescriptionCipher whose Decrypt always
// fails, standing in for a corrupt or otherwise unusable key at read time
// (distinct from erroringCipher in reverse_test.go, whose Encrypt fails
// instead).
type decryptErrorCipher struct{ err error }

func (c decryptErrorCipher) Encrypt(_ context.Context, _ string, plaintext string) (string, error) {
	return plaintext, nil
}

func (c decryptErrorCipher) Decrypt(context.Context, string, string) (string, error) {
	return "", c.err
}

// TestCrypto_DecryptAuditEntries_DecryptErrorPropagates proves
// decryptAuditEntries (via ByTransaction) surfaces a failing cipher.Decrypt
// call rather than silently returning the raw ciphertext or panicking.
func TestCrypto_DecryptAuditEntries_DecryptErrorPropagates(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	realCipher := newTestCipher(t, repo)
	_, txns, _, tenant, debitID, creditID := setupCryptoTestTenant(t, realCipher)
	ctx := context.Background()

	txn := mkTxnWithDescription(t, debitID, creditID, "a real description")
	if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	drainChainer(t, pool, tenant)

	sentinel := errors.New("boom: decrypt failed")
	failingAudits := ledger.NewAuditService(repo, ledger.WithAuditCipher(decryptErrorCipher{err: sentinel}))
	if _, err := failingAudits.ByTransaction(ctx, tenant, txn.ID); !errors.Is(err, sentinel) {
		t.Errorf("ByTransaction with a failing Decrypt: err = %v, want the sentinel", err)
	}
}

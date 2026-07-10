package ledger_test

// Task 6.2 (audit A9.3): PII crypto-shredding. These tests are the
// money-critical proof the task exists for: a posting description is
// encrypted once at post time, the SAME ciphertext lands in both
// postings.description and the audit snapshot (audit_log.after), and
// shredding a tenant's key (internal/crypto.Cipher, via
// domain.Repository.ShredTenantCryptoKey) makes descriptions permanently
// unreadable WITHOUT mutating any money row and WITHOUT breaking the
// tamper-evident hash chain: AuditService.Verify must still return Valid
// after a shred, because it hashes the exact stored ciphertext bytes and
// never decrypts them.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/crypto"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// cryptoTestMasterKey is a fixed, valid 32-byte master key, base64-encoded,
// used throughout this file. Its exact bytes carry no meaning; what matters
// is that every test in this file that wants a REAL cipher uses the same
// one, backed by the real postgres.Repository (which implements
// crypto.KeyStore directly, see internal/postgres/crypto_keys.go), so
// encrypt/unwrap round trips exercise the real crypto_keys table.
const cryptoTestMasterKey = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=" // base64("01234567890123456789012345678901")

// newTestCipher returns a *crypto.Cipher backed by repo (which satisfies
// crypto.KeyStore), for tests that want real envelope encryption against the
// shared test container rather than a nil (disabled) cipher.
func newTestCipher(t *testing.T, repo *postgres.Repository) *crypto.Cipher {
	t.Helper()
	c, err := crypto.NewCipher(cryptoTestMasterKey, repo)
	if err != nil {
		t.Fatalf("crypto.NewCipher: %v", err)
	}
	return c
}

// mkTxnWithDescription is mkTxn (idempotency_test.go) plus a real posting
// description on the debit leg, the free-text PII this whole feature exists
// to protect.
func mkTxnWithDescription(t *testing.T, debit, credit, description string) *domain.Transaction {
	t.Helper()
	d, _ := domain.NewMoney(250, "USD")
	c, _ := domain.NewMoney(-250, "USD")
	return &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit, Amount: d, Description: description},
		{AccountID: credit, Amount: c},
	}}
}

// rawPostingDescription reads a posting's description column directly,
// bypassing every service-layer decrypt, so the test can assert on exactly
// what is stored at rest.
func rawPostingDescription(t *testing.T, pool *pgxpool.Pool, transactionID, accountID string) string {
	t.Helper()
	var desc string
	err := pool.QueryRow(context.Background(),
		`SELECT description FROM postings WHERE transaction_id = $1 AND account_id = $2 AND description <> ''`,
		uuid.MustParse(transactionID), uuid.MustParse(accountID),
	).Scan(&desc)
	if err != nil {
		t.Fatalf("read raw posting description: %v", err)
	}
	return desc
}

// rawAuditAfterDescription reads the FIRST non-empty posting description
// embedded in an audit_log row's after snapshot for transactionID, directly,
// bypassing AuditService's own decrypt. It proves the exact same string
// rawPostingDescription reads is what the audit snapshot carries too (the
// encrypt-once, same-ciphertext-in-both-places invariant this task's
// correctness rests on).
func rawAuditAfterDescription(t *testing.T, pool *pgxpool.Pool, transactionID string) string {
	t.Helper()
	var after []byte
	err := pool.QueryRow(context.Background(),
		`SELECT after FROM audit_log WHERE transaction_id = $1 ORDER BY chain_seq LIMIT 1`,
		uuid.MustParse(transactionID),
	).Scan(&after)
	if err != nil {
		t.Fatalf("read raw audit after: %v", err)
	}
	var snapshot struct {
		Postings []struct {
			Description string `json:"description"`
		} `json:"postings"`
	}
	if err := json.Unmarshal(after, &snapshot); err != nil {
		t.Fatalf("unmarshal audit snapshot: %v", err)
	}
	for _, p := range snapshot.Postings {
		if p.Description != "" {
			return p.Description
		}
	}
	t.Fatal("audit snapshot has no non-empty posting description")
	return ""
}

// setupCryptoTestTenant creates a tenant with two USD accounts, returning
// everything a test in this file needs: the pool, both services, and the two
// account ids. A caller that also needs the *postgres.Repository directly
// (for example to call ShredTenantCryptoKey) builds its own with
// postgres.NewRepository(pool): it is a stateless wrapper, so a second value
// over the same pool behaves identically to this function's own internal one.
func setupCryptoTestTenant(t *testing.T, cipher *crypto.Cipher) (pool *pgxpool.Pool, txns *ledger.TransactionService, accounts *ledger.AccountService, tenant, debitID, creditID string) {
	t.Helper()
	pool = newTestPool(t)
	repo := postgres.NewRepository(pool)
	var opts []ledger.ServiceOption
	if cipher != nil {
		opts = append(opts, ledger.WithCipher(cipher))
	}
	txns = ledger.NewTransactionService(repo, discardLogger(), nil, opts...)
	var acctOpts []ledger.AccountOption
	if cipher != nil {
		acctOpts = append(acctOpts, ledger.WithAccountCipher(cipher))
	}
	accounts = ledger.NewAccountService(repo, acctOpts...)

	ctx := context.Background()
	tenant = uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "crypto shredding test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	debit := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	credit := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := accounts.Create(ctx, tenant, debit); err != nil {
		t.Fatalf("create debit account: %v", err)
	}
	if err := accounts.Create(ctx, tenant, credit); err != nil {
		t.Fatalf("create credit account: %v", err)
	}
	return pool, txns, accounts, tenant, debit.ID, credit.ID
}

// TestCrypto_RoundTrip_StoredCiphertextButPlaintextResponse proves: a posted
// description is stored as ciphertext (enc:v1: prefix) at rest, yet Post's
// OWN response (the *domain.Transaction handed back to the caller in the
// same call) still shows the plaintext the caller submitted, never
// ciphertext; and a later Get returns the same original plaintext, decrypted.
func TestCrypto_RoundTrip_StoredCiphertextButPlaintextResponse(t *testing.T) {
	t.Parallel()
	repo := postgres.NewRepository(newTestPool(t))
	cipher := newTestCipher(t, repo)
	pool, txns, _, tenant, debitID, creditID := setupCryptoTestTenant(t, cipher)
	ctx := context.Background()

	const plaintext = "rent payment for March"
	txn := mkTxnWithDescription(t, debitID, creditID, plaintext)
	if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post: %v", err)
	}

	// The caller's own response, from the very call that just posted it,
	// shows the ORIGINAL plaintext, never ciphertext.
	if got := txn.Postings[0].Description; got != plaintext {
		t.Errorf("Post's own response description = %q, want unchanged plaintext %q", got, plaintext)
	}

	// What is actually stored at rest is ciphertext.
	stored := rawPostingDescription(t, pool, txn.ID, debitID)
	if stored == plaintext {
		t.Fatal("stored posting description equals the plaintext: it was never encrypted")
	}
	if len(stored) < len(crypto.EncodingPrefix) || stored[:len(crypto.EncodingPrefix)] != crypto.EncodingPrefix {
		t.Errorf("stored description %q does not carry EncodingPrefix %q", stored, crypto.EncodingPrefix)
	}

	// A later, independent read (Get) decrypts back to the original.
	got, err := txns.Get(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Postings[0].Description != plaintext {
		t.Errorf("Get description = %q, want decrypted %q", got.Postings[0].Description, plaintext)
	}
}

// TestCrypto_AuditSnapshotCarriesIdenticalCiphertext proves the encrypt-once
// invariant: the SAME ciphertext string is stored in both
// postings.description and audit_log.after (byte-identical), and that the
// tamper-evident chain still verifies with that ciphertext in place.
func TestCrypto_AuditSnapshotCarriesIdenticalCiphertext(t *testing.T) {
	t.Parallel()
	repo := postgres.NewRepository(newTestPool(t))
	cipher := newTestCipher(t, repo)
	pool, txns, _, tenant, debitID, creditID := setupCryptoTestTenant(t, cipher)
	ctx := context.Background()

	txn := mkTxnWithDescription(t, debitID, creditID, "invoice #4471")
	if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	drainChainer(t, pool, tenant)

	storedPosting := rawPostingDescription(t, pool, txn.ID, debitID)
	storedAudit := rawAuditAfterDescription(t, pool, txn.ID)
	if storedPosting != storedAudit {
		t.Errorf("postings.description (%q) != audit_log.after's embedded description (%q); the audit snapshot must carry the IDENTICAL ciphertext string", storedPosting, storedAudit)
	}

	audits := ledger.NewAuditService(repo)
	result, err := audits.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid {
		t.Errorf("chain valid = false with a ciphertext-era row present, want true: %+v", result)
	}
}

// TestCrypto_ShredMakesDescriptionsRedacted_MoneyAndChainUnaffected is the
// deliverable proof for Task 6.2: after ShredTenantCryptoKey, the tenant's
// descriptions read as crypto.RedactedMarker, account balances are
// unchanged, and AuditService.Verify STILL returns Valid, because the
// row_hash covers the ciphertext bytes and shredding never touches them.
// Another tenant's descriptions are unaffected by this tenant's shred.
func TestCrypto_ShredMakesDescriptionsRedacted_MoneyAndChainUnaffected(t *testing.T) {
	t.Parallel()
	repo := postgres.NewRepository(newTestPool(t))
	cipher := newTestCipher(t, repo)
	pool, txns, accounts, tenant, debitID, creditID := setupCryptoTestTenant(t, cipher)
	_, otherTxns, _, otherTenant, otherDebitID, otherCreditID := setupCryptoTestTenant(t, cipher)
	ctx := context.Background()

	const plaintext = "salary payment"
	txn := mkTxnWithDescription(t, debitID, creditID, plaintext)
	if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post tenant under test: %v", err)
	}

	const otherPlaintext = "other tenant's own secret"
	otherTxn := mkTxnWithDescription(t, otherDebitID, otherCreditID, otherPlaintext)
	if _, err := otherTxns.Post(ctx, otherTenant, otherTxn, nil); err != nil {
		t.Fatalf("post other tenant: %v", err)
	}

	drainChainer(t, pool, tenant)
	drainChainer(t, pool, otherTenant)

	audits := ledger.NewAuditService(repo)
	before, err := audits.Verify(ctx, tenant)
	if err != nil || !before.Valid {
		t.Fatalf("verify before shred: result=%+v err=%v, want a valid chain", before, err)
	}

	debitBalanceBefore, err := accounts.Balance(ctx, tenant, debitID)
	if err != nil {
		t.Fatalf("balance before shred: %v", err)
	}
	creditBalanceBefore, err := accounts.Balance(ctx, tenant, creditID)
	if err != nil {
		t.Fatalf("credit balance before shred: %v", err)
	}

	// The money-critical operation: destroy this tenant's key. Nothing about
	// postings, transactions, or the audit log is touched by this call.
	if err := repo.ShredTenantCryptoKey(ctx, tenant); err != nil {
		t.Fatalf("shred tenant crypto key: %v", err)
	}

	// 1. Descriptions read as the redacted marker, not an error.
	got, err := txns.Get(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get after shred: %v", err)
	}
	if got.Postings[0].Description != crypto.RedactedMarker {
		t.Errorf("description after shred = %q, want %q", got.Postings[0].Description, crypto.RedactedMarker)
	}

	// 2. Money data is completely unchanged.
	debitBalanceAfter, err := accounts.Balance(ctx, tenant, debitID)
	if err != nil {
		t.Fatalf("balance after shred: %v", err)
	}
	creditBalanceAfter, err := accounts.Balance(ctx, tenant, creditID)
	if err != nil {
		t.Fatalf("credit balance after shred: %v", err)
	}
	if debitBalanceBefore.Amount() != debitBalanceAfter.Amount() {
		t.Errorf("debit balance changed by shredding: before=%d after=%d", debitBalanceBefore.Amount(), debitBalanceAfter.Amount())
	}
	if creditBalanceBefore.Amount() != creditBalanceAfter.Amount() {
		t.Errorf("credit balance changed by shredding: before=%d after=%d", creditBalanceBefore.Amount(), creditBalanceAfter.Amount())
	}

	// 3. THE KEY PROOF: the tamper-evident chain still verifies. Shredding
	// destroyed the key, not the stored ciphertext bytes the row_hash
	// covers, so recomputation is unaffected.
	after, err := audits.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify after shred: %v", err)
	}
	if !after.Valid {
		t.Fatalf("chain valid = false AFTER a shred, want true (shredding must never break the hash chain): %+v", after)
	}
	if after.Checked != before.Checked {
		t.Errorf("checked rows changed by shredding: before=%d after=%d, want unchanged", before.Checked, after.Checked)
	}

	// 4. Another tenant's descriptions are completely unaffected.
	otherGot, err := otherTxns.Get(ctx, otherTenant, otherTxn.ID)
	if err != nil {
		t.Fatalf("get other tenant after this tenant's shred: %v", err)
	}
	if otherGot.Postings[0].Description != otherPlaintext {
		t.Errorf("other tenant's description after THIS tenant's shred = %q, want unaffected %q", otherGot.Postings[0].Description, otherPlaintext)
	}
	otherAudits, err := audits.Verify(ctx, otherTenant)
	if err != nil || !otherAudits.Valid {
		t.Errorf("other tenant's chain after this tenant's shred: result=%+v err=%v, want unaffected and valid", otherAudits, err)
	}
}

// TestCrypto_BackwardCompat_LegacyPlaintextReadsUnchanged proves a posting
// description written before Task 6.2 existed (no EncodingPrefix, inserted
// directly to simulate a pre-6.2 row) reads back UNCHANGED through Get, even
// with a real cipher configured: Decrypt passes legacy plaintext through
// as-is rather than trying to decrypt it.
func TestCrypto_BackwardCompat_LegacyPlaintextReadsUnchanged(t *testing.T) {
	t.Parallel()
	repo := postgres.NewRepository(newTestPool(t))
	cipher := newTestCipher(t, repo)
	pool, txns, _, tenant, debitID, creditID := setupCryptoTestTenant(t, cipher)
	ctx := context.Background()

	// Post with NO description, then overwrite the stored column directly
	// with a legacy-shaped plaintext value, standing in for a row written
	// before Task 6.2 ever existed.
	txn := mkTxnWithDescription(t, debitID, creditID, "")
	if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	const legacy = "dinner repayment"
	if _, err := pool.Exec(ctx,
		`UPDATE postings SET description = $1 WHERE transaction_id = $2 AND account_id = $3`,
		legacy, uuid.MustParse(txn.ID), uuid.MustParse(debitID),
	); err != nil {
		t.Fatalf("simulate legacy plaintext row: %v", err)
	}

	got, err := txns.Get(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Postings[0].Description != legacy {
		t.Errorf("legacy plaintext description = %q, want unchanged %q", got.Postings[0].Description, legacy)
	}
}

// TestCrypto_DisabledMode_StoresPlaintextAsToday proves that with NO cipher
// configured (the default when LEDGER_MASTER_KEY is unset), descriptions are
// stored and returned exactly as before Task 6.2: no EncodingPrefix, no
// decrypt step, byte-identical round trip.
func TestCrypto_DisabledMode_StoresPlaintextAsToday(t *testing.T) {
	t.Parallel()
	pool, txns, _, tenant, debitID, creditID := setupCryptoTestTenant(t, nil)
	ctx := context.Background()

	const plaintext = "no encryption configured"
	txn := mkTxnWithDescription(t, debitID, creditID, plaintext)
	if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post: %v", err)
	}

	stored := rawPostingDescription(t, pool, txn.ID, debitID)
	if stored != plaintext {
		t.Errorf("stored description with encryption disabled = %q, want unchanged plaintext %q", stored, plaintext)
	}

	got, err := txns.Get(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Postings[0].Description != plaintext {
		t.Errorf("Get description with encryption disabled = %q, want unchanged %q", got.Postings[0].Description, plaintext)
	}
}

// TestCrypto_CrossTenantCannotDecrypt proves tenant A's cipher instance
// (same *crypto.Cipher, since it is a stateless, request-scoped-key-lookup
// type, exactly as cmd/server wires ONE cipher for every tenant) cannot
// decrypt tenant B's ciphertext: each tenant's DEK is independent, so
// Decrypt fails closed rather than returning tenant B's real plaintext or
// silent garbage.
func TestCrypto_CrossTenantCannotDecrypt(t *testing.T) {
	t.Parallel()
	repo := postgres.NewRepository(newTestPool(t))
	cipher := newTestCipher(t, repo)
	ctx := context.Background()

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenantA, "cross tenant test A"); err != nil {
		t.Fatalf("create tenant a: %v", err)
	}
	if err := repo.CreateTenant(ctx, tenantB, "cross tenant test B"); err != nil {
		t.Fatalf("create tenant b: %v", err)
	}

	ctA, err := cipher.Encrypt(ctx, tenantA, "tenant a secret")
	if err != nil {
		t.Fatalf("encrypt for tenant a: %v", err)
	}

	if pt, err := cipher.Decrypt(ctx, tenantB, ctA); err == nil {
		t.Errorf("tenant b decrypted tenant a's ciphertext as %q, want a decrypt failure", pt)
	}
}

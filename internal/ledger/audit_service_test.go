package ledger_test

// Task 5.3 / 6.2 (audit A2.4, A9.3): AuditService.Head and LatestAnchor (thin
// passthroughs to domain.Repository), WithAuditCipher's decrypt-on-read path
// (crypto.go's decryptAuditEntries/decryptSnapshotDescriptions), and
// NewAuditServiceWithPageSize's zero/negative-pageSize fallback. These live
// in a new file rather than audit_paging_test.go or audit_verify_test.go
// since none of the existing AuditService test files cover Head, LatestAnchor,
// or the cipher-enabled decrypt path at all.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestAuditService_HeadAndLatestAnchor covers both thin passthroughs: Head
// reports ok=false for an empty chain and the correct chain_seq/row_hash once
// a post is chained; LatestAnchor reports ok=false until
// internal/audit.AnchorJob records one, then matches it.
func TestAuditService_HeadAndLatestAnchor(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	audits := ledger.NewAuditService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "audit service head test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	seq, hash, ok, err := audits.Head(ctx, tenant)
	if err != nil {
		t.Fatalf("Head (empty chain): %v", err)
	}
	if ok {
		t.Fatalf("Head (empty chain): ok = true, want false (seq=%d hash=%q)", seq, hash)
	}

	_, ok, err = audits.LatestAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("LatestAnchor (none recorded): %v", err)
	}
	if ok {
		t.Fatal("LatestAnchor (none recorded): ok = true, want false")
	}

	debit := mustCreateAccount(t, repo, tenant, "USD")
	credit := mustCreateAccount(t, repo, tenant, "USD")
	if _, err := txns.Post(ctx, tenant, mkTxn(t, debit.ID, credit.ID), nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	drainChainer(t, pool, tenant)

	wantSeq, wantHash, headOK, err := repo.GetAuditHead(ctx, tenant)
	if err != nil || !headOK {
		t.Fatalf("repo GetAuditHead: ok=%v err=%v", headOK, err)
	}
	seq, hash, ok, err = audits.Head(ctx, tenant)
	if err != nil {
		t.Fatalf("Head (populated): %v", err)
	}
	if !ok || seq != wantSeq || hash != wantHash {
		t.Errorf("Head (populated) = (seq=%d hash=%q ok=%v), want (seq=%d hash=%q ok=true)", seq, hash, ok, wantSeq, wantHash)
	}

	anchorJob := audit.NewAnchorJob(pool, discardLogger(), time.Hour)
	if _, err := anchorJob.AnchorOnce(ctx); err != nil {
		t.Fatalf("anchor once: %v", err)
	}
	anchor, ok, err := audits.LatestAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("LatestAnchor (recorded): %v", err)
	}
	if !ok {
		t.Fatal("LatestAnchor (recorded): ok = false, want true")
	}
	if anchor.ChainSeq != wantSeq || anchor.RowHash != wantHash {
		t.Errorf("LatestAnchor = %+v, want chain_seq=%d row_hash=%q", anchor, wantSeq, wantHash)
	}
}

// TestAuditService_WithAuditCipher_DecryptsEmbeddedSnapshotDescriptions
// proves WithAuditCipher makes ByTransaction and ByAccount decrypt the
// posting description embedded in an audit snapshot's "after" JSON (Task 6.2,
// audit A9.3), exercising decryptAuditEntries and, through it,
// decryptSnapshotDescriptions: without the option, the raw ciphertext would
// leak through instead.
func TestAuditService_WithAuditCipher_DecryptsEmbeddedSnapshotDescriptions(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	cipher := newTestCipher(t, repo)
	_, txns, _, tenant, debitID, creditID := setupCryptoTestTenant(t, cipher)
	ctx := context.Background()

	const plaintext = "audit cipher plaintext"
	txn := mkTxnWithDescription(t, debitID, creditID, plaintext)
	if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	drainChainer(t, pool, tenant)

	// Without a cipher: the audit entry's embedded description is raw
	// ciphertext, never the plaintext.
	plainAudits := ledger.NewAuditService(repo)
	rawRows, err := plainAudits.ByTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("ByTransaction (no cipher): %v", err)
	}
	if len(rawRows) != 1 {
		t.Fatalf("ByTransaction (no cipher) = %d rows, want 1", len(rawRows))
	}
	if containsDescription(t, rawRows[0].After, plaintext) {
		t.Fatal("ByTransaction (no cipher) embedded plaintext description in the snapshot, want ciphertext")
	}

	// With WithAuditCipher: the embedded description decrypts back to the
	// original plaintext.
	cipheredAudits := ledger.NewAuditService(repo, ledger.WithAuditCipher(cipher))
	rows, err := cipheredAudits.ByTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("ByTransaction (with cipher): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ByTransaction (with cipher) = %d rows, want 1", len(rows))
	}
	if !containsDescription(t, rows[0].After, plaintext) {
		t.Errorf("ByTransaction (with cipher) after = %s, want it to embed decrypted plaintext %q", rows[0].After, plaintext)
	}

	byAcct, err := cipheredAudits.ByAccount(ctx, tenant, debitID, nil, 10)
	if err != nil {
		t.Fatalf("ByAccount (with cipher): %v", err)
	}
	if len(byAcct) != 1 {
		t.Fatalf("ByAccount (with cipher) = %d rows, want 1", len(byAcct))
	}
	if !containsDescription(t, byAcct[0].After, plaintext) {
		t.Errorf("ByAccount (with cipher) after = %s, want it to embed decrypted plaintext %q", byAcct[0].After, plaintext)
	}
}

// containsDescription is a crude but sufficient substring check for whether
// an audit snapshot's raw JSON embeds want somewhere in its bytes: exact
// unmarshaling into the snapshot shape is already exercised elsewhere
// (rawAuditAfterDescription in crypto_shredding_test.go); this helper only
// needs to tell ciphertext apart from plaintext for the assertions above.
func containsDescription(t *testing.T, after []byte, want string) bool {
	t.Helper()
	return len(after) > 0 && bytesContains(after, []byte(want))
}

func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestAuditService_NewWithPageSize_NonPositiveFallsBackToDefault proves
// NewAuditServiceWithPageSize falls back to DefaultVerifyPageSize for a
// zero or negative pageSize, rather than paging with a size that would make
// ListAuditForVerifyPage's limit zero (an infinite loop, since a page of 0
// rows is always "shorter than the page size") or negative (a broken SQL
// LIMIT). A small chain verifies correctly either way, in exactly one page,
// the same as the package default.
func TestAuditService_NewWithPageSize_NonPositiveFallsBackToDefault(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	for _, pageSize := range []int{0, -1} {
		t.Run(mustItoa(pageSize), func(t *testing.T) {
			t.Parallel()
			tenant := uuid.NewString()
			if err := repo.CreateTenant(ctx, tenant, "page size fallback test tenant"); err != nil {
				t.Fatalf("create tenant: %v", err)
			}
			debit := mustCreateAccount(t, repo, tenant, "USD")
			credit := mustCreateAccount(t, repo, tenant, "USD")
			if _, err := txns.Post(ctx, tenant, mkTxn(t, debit.ID, credit.ID), nil); err != nil {
				t.Fatalf("post: %v", err)
			}
			drainChainer(t, pool, tenant)

			spy := &pagingSpyRepo{Repository: repo}
			audits := ledger.NewAuditServiceWithPageSize(spy, pageSize)
			result, err := audits.Verify(ctx, tenant)
			if err != nil {
				t.Fatalf("Verify with pageSize=%d: %v", pageSize, err)
			}
			if !result.Valid || result.Checked != 1 {
				t.Errorf("Verify with pageSize=%d = %+v, want Valid=true Checked=1", pageSize, result)
			}
			if spy.pageCalls != 1 {
				t.Errorf("pageCalls with pageSize=%d = %d, want exactly 1 (a one-row chain in one default-sized page)", pageSize, spy.pageCalls)
			}
		})
	}
}

// mustItoa avoids importing strconv just for a subtest name.
func mustItoa(n int) string {
	if n == 0 {
		return "zero"
	}
	if n < 0 {
		return "negative"
	}
	return "positive"
}

// stubbedAuditRepo wraps a domain.Repository and lets a test force exactly
// one of its methods to fail, leaving every other method to delegate to the
// embedded repository unchanged. It exists to drive AuditService's several
// "repository call failed" branches (ByTransaction, ByAccount, verifyFrom,
// VerifyFromLatestAnchor), none of which a real Postgres round trip can
// provoke on demand.
type stubbedAuditRepo struct {
	domain.Repository
	listAuditByTransactionErr error
	listAuditByAccountErr     error
	countPendingOutboxErr     error
	listAuditForVerifyPageErr error
	latestAuditAnchorErr      error
}

func (s *stubbedAuditRepo) ListAuditByTransaction(ctx context.Context, tenantID, transactionID string) ([]domain.AuditEntry, error) {
	if s.listAuditByTransactionErr != nil {
		return nil, s.listAuditByTransactionErr
	}
	return s.Repository.ListAuditByTransaction(ctx, tenantID, transactionID)
}

func (s *stubbedAuditRepo) ListAuditByAccount(ctx context.Context, tenantID, accountID string, after *domain.StatementCursor, limit int) ([]domain.AuditEntry, error) {
	if s.listAuditByAccountErr != nil {
		return nil, s.listAuditByAccountErr
	}
	return s.Repository.ListAuditByAccount(ctx, tenantID, accountID, after, limit)
}

func (s *stubbedAuditRepo) CountPendingOutbox(ctx context.Context, tenantID string) (int, error) {
	if s.countPendingOutboxErr != nil {
		return 0, s.countPendingOutboxErr
	}
	return s.Repository.CountPendingOutbox(ctx, tenantID)
}

func (s *stubbedAuditRepo) ListAuditForVerifyPage(ctx context.Context, tenantID string, afterChainSeq int64, limit int) ([]domain.AuditEntry, error) {
	if s.listAuditForVerifyPageErr != nil {
		return nil, s.listAuditForVerifyPageErr
	}
	return s.Repository.ListAuditForVerifyPage(ctx, tenantID, afterChainSeq, limit)
}

func (s *stubbedAuditRepo) LatestAuditAnchor(ctx context.Context, tenantID string) (domain.AuditAnchor, bool, error) {
	if s.latestAuditAnchorErr != nil {
		return domain.AuditAnchor{}, false, s.latestAuditAnchorErr
	}
	return s.Repository.LatestAuditAnchor(ctx, tenantID)
}

// TestAuditService_ByTransactionAndByAccount_RepoErrorPropagates proves both
// reads surface a repository failure unchanged rather than swallowing it or
// panicking on the way to decryptAuditEntries.
func TestAuditService_ByTransactionAndByAccount_RepoErrorPropagates(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	sentinel := errors.New("boom: list audit failed")

	byTxnAudits := ledger.NewAuditService(&stubbedAuditRepo{Repository: repo, listAuditByTransactionErr: sentinel})
	if _, err := byTxnAudits.ByTransaction(context.Background(), uuid.NewString(), uuid.NewString()); !errors.Is(err, sentinel) {
		t.Errorf("ByTransaction with a failing repo: err = %v, want the sentinel", err)
	}

	byAcctAudits := ledger.NewAuditService(&stubbedAuditRepo{Repository: repo, listAuditByAccountErr: sentinel})
	if _, err := byAcctAudits.ByAccount(context.Background(), uuid.NewString(), uuid.NewString(), nil, 10); !errors.Is(err, sentinel) {
		t.Errorf("ByAccount with a failing repo: err = %v, want the sentinel", err)
	}
}

// TestAuditService_Verify_RepoErrorsPropagate proves verifyFrom (Verify's and
// VerifyFromLatestAnchor's shared walk) surfaces both a failing
// CountPendingOutbox and a failing ListAuditForVerifyPage, and that
// VerifyFromLatestAnchor itself surfaces a failing LatestAuditAnchor before
// ever reaching verifyFrom.
func TestAuditService_Verify_RepoErrorsPropagate(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	tenant := uuid.NewString()
	sentinel := errors.New("boom: repo failed")

	countErrAudits := ledger.NewAuditService(&stubbedAuditRepo{Repository: repo, countPendingOutboxErr: sentinel})
	if _, err := countErrAudits.Verify(context.Background(), tenant); !errors.Is(err, sentinel) {
		t.Errorf("Verify with a failing CountPendingOutbox: err = %v, want the sentinel", err)
	}

	pageErrAudits := ledger.NewAuditService(&stubbedAuditRepo{Repository: repo, listAuditForVerifyPageErr: sentinel})
	if _, err := pageErrAudits.Verify(context.Background(), tenant); !errors.Is(err, sentinel) {
		t.Errorf("Verify with a failing ListAuditForVerifyPage: err = %v, want the sentinel", err)
	}

	anchorErrAudits := ledger.NewAuditService(&stubbedAuditRepo{Repository: repo, latestAuditAnchorErr: sentinel})
	if _, err := anchorErrAudits.VerifyFromLatestAnchor(context.Background(), tenant); !errors.Is(err, sentinel) {
		t.Errorf("VerifyFromLatestAnchor with a failing LatestAuditAnchor: err = %v, want the sentinel", err)
	}
}

package ledger

import (
	"context"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// DefaultVerifyPageSize is how many audit rows Verify and
// VerifyFromLatestAnchor read per round trip when a caller does not need a
// different size (Task 5.3, audit A2.4): large enough that a normal chain
// verifies in one or two pages, small enough that memory use stays bounded
// regardless of how long a tenant's chain has grown.
const DefaultVerifyPageSize = 1000

// AuditService reads the append-only audit log. It is a thin read-through to the
// repository: the audit rows are written transactionally by TransactionService,
// so this service only queries them.
type AuditService struct {
	repo     domain.Repository
	pageSize int
	cipher   DescriptionCipher
}

// AuditServiceOption configures an optional AuditService dependency, mirroring
// TransactionService's ServiceOption and AccountService's AccountOption:
// existing callers and tests that never pass one keep compiling and behaving
// unchanged.
type AuditServiceOption func(*AuditService)

// WithAuditCipher sets the DescriptionCipher ByTransaction and ByAccount use
// to decrypt a posting description embedded in an audit snapshot's "after"
// (Task 6.2, audit A9.3), FOR DISPLAY ONLY: Verify and VerifyFromLatestAnchor
// never use it, since the tamper-evident hash chain must always be
// recomputed over the exact stored (ciphertext) bytes; see
// decryptAuditEntries's own doc comment (crypto.go). Without this option the
// cipher is nil, so ByTransaction/ByAccount return the raw stored bytes
// unchanged, exactly as before Task 6.2.
func WithAuditCipher(c DescriptionCipher) AuditServiceOption {
	return func(s *AuditService) { s.cipher = c }
}

// NewAuditService returns an AuditService backed by repo, paging Verify and
// VerifyFromLatestAnchor at DefaultVerifyPageSize.
func NewAuditService(repo domain.Repository, opts ...AuditServiceOption) *AuditService {
	s := &AuditService{repo: repo, pageSize: DefaultVerifyPageSize}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// NewAuditServiceWithPageSize is NewAuditService with an explicit page size
// for Verify's and VerifyFromLatestAnchor's paging loop (Task 5.3). pageSize
// falls back to DefaultVerifyPageSize when zero or negative, the same
// zero-falls-back-to-default idiom internal/audit.NewChainer and
// webhook.NewWorker use for their own batch sizes. Production code should
// use NewAuditService; this constructor exists so a caller (chiefly tests)
// can force paging to span many small pages instead of one page covering an
// entire small chain, proving no page-boundary row is ever skipped or
// double-checked.
func NewAuditServiceWithPageSize(repo domain.Repository, pageSize int, opts ...AuditServiceOption) *AuditService {
	if pageSize <= 0 {
		pageSize = DefaultVerifyPageSize
	}
	s := &AuditService{repo: repo, pageSize: pageSize}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ByTransaction returns the audit rows for a transaction, oldest first. Each
// row's embedded posting descriptions are decrypted (Task 6.2, audit A9.3)
// when a cipher is configured; see decryptAuditEntries for what "decrypted"
// means for a legacy plaintext row or a shredded tenant's ciphertext, and why
// this never touches PrevHash/RowHash.
func (s *AuditService) ByTransaction(ctx context.Context, tenantID, transactionID string) ([]domain.AuditEntry, error) {
	entries, err := s.repo.ListAuditByTransaction(ctx, tenantID, transactionID)
	if err != nil {
		return nil, err
	}
	return decryptAuditEntries(ctx, s.cipher, tenantID, entries)
}

// Head returns tenantID's current chain head (chain_seq, row_hash). ok is
// false for an empty chain. It is a thin passthrough to
// domain.Repository.GetAuditHead (Task 5.3), surfaced so the verify-audit-
// chain endpoint (internal/api/audit.go) can report the live head alongside
// the latest anchor without reaching past this service into the repository
// directly.
func (s *AuditService) Head(ctx context.Context, tenantID string) (chainSeq int64, rowHash string, ok bool, err error) {
	return s.repo.GetAuditHead(ctx, tenantID)
}

// LatestAnchor returns tenantID's most recently recorded off-box anchor. ok
// is false when none has ever been recorded. Thin passthrough to
// domain.Repository.LatestAuditAnchor (Task 5.3), for the same reason Head
// above is.
func (s *AuditService) LatestAnchor(ctx context.Context, tenantID string) (domain.AuditAnchor, bool, error) {
	return s.repo.LatestAuditAnchor(ctx, tenantID)
}

// ByAccount returns one keyset page of audit rows for every transaction
// touching the account, newest first. Each row's embedded posting
// descriptions are decrypted (Task 6.2, audit A9.3) exactly like
// ByTransaction above.
func (s *AuditService) ByAccount(ctx context.Context, tenantID, accountID string, after *domain.StatementCursor, limit int) ([]domain.AuditEntry, error) {
	entries, err := s.repo.ListAuditByAccount(ctx, tenantID, accountID, after, limit)
	if err != nil {
		return nil, err
	}
	return decryptAuditEntries(ctx, s.cipher, tenantID, entries)
}

// List returns up to limit of the tenant's audit rows, newest first,
// keyset-paged. Decrypts any encrypted fields the same as ByAccount.
func (s *AuditService) List(ctx context.Context, tenantID string, after *domain.StatementCursor, limit int) ([]domain.AuditEntry, error) {
	entries, err := s.repo.ListAudit(ctx, tenantID, after, limit)
	if err != nil {
		return nil, err
	}
	return decryptAuditEntries(ctx, s.cipher, tenantID, entries)
}

// VerifyResult is the outcome of walking a tenant's audit hash chain
// (ADR-012, "A per-tenant, tamper-evident audit chain"). Checked is how many
// rows were confirmed to chain correctly before the walk stopped: the full
// chain length when Valid is true, or the count up to and including the first
// broken row when Valid is false. FirstBreakID is empty when Valid is true.
//
// Pending is the number of the tenant's audit_outbox rows the background
// chainer has not yet processed (ADR-017): events that are durably posted
// but not yet reflected in the rows Checked walked. The chain's
// tamper-evidence guarantee is unchanged for everything it has processed;
// Pending is how a caller sees whether the chain is current or lagging, not
// a sign anything is wrong by itself (see internal/audit.Chainer and
// ADR-017 section 5).
type VerifyResult struct {
	Valid        bool
	Checked      int
	FirstBreakID string
	Pending      int
}

// Verify walks tenantID's ENTIRE audit chain, oldest first from genesis, and
// recomputes every row's hash from its own stored content and its
// predecessor's stored hash, the same recomputation domain.ComputeAuditRowHash
// performs when a row is first appended. It stops at the first row whose
// stored PrevHash or RowHash does not match what recomputation expects: that
// row (or the one before it) was altered after the fact. An empty chain is
// valid by definition (nothing to break). It also reports Pending: the count
// of the tenant's outbox rows not yet chained, so a caller can tell a short
// chain (nothing wrong, the chainer just has not caught up yet) from a chain
// that legitimately has no more events.
//
// Verify pages through the chain in batches of s.pageSize (Task 5.3, audit
// A2.4) rather than loading every row into memory at once
// (ListAuditForVerify's approach): memory use is therefore bounded regardless
// of chain length, at the cost of one round trip per page instead of one for
// the whole chain. The tamper-detection result is identical either way; only
// the memory/round-trip profile changes.
//
// A caller that only needs to confirm nothing has changed SINCE the last
// off-box anchor, at a cost bounded by growth since then rather than total
// chain length, should call VerifyFromLatestAnchor instead. Verify itself is
// still what a caller needs for the complete, from-genesis guarantee: it is
// the only one of the two that can ever detect a rewrite of history that
// predates every anchor ever taken (see VerifyFromLatestAnchor's own doc
// comment for the trust boundary that trades away).
func (s *AuditService) Verify(ctx context.Context, tenantID string) (VerifyResult, error) {
	return s.verifyFrom(ctx, tenantID, domain.AuditGenesisHash, 0)
}

// VerifyFromLatestAnchor verifies tenantID's chain starting from its most
// recently recorded off-box anchor (Task 5.3), instead of genesis: it reads
// the latest audit_anchors row and, when one exists, walks only the rows
// with chain_seq greater than the anchor's, seeding prev with the anchor's
// own row_hash rather than recomputing anything at or before it. This bounds
// the work to growth since the last anchor, not the chain's total length,
// which matters once a chain is old enough that a full Verify is itself an
// expensive, if still memory-bounded, walk.
//
// This is a genuine trust boundary, not a free lunch: the anchored prefix is
// TRUSTED as a checkpoint, not re-proven. Trusting it is meaningful only
// because the anchor job (internal/audit.AnchorJob) both records it here AND
// emits it as a structured log line an off-box shipper captures (Task 5.6):
// that external, append-only copy is the actual ground truth for the
// anchored prefix, and a caller (or an automated monitor) comparing the live
// head this package's callers can read (domain.Repository.GetAuditHead)
// against the last shipped anchor line is how a rewrite of already-anchored
// history is caught, since a privileged rewrite that also recomputes every
// downstream hash consistently would otherwise leave THIS in-database check
// with nothing to find (see migration 0025's own doc comment for why). A
// caller that needs the complete, from-genesis, no-external-trust guarantee
// must call Verify, not this method.
//
// When the tenant has no anchor yet (a brand-new tenant, or one that posted
// before the anchor job's first tick), this falls back to a full Verify from
// genesis: there is no checkpoint to trust yet, so there is nothing to gain
// by pretending otherwise.
func (s *AuditService) VerifyFromLatestAnchor(ctx context.Context, tenantID string) (VerifyResult, error) {
	anchor, ok, err := s.repo.LatestAuditAnchor(ctx, tenantID)
	if err != nil {
		return VerifyResult{}, err
	}
	if !ok {
		return s.Verify(ctx, tenantID)
	}
	return s.verifyFrom(ctx, tenantID, anchor.RowHash, anchor.ChainSeq)
}

// verifyFrom is Verify's and VerifyFromLatestAnchor's shared paging walk: it
// starts the chain-recomputation with prev already at startHash (the true
// AuditGenesisHash for a from-genesis walk, or a trusted anchor's row_hash
// for a from-anchor one) and reads only rows with chain_seq strictly greater
// than startChainSeq, paging s.pageSize rows at a time until a page returns
// fewer rows than requested. Checked counts only the rows this call actually
// walked (the tail, for a from-anchor call), not the tenant's total chain
// length.
func (s *AuditService) verifyFrom(ctx context.Context, tenantID, startHash string, startChainSeq int64) (VerifyResult, error) {
	pending, err := s.repo.CountPendingOutbox(ctx, tenantID)
	if err != nil {
		return VerifyResult{}, err
	}

	prev := startHash
	after := startChainSeq
	checked := 0
	for {
		rows, err := s.repo.ListAuditForVerifyPage(ctx, tenantID, after, s.pageSize)
		if err != nil {
			return VerifyResult{}, err
		}
		for _, row := range rows {
			checked++
			if row.PrevHash != prev || row.RowHash != domain.ComputeAuditRowHash(tenantID, row, prev) {
				return VerifyResult{Valid: false, Checked: checked, FirstBreakID: row.ID, Pending: pending}, nil
			}
			prev = row.RowHash
			after = row.ChainSeq
		}
		if len(rows) < s.pageSize {
			return VerifyResult{Valid: true, Checked: checked, Pending: pending}, nil
		}
	}
}

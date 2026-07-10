package ledger

import (
	"context"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// AuditService reads the append-only audit log. It is a thin read-through to the
// repository: the audit rows are written transactionally by TransactionService,
// so this service only queries them.
type AuditService struct {
	repo domain.Repository
}

// NewAuditService returns an AuditService backed by repo.
func NewAuditService(repo domain.Repository) *AuditService {
	return &AuditService{repo: repo}
}

// ByTransaction returns the audit rows for a transaction, oldest first.
func (s *AuditService) ByTransaction(ctx context.Context, tenantID, transactionID string) ([]domain.AuditEntry, error) {
	return s.repo.ListAuditByTransaction(ctx, tenantID, transactionID)
}

// ByAccount returns one keyset page of audit rows for every transaction
// touching the account, newest first.
func (s *AuditService) ByAccount(ctx context.Context, tenantID, accountID string, after *domain.StatementCursor, limit int) ([]domain.AuditEntry, error) {
	return s.repo.ListAuditByAccount(ctx, tenantID, accountID, after, limit)
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

// Verify walks tenantID's audit chain oldest first and recomputes every row's
// hash from its own stored content and its predecessor's stored hash, the
// same recomputation domain.ComputeAuditRowHash performs when a row is first
// appended. It stops at the first row whose stored PrevHash or RowHash does
// not match what recomputation expects: that row (or the one before it) was
// altered after the fact. An empty chain is valid by definition (nothing to
// break). It also reports Pending: the count of the tenant's outbox rows not
// yet chained, so a caller can tell a short chain (nothing wrong, the
// chainer just has not caught up yet) from a chain that legitimately has no
// more events.
func (s *AuditService) Verify(ctx context.Context, tenantID string) (VerifyResult, error) {
	rows, err := s.repo.ListAuditForVerify(ctx, tenantID)
	if err != nil {
		return VerifyResult{}, err
	}
	pending, err := s.repo.CountPendingOutbox(ctx, tenantID)
	if err != nil {
		return VerifyResult{}, err
	}

	prev := domain.AuditGenesisHash
	for i, row := range rows {
		checked := i + 1
		if row.PrevHash != prev || row.RowHash != domain.ComputeAuditRowHash(tenantID, row, prev) {
			return VerifyResult{Valid: false, Checked: checked, FirstBreakID: row.ID, Pending: pending}, nil
		}
		prev = row.RowHash
	}
	return VerifyResult{Valid: true, Checked: len(rows), Pending: pending}, nil
}

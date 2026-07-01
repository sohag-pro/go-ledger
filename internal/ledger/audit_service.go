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

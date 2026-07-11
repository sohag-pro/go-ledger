package ledger

import (
	"context"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// DisputeActionReverse and DisputeActionReject are the two valid actions
// POST /v1/disputes/{id}/resolve accepts (Task 6.3, audit A9.2). Reverse
// posts a real reversal of the disputed transaction through
// TransactionService.ReverseTransaction, the normal posting path (screening,
// policy, account status, encryption); reject moves no money at all.
const (
	DisputeActionReverse = "reverse"
	DisputeActionReject  = "reject"
)

// DisputeService is the application service backing the /v1/disputes
// endpoints (Task 6.3, audit A9.2): a dispute/chargeback data model built on
// the reversal primitive (Task 4.2). Opening a dispute records intent only;
// resolving one with DisputeActionReverse posts a real reversal through the
// TransactionService it is constructed with, never a raw insert, so a
// dispute-driven reversal goes through exactly the same screening, policy,
// account-status, and encryption checks a caller-initiated reversal does.
type DisputeService struct {
	repo domain.Repository
	txns *TransactionService
}

// NewDisputeService returns a DisputeService backed by repo, resolving
// DisputeActionReverse through txns.ReverseTransaction.
func NewDisputeService(repo domain.Repository, txns *TransactionService) *DisputeService {
	return &DisputeService{repo: repo, txns: txns}
}

// Open records a new dispute against transactionID, status DisputeOpen. It
// returns domain.ErrTransactionNotFound if transactionID names no
// transaction within tenantID (checked explicitly here, via GetTransaction,
// before ever writing a dispute row; the composite FK, migration 0029,
// disputes_txn_fk, enforces the same constraint at the database as a
// backstop), or a validation error if reason is empty or too long.
func (s *DisputeService) Open(ctx context.Context, tenantID, transactionID, reason string) (domain.Dispute, error) {
	if _, err := s.repo.GetTransaction(ctx, tenantID, transactionID); err != nil {
		return domain.Dispute{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.Dispute{}, err
	}
	d := domain.Dispute{
		ID:            id.String(),
		TenantID:      tenantID,
		TransactionID: transactionID,
		Status:        domain.DisputeOpen,
		Reason:        reason,
	}
	if err := d.Validate(); err != nil {
		return domain.Dispute{}, err
	}
	if err := s.repo.CreateDispute(ctx, tenantID, &d); err != nil {
		return domain.Dispute{}, err
	}
	return d, nil
}

// Get returns a dispute, or domain.ErrDisputeNotFound.
func (s *DisputeService) Get(ctx context.Context, tenantID, id string) (domain.Dispute, error) {
	return s.repo.GetDispute(ctx, tenantID, id)
}

// List returns up to limit of the tenant's disputes, newest first, keyset
// paged, optionally filtered by status.
func (s *DisputeService) List(ctx context.Context, tenantID string, status *domain.DisputeStatus, after *domain.StatementCursor, limit int) ([]domain.Dispute, error) {
	return s.repo.ListDisputes(ctx, tenantID, status, after, limit)
}

// Resolve resolves the dispute identified by id with action
// (DisputeActionReverse or DisputeActionReject), or returns
// domain.ErrInvalidDisputeAction for any other value.
//
// It first reads the current dispute and rejects with
// domain.ErrDisputeAlreadyResolved if its Status is already terminal: this
// is the common, non-racing case (a second, sequential resolve attempt),
// caught here before ever touching the reversal path again. A second,
// TRULY CONCURRENT resolve attempt can still race past this check (both
// read Status "open" before either commits); the backstop is
// Repository.ResolveDispute's own guarded UPDATE ... WHERE status = 'open',
// which the second attempt's commit will find already flipped and report
// the same ErrDisputeAlreadyResolved. In that race, both attempts may call
// ReverseTransaction, but that call is itself idempotent (Task 4.2): the
// loser gets back the SAME reversal the winner posted, so no double
// reversal, and no double money movement, is ever possible either way.
//
// For DisputeActionReverse, the reversal is posted through
// TransactionService.ReverseTransaction BEFORE the dispute row is updated:
// this is the real posting path (screening, policy, account status,
// encryption), never a raw insert. If the disputed transaction was already
// reversed by some other means (for example a direct POST
// /v1/transactions/{id}/reverse call before the dispute was resolved),
// ReverseTransaction's own idempotency reuses that existing reversal rather
// than posting a second one.
func (s *DisputeService) Resolve(ctx context.Context, tenantID, id, action string) (domain.Dispute, error) {
	if action != DisputeActionReverse && action != DisputeActionReject {
		return domain.Dispute{}, domain.ErrInvalidDisputeAction
	}
	d, err := s.repo.GetDispute(ctx, tenantID, id)
	if err != nil {
		return domain.Dispute{}, err
	}
	if d.Status.Terminal() {
		return domain.Dispute{}, domain.ErrDisputeAlreadyResolved
	}

	if action == DisputeActionReject {
		return s.repo.ResolveDispute(ctx, tenantID, id, domain.DisputeResolvedRejected, nil)
	}

	// action == DisputeActionReverse: post the real reversal first, through
	// the normal posting path, before ever touching the dispute row.
	reversal, _, err := s.txns.ReverseTransaction(ctx, tenantID, d.TransactionID)
	if err != nil {
		return domain.Dispute{}, err
	}
	return s.repo.ResolveDispute(ctx, tenantID, id, domain.DisputeResolvedReversed, &reversal.ID)
}

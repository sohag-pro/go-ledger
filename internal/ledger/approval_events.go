package ledger

import (
	"context"
	"encoding/json"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// appendPendingEvent writes a v2 lifecycle audit event for a pending
// transition (ADR-025): subject is the pending, transaction_id is empty
// except for approval.approved (passed as txID). actor is the individual
// principal that drove the transition (the creator for approval.requested,
// the decider for approve/reject/cancel, "system" for the expiry sweep), so
// the tamper-evident log attributes each lifecycle event to a specific key
// rather than to the whole tenant.
func appendPendingEvent(ctx context.Context, tx domain.Tx, tenantID, actor, action string, p *domain.PendingTransaction, txID *string) error {
	after, err := json.Marshal(map[string]any{
		"id":            p.ID,
		"kind":          p.Kind,
		"status":        p.Status,
		"threshold_ccy": p.ThresholdCcy,
		"threshold_amt": p.ThresholdAmt,
		"created_by":    p.CreatedBy,
	})
	if err != nil {
		return err
	}
	ev := domain.AuditEvent{
		Action:      action,
		Actor:       actor,
		After:       after,
		SubjectType: "pending_transaction",
		SubjectID:   p.ID,
		HashVersion: domain.AuditHashV2,
	}
	if txID != nil {
		ev.TransactionID = *txID
	}
	return tx.AppendAuditOutbox(ctx, tenantID, ev)
}

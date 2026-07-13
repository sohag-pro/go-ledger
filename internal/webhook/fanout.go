package webhook

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// fanOutBatch reads up to cfg.FanoutBatch audit_log rows past the fan-out
// cursor, creates a pending webhook_deliveries row for every ACTIVE
// subscription (in that event's tenant) whose event-type filter matches the
// event's action, and advances the cursor past every row it read, all in
// one transaction (Task 4.1): the cursor advance and the delivery inserts
// commit together or not at all, so a crash mid-batch leaves the cursor
// exactly where it was and the next attempt reads the same audit_log rows
// again. That replay is safe: InsertWebhookDelivery's ON CONFLICT DO NOTHING
// against the (subscription_id, audit_chain_seq) unique index (migration
// 0021) makes re-inserting the same pairing a no-op, so fan-out is
// exactly-once into webhook_deliveries no matter how many times a batch is
// retried.
//
// It returns how many webhook_deliveries rows were newly created (not how
// many audit_log rows were read: a chained event with no matching active
// subscription still advances the cursor past it but creates zero rows).
func (w *Worker) fanOutBatch(ctx context.Context, db dbtx) (int, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("webhook fan-out: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	q := sqlc.New(tx)

	cursor, err := q.GetWebhookFanoutCursorForUpdate(ctx)
	if err != nil {
		return 0, fmt.Errorf("webhook fan-out: read cursor: %w", err)
	}

	events, err := q.ListAuditLogSinceChainSeq(ctx, sqlc.ListAuditLogSinceChainSeqParams{
		AfterSeq:   cursor,
		BatchLimit: int32(w.cfg.FanoutBatch), //nolint:gosec // FanoutBatch is an application-configured, small positive value
	})
	if err != nil {
		return 0, fmt.Errorf("webhook fan-out: scan audit_log: %w", err)
	}
	if len(events) == 0 {
		return 0, nil
	}

	// Subscriptions for a tenant are read at most once per batch: several
	// audit events in the same batch, for the same tenant, reuse the same
	// cached slice instead of re-querying per row.
	subsByTenant := make(map[uuid.UUID][]sqlc.ListActiveWebhookSubscriptionsByTenantRow)
	created := 0
	maxSeq := cursor
	for _, ev := range events {
		if ev.ChainSeq > maxSeq {
			maxSeq = ev.ChainSeq
		}
		subs, ok := subsByTenant[ev.TenantID]
		if !ok {
			subs, err = q.ListActiveWebhookSubscriptionsByTenant(ctx, ev.TenantID)
			if err != nil {
				return 0, fmt.Errorf("webhook fan-out: list subscriptions for tenant %s: %w", ev.TenantID, err)
			}
			subsByTenant[ev.TenantID] = subs
		}
		for _, sub := range subs {
			domainSub := domain.WebhookSubscription{EventTypes: sub.EventTypes}
			if !domainSub.Matches(ev.Action) {
				continue
			}
			deliveryID, err := uuid.NewV7()
			if err != nil {
				return 0, fmt.Errorf("webhook fan-out: generate delivery id: %w", err)
			}
			payload := domain.WebhookPayload{
				ID:            deliveryID.String(),
				Event:         ev.Action,
				TenantID:      ev.TenantID.String(),
				TransactionID: pgUUIDToString(ev.TransactionID),
				SubjectID:     pgUUIDToString(ev.SubjectID),
				OccurredAt:    ev.CreatedAt,
				Data:          json.RawMessage(ev.After),
			}
			if ev.SubjectType.Valid {
				payload.SubjectType = ev.SubjectType.String
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return 0, fmt.Errorf("webhook fan-out: marshal payload: %w", err)
			}
			rows, err := q.InsertWebhookDelivery(ctx, sqlc.InsertWebhookDeliveryParams{
				ID:             deliveryID,
				TenantID:       ev.TenantID,
				SubscriptionID: sub.ID,
				AuditChainSeq:  ev.ChainSeq,
				EventType:      ev.Action,
				Payload:        body,
			})
			if err != nil {
				return 0, fmt.Errorf("webhook fan-out: insert delivery: %w", err)
			}
			created += int(rows)
		}
	}

	if err := q.SetWebhookFanoutCursor(ctx, maxSeq); err != nil {
		return 0, fmt.Errorf("webhook fan-out: advance cursor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("webhook fan-out: commit: %w", err)
	}
	return created, nil
}

// pgUUIDToString maps a nullable pgtype.UUID column back onto a plain,
// possibly-empty string (ADR-025, migration 0034): "" when the column is
// NULL (a chained non-transaction lifecycle event has no transaction_id, or
// an ordinary transaction event has no subject_id), otherwise its string
// form. pgtype.UUID.String() does NOT check Valid itself (it happily
// formats the zero value), so this must be used instead of calling it
// directly on a nullable column. Both callers rely on WebhookPayload's
// TransactionID and SubjectID being omitempty (Task 10, ADR-025), so this
// empty-string mapping also means the field is omitted from the delivered
// JSON, not sent as a misleading present-but-empty key.
func pgUUIDToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

package ledger

import (
	"context"
	"encoding/json"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// ReverseTransaction posts the negated legs of the transaction identified by
// originalID as a new, linked transaction (domain.Transaction.BuildReversal),
// atomically, inside the same kind of SERIALIZABLE transaction Post uses
// (RunInTx). Postings are append-only (ADR-001): a reversal never mutates
// the original, it is a brand new transaction whose postings undo it.
//
// Not found (originalID names no transaction in tenantID) returns
// domain.ErrTransactionNotFound. Reversing a transaction that is itself a
// reversal (its ReversesTransactionID is already set) returns
// domain.ErrCannotReverseReversal: there is no reversal of a reversal in
// this model, only forward, append-only corrections.
//
// ReverseTransaction is idempotent: a transaction can be reversed at most
// once. The database enforces this (transactions_one_reversal_idx, migration
// 0017), so this is not merely an application-level courtesy. Calling it
// again for an original that already has a reversal returns the SAME
// reversal, with alreadyReversed = true, instead of posting a second one;
// this is checked both up front (before ever attempting to post, mirroring
// Convert's idempotency-key precheck) and via the database's unique index as
// the race guard, so a concurrent double-reverse (two callers racing to
// reverse the same original) still converges on exactly one reversal: the
// loser's insert hits domain.ErrTransactionAlreadyReversed, caught here, and
// the loser reads back the winner's reversal instead of erroring.
//
// Every real reversal also writes one audit_outbox row in the same
// transaction, action domain.ActionTransactionReversed (ADR-017): like Post,
// the tamper-evident audit chain itself is built asynchronously by the
// background chainer, not inside this call.
func (s *TransactionService) ReverseTransaction(ctx context.Context, tenantID, originalID string) (reversal *domain.Transaction, alreadyReversed bool, err error) {
	ctx, span := s.tracer.Start(ctx, "ledger.ReverseTransaction",
		oteltrace.WithAttributes(
			attribute.String("tenant_id", tenantID),
			attribute.String("transaction.original_id", originalID),
		),
	)
	defer span.End()

	original, err := s.repo.GetTransaction(ctx, tenantID, originalID)
	if err != nil {
		span.RecordError(err)
		return nil, false, err
	}
	if original.ReversesTransactionID != nil {
		span.RecordError(domain.ErrCannotReverseReversal)
		span.SetStatus(codes.Error, "cannot reverse a reversal")
		return nil, false, domain.ErrCannotReverseReversal
	}

	// Idempotency precheck, mirroring Convert's idempotency-key precheck
	// before RunInTx (see convertReplay's sibling doc comment): a hit here
	// returns the existing reversal without ever attempting a second post.
	existing, err := s.repo.GetReversalOf(ctx, tenantID, originalID)
	switch {
	case err == nil:
		s.log.InfoContext(ctx, "reversal already exists",
			"tenant_id", tenantID, "original_id", originalID, "reversal_id", existing.ID)
		return &existing, true, nil
	case errors.Is(err, domain.ErrTransactionNotFound):
		// No existing reversal: proceed with a real one.
	default:
		span.RecordError(err)
		return nil, false, err
	}

	t, err := original.BuildReversal("")
	if err != nil {
		span.RecordError(err)
		return nil, false, err
	}
	if err := t.Validate(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "validation failed")
		return nil, false, err
	}

	runErr := s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, tx domain.Tx) error {
		if err := tx.CreateTransaction(ctx, tenantID, &t); err != nil {
			return err
		}
		after, err := json.Marshal(auditSnapshot(&t))
		if err != nil {
			return err
		}
		return tx.AppendAuditOutbox(ctx, tenantID, domain.AuditEvent{
			Action:        domain.ActionTransactionReversed,
			TransactionID: t.ID,
			Actor:         tenantID,
			After:         after,
		})
	})
	if runErr != nil {
		if errors.Is(runErr, domain.ErrTransactionAlreadyReversed) {
			// A concurrent reverse for this original committed between our
			// precheck and this attempt's insert: transactions_one_reversal_idx
			// is what caught it. Read back the winner's reversal exactly as
			// the precheck above would have.
			existing, err := s.repo.GetReversalOf(ctx, tenantID, originalID)
			if err != nil {
				span.RecordError(err)
				return nil, false, err
			}
			return &existing, true, nil
		}
		span.RecordError(runErr)
		span.SetStatus(codes.Error, "reverse failed")
		s.log.ErrorContext(ctx, "reverse transaction failed",
			"tenant_id", tenantID, "original_id", originalID, "error", runErr)
		return nil, false, runErr
	}

	s.log.InfoContext(ctx, "transaction reversed",
		"tenant_id", tenantID, "original_id", originalID, "reversal_id", t.ID)
	return &t, false, nil
}

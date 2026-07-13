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
//
// A reversal posts real money (the negated legs plus the audit row), exactly
// like Post and Convert, so it goes through the same PrePostHook screening
// gate those two use (Task 6.1 gap fix, audit A9.1): the fully-built reversal
// transaction is reviewed synchronously, immediately before RunInTx, so a
// rejection or an ambiguous hook failure both return here with nothing
// written, the same fail-closed guarantee Post and Convert give. This only
// runs for a genuinely NEW reversal: the idempotent "already reversed" path
// above (existing found via GetReversalOf, and the ErrTransactionAlreadyReversed
// race-guard branch below) posts nothing new, so it is never re-screened, the
// same replay exemption reviewPost's own callers already give Post and
// Convert.
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
	existing, err := s.getReversalOfDecrypted(ctx, tenantID, originalID)
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

	// Screening runs on the fully-built reversal, synchronously, and BEFORE
	// RunInTx opens any transaction (Task 6.1 gap fix, audit A9.1): a
	// rejection or an ambiguous hook failure both return here, before a
	// single row (the reversal's postings or its audit_outbox row) is
	// written. A reversal moves real money, exactly like Post and Convert, so
	// a party a screening system would block must not be able to receive
	// funds through a reversal either.
	if err := reviewPost(ctx, s.prePostHook, tenantID, &t); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "screening rejected reversal")
		return nil, false, err
	}

	// Approval gate (ADR-025, Task 6), same as Post and Convert: an
	// over-threshold reversal is held as a pending instead of posted, unless
	// this is the approval replay of an already-cleared pending. A reversal
	// gets one exemption Post and Convert do not need: if the ORIGINAL
	// transaction being reversed already cleared the gate (an approved
	// pending produced it), the reversal is not held again, because that
	// money was already approved once and a reversal is just undoing it, not
	// moving new, never-reviewed funds.
	if !isApprovalReplay(ctx) {
		if ccy, amt, gated := s.approval.Gate(t.Postings); gated {
			approved, err := s.repo.PendingApprovedForTransaction(ctx, tenantID, originalID)
			if err != nil {
				span.RecordError(err)
				return nil, false, err
			}
			if !approved {
				// ReverseTransaction takes no caller-supplied idempotency
				// key (its own idempotency comes from the
				// transactions_one_reversal_idx unique constraint on
				// reverses_transaction_id, not a request header), so there
				// is nothing to dedup a held reversal's pending on here.
				return nil, false, s.holdForApproval(ctx, tenantID, tenantID, domain.PendingKindReverse, reversePayload(originalID), ccy, amt, "")
			}
		}
	}

	// Encrypt-once, same as Post and Convert (Task 6.2, audit A9.3): see
	// Post's own doc comment at its matching call site in service.go. The
	// reversal's narration ("reversal of <original id>", see BuildReversal)
	// is not free-text a caller supplied, but it is still encrypted
	// uniformly with every other posting description, so a read of a
	// reversal's history behaves identically whether or not its own
	// narration happens to be caller-supplied text.
	originalPostings := t.Postings
	if s.cipher != nil {
		encrypted, err := encryptPostings(ctx, s.cipher, tenantID, t.Postings)
		if err != nil {
			span.RecordError(err)
			return nil, false, err
		}
		t.Postings = encrypted
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
	// Restore the caller's plaintext narration regardless of outcome; see
	// Post's matching restore in service.go for why this is safe even on the
	// already-reversed race-guard branch just below, which overwrites the
	// returned *domain.Transaction entirely from a fresh, decrypted fetch.
	t.Postings = originalPostings

	if runErr != nil {
		if errors.Is(runErr, domain.ErrTransactionAlreadyReversed) {
			// A concurrent reverse for this original committed between our
			// precheck and this attempt's insert: transactions_one_reversal_idx
			// is what caught it. Read back the winner's reversal exactly as
			// the precheck above would have.
			existing, err := s.getReversalOfDecrypted(ctx, tenantID, originalID)
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

// getReversalOfDecrypted fetches originalID's reversal and decrypts its
// postings' descriptions (Task 6.2, audit A9.3), the GetReversalOf
// counterpart to TransactionService.getDecrypted (service.go). err is
// returned unchanged when the fetch itself fails (in particular,
// domain.ErrTransactionNotFound passes through exactly as
// s.repo.GetReversalOf would report it), so callers that switch on
// errors.Is(err, domain.ErrTransactionNotFound) keep working unmodified.
func (s *TransactionService) getReversalOfDecrypted(ctx context.Context, tenantID, originalID string) (domain.Transaction, error) {
	t, err := s.repo.GetReversalOf(ctx, tenantID, originalID)
	if err != nil {
		return domain.Transaction{}, err
	}
	return s.decryptTransaction(ctx, tenantID, t)
}

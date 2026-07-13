package ledger

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// ApprovalService decides a pending transaction Task 6's approval gate held
// (ADR-025): Approve replays the pending's stored payload through the normal
// post path (Post, Convert, or ReverseTransaction) against CURRENT balances,
// links the resulting transaction, and emits an approval.approved lifecycle
// event; Reject and Cancel close a pending out without ever posting money.
// Every transition row-locks the pending (GetPendingForUpdate) so two
// racing decisions on the same pending cannot both proceed.
type ApprovalService struct {
	repo domain.Repository
	txns *TransactionService
	cfg  ApprovalConfig
}

// NewApprovalService returns an ApprovalService backed by repo (for the
// pending's own storage) and txns (the normal post path a replay runs
// through), governed by cfg (the same ApprovalConfig txns itself was built
// with, for RequireDifferentActor).
func NewApprovalService(repo domain.Repository, txns *TransactionService, cfg ApprovalConfig) *ApprovalService {
	return &ApprovalService{repo: repo, txns: txns, cfg: cfg}
}

// lockAndCheck opens its own transaction, row-locks the pending
// (GetPendingForUpdate), and returns it. A pending already in a rejected,
// cancelled, or expired state is terminal and decided at most once, so this
// returns domain.ErrPendingAlreadyDecided for any of those. An
// already-approved pending is deliberately NOT rejected here: Approve's
// whole point is that a second Approve is a no-op returning the same
// transaction, while Reject and Cancel treat "already approved" as any
// other terminal state and check for it themselves after calling this.
//
// The row lock this takes is only held for the duration of this one short
// transaction, not across whatever the caller does next (a replay, in
// Approve's case, runs its own separate RunInTx inside Post/Convert/
// ReverseTransaction). That is intentional, not a gap: see replay's and
// Approve's own doc comments for how the pending's own idempotency key
// (derived from its id) is what actually keeps two racing or crash-retried
// decisions on the same pending from ever posting twice, not this lock by
// itself.
func (s *ApprovalService) lockAndCheck(ctx context.Context, tenantID, id string) (*domain.PendingTransaction, error) {
	var pending *domain.PendingTransaction
	err := s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, tx domain.Tx) error {
		p, err := tx.GetPendingForUpdate(ctx, tenantID, id)
		if err != nil {
			return err
		}
		pending = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	switch pending.Status {
	case domain.PendingStatusPending, domain.PendingStatusApproved:
		return pending, nil
	default:
		return nil, domain.ErrPendingAlreadyDecided
	}
}

// approvalIdempotencyKey derives a deterministic idempotency key from the
// pending's own id: every replay of the same pending, whether from a second
// Approve call racing the first, or a retry after a crash between the
// replay's own post and the second transaction that marks the pending
// approved, reuses this exact key. Post and Convert's own idempotency
// machinery (GetIdempotencyKey's precheck, and the unique index backing it
// for the genuine race window) is what actually guarantees the pending is
// posted at most once; this key is what plugs a pending decision into that
// already-proven mechanism instead of inventing a new one.
func approvalIdempotencyKey(pendingID string) *domain.Idempotency {
	return &domain.Idempotency{Key: "approval:" + pendingID}
}

// replay unmarshals pending's stored payload and dispatches it to the
// matching TransactionService entry point, running as an approval replay
// (ctx must already carry withApprovalReplay) so the approval gate itself is
// not re-triggered. It validates against CURRENT state exactly like a
// brand-new call to that entry point would: an FX convert resolves a fresh
// rate rather than replaying a stale one, and a plain post is re-checked
// against the account's current status and minimum balance, which were
// never checked at hold time (the gate fires before either check).
func (s *ApprovalService) replay(ctx context.Context, tenantID string, p *domain.PendingTransaction) (*domain.Transaction, error) {
	switch p.Kind {
	case domain.PendingKindPost:
		var body postPayloadBody
		if err := json.Unmarshal(p.Payload, &body); err != nil {
			return nil, fmt.Errorf("ledger: unmarshal pending %s post payload: %w", p.ID, err)
		}
		postings := make([]domain.Posting, 0, len(body.Postings))
		for _, leg := range body.Postings {
			amt, err := domain.NewMoney(leg.Amount, domain.Currency(leg.Currency))
			if err != nil {
				return nil, err
			}
			postings = append(postings, domain.Posting{
				AccountID:   leg.AccountID,
				Amount:      amt,
				Description: leg.Description,
			})
		}
		t := &domain.Transaction{
			Postings:    postings,
			Reference:   body.Reference,
			EffectiveAt: body.EffectiveAt,
		}
		if _, err := s.txns.Post(ctx, tenantID, t, approvalIdempotencyKey(p.ID)); err != nil {
			return nil, err
		}
		return t, nil

	case domain.PendingKindConvert:
		var body convertPayloadBody
		if err := json.Unmarshal(p.Payload, &body); err != nil {
			return nil, fmt.Errorf("ledger: unmarshal pending %s convert payload: %w", p.ID, err)
		}
		tx, _, err := s.txns.Convert(ctx, tenantID, ConvertRequest(body), approvalIdempotencyKey(p.ID))
		if err != nil {
			return nil, err
		}
		return tx, nil

	case domain.PendingKindReverse:
		var body reversePayloadBody
		if err := json.Unmarshal(p.Payload, &body); err != nil {
			return nil, fmt.Errorf("ledger: unmarshal pending %s reverse payload: %w", p.ID, err)
		}
		// ReverseTransaction takes no idempotency key: it is already
		// idempotent on the original transaction id alone (GetReversalOf's
		// precheck, backed by the transactions_one_reversal_idx unique
		// index), the same guarantee approvalIdempotencyKey gives Post and
		// Convert here.
		tx, _, err := s.txns.ReverseTransaction(ctx, tenantID, body.ReversedTransactionID)
		if err != nil {
			return nil, err
		}
		return tx, nil

	default:
		return nil, fmt.Errorf("ledger: pending %s has unrecognized kind %q", p.ID, p.Kind)
	}
}

// Approve locks the pending, validates the transition, and (if still
// pending) replays the original request through the normal post path
// against CURRENT state. The replay runs with withApprovalReplay so it is
// not re-gated.
//
// CRASH-SAFETY: the replay posts the transaction in its own RunInTx (inside
// Post/Convert/ReverseTransaction); marking the pending approved and linking
// the transaction id is a SECOND, separate transaction below. A crash
// between the two leaves a posted transaction with a still-pending row. The
// next Approve call is safe regardless of which side of that gap it lands
// on: if the second transaction already committed, lockAndCheck's re-read
// sees Status == approved and this returns the linked transaction without
// replaying; if only the first committed, replay calls Post/Convert again
// with the SAME derived idempotency key (approvalIdempotencyKey), so it
// replays the already-posted transaction instead of creating a second one,
// and the second transaction then runs (for the first time) to mark the
// pending approved. Either way a second Approve is a no-op that returns the
// same transaction id.
func (s *ApprovalService) Approve(ctx context.Context, tenantID, id, actor string) (*domain.Transaction, error) {
	pending, err := s.lockAndCheck(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if pending.Status == domain.PendingStatusApproved {
		if pending.TransactionID == nil {
			return nil, fmt.Errorf("ledger: pending %s is approved but has no linked transaction", id)
		}
		tx, err := s.txns.Get(ctx, tenantID, *pending.TransactionID)
		if err != nil {
			return nil, err
		}
		return &tx, nil
	}
	if s.cfg.RequireDifferentActor && actor == pending.CreatedBy {
		return nil, domain.ErrCannotApproveOwn
	}

	tx, err := s.replay(withApprovalReplay(ctx), tenantID, pending)
	if err != nil {
		// Validation failed against current state (for example a
		// MinBalanceBreachError or AccountNotActiveError): nothing was
		// posted (Post/Convert/ReverseTransaction's own RunInTx rolled back),
		// and the pending stays pending, untouched, ready for a retry once
		// the underlying condition clears.
		return nil, err
	}

	err = s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, dtx domain.Tx) error {
		txID := tx.ID
		if err := dtx.UpdatePendingStatus(ctx, tenantID, id, domain.PendingStatusApproved, actor, nil, &txID); err != nil {
			return err
		}
		pending.Status = domain.PendingStatusApproved
		return appendPendingEvent(ctx, dtx, tenantID, "approval.approved", pending, &txID)
	})
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// Reject locks the pending, refuses an already-decided one (including an
// already-approved one) with domain.ErrPendingAlreadyDecided, and otherwise
// moves it to rejected with reason, atomically with an approval.rejected
// lifecycle event. Nothing is ever posted for a rejected pending.
func (s *ApprovalService) Reject(ctx context.Context, tenantID, id, actor string, reason *string) error {
	pending, err := s.lockAndCheck(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if pending.Status != domain.PendingStatusPending {
		return domain.ErrPendingAlreadyDecided
	}

	return s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, dtx domain.Tx) error {
		if err := dtx.UpdatePendingStatus(ctx, tenantID, id, domain.PendingStatusRejected, actor, reason, nil); err != nil {
			return err
		}
		pending.Status = domain.PendingStatusRejected
		pending.Reason = reason
		return appendPendingEvent(ctx, dtx, tenantID, "approval.rejected", pending, nil)
	})
}

// Cancel locks the pending, requires actor to be the pending's own creator
// (domain.ErrNotPendingCreator otherwise), refuses an already-decided one
// with domain.ErrPendingAlreadyDecided, and otherwise moves it to cancelled,
// atomically with an approval.cancelled lifecycle event. Nothing is ever
// posted for a cancelled pending.
func (s *ApprovalService) Cancel(ctx context.Context, tenantID, id, actor string) error {
	pending, err := s.lockAndCheck(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if pending.Status != domain.PendingStatusPending {
		return domain.ErrPendingAlreadyDecided
	}
	if actor != pending.CreatedBy {
		return domain.ErrNotPendingCreator
	}

	return s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, dtx domain.Tx) error {
		if err := dtx.UpdatePendingStatus(ctx, tenantID, id, domain.PendingStatusCancelled, actor, nil, nil); err != nil {
			return err
		}
		pending.Status = domain.PendingStatusCancelled
		return appendPendingEvent(ctx, dtx, tenantID, "approval.cancelled", pending, nil)
	})
}

// Get returns the pending transaction with the given id within the tenant,
// or domain.ErrPendingTransactionNotFound. A thin read-through to the
// repository, the same shape TransactionService.Get already uses.
func (s *ApprovalService) Get(ctx context.Context, tenantID, id string) (*domain.PendingTransaction, error) {
	return s.repo.GetPendingTransaction(ctx, tenantID, id)
}

// List returns up to limit of the tenant's pending transactions, optionally
// filtered by status, keyset paged from after. A thin read-through to the
// repository, the same shape TransactionService.ListTransactions already
// uses.
func (s *ApprovalService) List(ctx context.Context, tenantID string, status *domain.PendingStatus, after *domain.StatementCursor, limit int) ([]domain.PendingTransaction, error) {
	return s.repo.ListPendingTransactions(ctx, tenantID, status, after, limit)
}

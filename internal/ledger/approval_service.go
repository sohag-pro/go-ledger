package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// ApprovalService decides a pending transaction Task 6's approval gate held
// (ADR-025): Approve replays the pending's stored payload through the normal
// post path (Post, Convert, or ReverseTransaction) against CURRENT balances,
// links the resulting transaction, and emits an approval.approved lifecycle
// event; Reject and Cancel close a pending out without ever posting money.
// Every transition row-locks the pending (GetPendingForUpdate) and holds
// that lock across its own status write, so two racing decisions on the same
// pending are serialized: the loser blocks on the lock until the winner
// commits, then re-reads a now-terminal row instead of racing an unguarded
// write against it. See lockedTransition (Reject, Cancel) and Approve's own
// doc comment (which cannot use lockedTransition directly: its write has to
// happen on the far side of a replay that must NOT run with the row locked).
type ApprovalService struct {
	repo domain.Repository
	txns *TransactionService
	cfg  ApprovalConfig
	log  *slog.Logger
}

// NewApprovalService returns an ApprovalService backed by repo (for the
// pending's own storage) and txns (the normal post path a replay runs
// through), governed by cfg (the same ApprovalConfig txns itself was built
// with, for RequireDifferentActor). If log is nil the default slog logger is
// used (matching NewTransactionService).
func NewApprovalService(repo domain.Repository, txns *TransactionService, cfg ApprovalConfig, log *slog.Logger) *ApprovalService {
	if log == nil {
		log = slog.Default()
	}
	return &ApprovalService{repo: repo, txns: txns, cfg: cfg, log: log}
}

// approvalIdempotencyKey derives a deterministic idempotency key from the
// pending's own id: every replay of the same pending, whether from a second
// Approve call racing the first, or a retry after a crash between the
// replay's own post and the later transaction that marks the pending
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
// This runs in three phases instead of one lockedTransition, because unlike
// Reject and Cancel, the write in the middle (the replay) must NOT run with
// the pending row locked: it opens its own RunInTx inside Post/Convert/
// ReverseTransaction, and holding the pending's row lock across that would
// serialize every concurrent Approve/Reject/Cancel on the SAME pending
// behind the replay's full posting latency for no reason (worse, it would
// invite a lock-ordering deadlock against whatever the replay itself locks).
// Instead, each phase that touches the row takes and releases its own lock,
// and Phase C is what makes the sequence race-safe overall:
//
//   - Phase A (locked): read the pending. Already approved -> return its
//     linked transaction (idempotent no-op, no replay). Any other terminal
//     status -> ErrPendingAlreadyDecided. Four-eyes violation -> ErrCannotApproveOwn.
//     Otherwise (pending) -> fall through to the replay, lock released.
//   - Phase B (unlocked): replay. On error, nothing posted, pending
//     untouched, return the error.
//   - Phase C (re-locked): re-read the pending. A concurrent decision may
//     have run to completion in the window between Phase A's lock release
//     and this lock's acquisition:
//     -- already approved: a concurrent Approve's replay landed first (its
//     replay resolves the SAME idempotency key, so it posted the SAME
//     transaction ours did); leave the row alone and do not emit a
//     second approval.approved event.
//     -- still pending: the common case; write approved + link + event now,
//     under this lock.
//     -- rejected/cancelled/expired: a concurrent decision terminalized the
//     pending WHILE this replay was posting. The transaction above is
//     already posted and cannot be un-posted, so POST WINS: force the
//     pending back to approved and linked, so a posted transaction is
//     never left orphaned under a terminal non-approved pending. This is
//     an exceptional path; it is logged as a warning.
//
// CRASH-SAFETY: a crash between Phase B and Phase C leaves a posted
// transaction with a still-pending row. The next Approve call is safe
// regardless of which side of that gap it lands on: if Phase C already
// committed, Phase A's re-read sees Status == approved and returns the
// linked transaction without replaying; if only Phase B committed, the
// retry's own Phase B calls Post/Convert again with the SAME derived
// idempotency key (approvalIdempotencyKey), so it replays the
// already-posted transaction instead of creating a second one, and its
// Phase C then runs (for the first time) to mark the pending approved.
func (s *ApprovalService) Approve(ctx context.Context, tenantID, id, actor string) (*domain.Transaction, error) {
	// Phase A: lock, read, validate the transition.
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
	case domain.PendingStatusApproved:
		if pending.TransactionID == nil {
			return nil, fmt.Errorf("ledger: pending %s is approved but has no linked transaction", id)
		}
		tx, err := s.txns.Get(ctx, tenantID, *pending.TransactionID)
		if err != nil {
			return nil, err
		}
		return &tx, nil
	case domain.PendingStatusPending:
		// Proceed to the four-eyes check and replay below.
	default:
		return nil, domain.ErrPendingAlreadyDecided
	}

	if s.cfg.RequireDifferentActor && actor == pending.CreatedBy {
		return nil, domain.ErrCannotApproveOwn
	}

	// Phase B: replay, unlocked (see the doc comment above for why).
	tx, err := s.replay(withApprovalReplay(ctx), tenantID, pending)
	if err != nil {
		// Validation failed against current state (for example a
		// MinBalanceBreachError or AccountNotActiveError): nothing was
		// posted (Post/Convert/ReverseTransaction's own RunInTx rolled back),
		// and the pending stays pending, untouched, ready for a retry once
		// the underlying condition clears.
		return nil, err
	}
	txID := tx.ID

	// Phase C: re-lock and resolve against whatever the row looks like now.
	err = s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, dtx domain.Tx) error {
		current, err := dtx.GetPendingForUpdate(ctx, tenantID, id)
		if err != nil {
			return err
		}
		switch current.Status {
		case domain.PendingStatusApproved:
			// A concurrent Approve already committed Phase C first. Its
			// replay resolved the same idempotency key ours did, so it
			// posted the exact same transaction: leave the row alone and
			// do not emit a second approval.approved event.
			return nil
		case domain.PendingStatusPending:
			if err := dtx.UpdatePendingStatus(ctx, tenantID, id, domain.PendingStatusApproved, actor, nil, &txID); err != nil {
				return err
			}
			current.Status = domain.PendingStatusApproved
			return appendPendingEvent(ctx, dtx, tenantID, actor, "approval.approved", current, &txID)
		default:
			// rejected, cancelled, or expired: a concurrent decision
			// terminalized this pending while our replay above was
			// posting the transaction. That transaction is real money
			// already moved and cannot be un-posted, so POST WINS: force
			// the pending back to approved and linked to it rather than
			// leave a terminal, non-approved pending pointing at nothing
			// while a live posted transaction has no pending to show for
			// it. This is an exceptional race, not routine behavior.
			s.log.Warn("ledger: approval post-wins race: a concurrent decision terminalized a pending while its replay was posting; forcing it back to approved so the posted transaction is not orphaned",
				"tenant_id", tenantID,
				"pending_id", id,
				"transaction_id", txID,
				"overridden_status", string(current.Status),
				"actor", actor,
			)
			if err := dtx.UpdatePendingStatus(ctx, tenantID, id, domain.PendingStatusApproved, actor, nil, &txID); err != nil {
				return err
			}
			current.Status = domain.PendingStatusApproved
			return appendPendingEvent(ctx, dtx, tenantID, actor, "approval.approved", current, &txID)
		}
	})
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// lockedTransition runs a Reject- or Cancel-shaped decision as a single
// RunInTx: lock the pending (GetPendingForUpdate), let check validate the
// transition against the locked row (returning any error, most commonly
// domain.ErrPendingAlreadyDecided or domain.ErrNotPendingCreator, to abort
// before anything is written), then write. The lock is held from the read
// through the write, so a concurrent decision on the same pending is
// serialized out: the loser blocks on the lock until the winner's
// transaction commits, then check sees the now-terminal row and refuses.
func (s *ApprovalService) lockedTransition(
	ctx context.Context,
	tenantID, id string,
	check func(p *domain.PendingTransaction) error,
	write func(ctx context.Context, tx domain.Tx, p *domain.PendingTransaction) error,
) error {
	return s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, tx domain.Tx) error {
		pending, err := tx.GetPendingForUpdate(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if err := check(pending); err != nil {
			return err
		}
		return write(ctx, tx, pending)
	})
}

// Reject locks the pending, refuses an already-decided one (including an
// already-approved one) with domain.ErrPendingAlreadyDecided, and otherwise
// moves it to rejected with reason, atomically with an approval.rejected
// lifecycle event, all under the SAME row lock. Nothing is ever posted for a
// rejected pending.
func (s *ApprovalService) Reject(ctx context.Context, tenantID, id, actor string, reason *string) error {
	return s.lockedTransition(ctx, tenantID, id,
		func(p *domain.PendingTransaction) error {
			if p.Status != domain.PendingStatusPending {
				return domain.ErrPendingAlreadyDecided
			}
			return nil
		},
		func(ctx context.Context, tx domain.Tx, pending *domain.PendingTransaction) error {
			if err := tx.UpdatePendingStatus(ctx, tenantID, id, domain.PendingStatusRejected, actor, reason, nil); err != nil {
				return err
			}
			pending.Status = domain.PendingStatusRejected
			pending.Reason = reason
			return appendPendingEvent(ctx, tx, tenantID, actor, "approval.rejected", pending, nil)
		},
	)
}

// Cancel locks the pending, requires actor to be the pending's own creator
// (domain.ErrNotPendingCreator otherwise), refuses an already-decided one
// with domain.ErrPendingAlreadyDecided, and otherwise moves it to cancelled,
// atomically with an approval.cancelled lifecycle event, all under the SAME
// row lock. Nothing is ever posted for a cancelled pending.
func (s *ApprovalService) Cancel(ctx context.Context, tenantID, id, actor string) error {
	return s.lockedTransition(ctx, tenantID, id,
		func(p *domain.PendingTransaction) error {
			if p.Status != domain.PendingStatusPending {
				return domain.ErrPendingAlreadyDecided
			}
			if actor != p.CreatedBy {
				return domain.ErrNotPendingCreator
			}
			return nil
		},
		func(ctx context.Context, tx domain.Tx, pending *domain.PendingTransaction) error {
			if err := tx.UpdatePendingStatus(ctx, tenantID, id, domain.PendingStatusCancelled, actor, nil, nil); err != nil {
				return err
			}
			pending.Status = domain.PendingStatusCancelled
			return appendPendingEvent(ctx, tx, tenantID, actor, "approval.cancelled", pending, nil)
		},
	)
}

// SweepExpiredPending moves every pending left undecided past cfg.TTL to
// expired (Task 8, ADR-025), mirroring the existing idempotency-key sweep's
// shape: a background goroutine (runPendingSweep in cmd/server) calls this
// on an interval, not a per-request path. The repository's own
// SweepExpiredPending does the actual UPDATE (across every tenant, outside
// any RunInTx: see its doc comment), so this method's only job is to turn
// each returned row into an approval.expired lifecycle event. Each event is
// appended in its own RunInTx, scoped to that row's own tenant: the swept
// rows can span tenants, and appendPendingEvent needs a domain.Tx opened
// under the right tenant GUC for its RLS-scoped write to land. A failure
// appending one row's event does not stop the sweep from emitting the rest;
// it is returned as a combined error after every row has been tried, the
// same "keep going, report at the end" shape SweepExpiredIdempotencyKeys'
// caller already tolerates for pure housekeeping.
func (s *ApprovalService) SweepExpiredPending(ctx context.Context) (int64, error) {
	expired, err := s.repo.SweepExpiredPending(ctx, s.cfg.TTL)
	if err != nil {
		return 0, err
	}
	var errs []error
	for i := range expired {
		p := expired[i]
		err := s.repo.RunInTx(ctx, p.TenantID, func(ctx context.Context, tx domain.Tx) error {
			return appendPendingEvent(ctx, tx, p.TenantID, "system", "approval.expired", &p, nil)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("ledger: emit approval.expired for pending %s: %w", p.ID, err))
		}
	}
	if len(errs) > 0 {
		return int64(len(expired)), errors.Join(errs...)
	}
	return int64(len(expired)), nil
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

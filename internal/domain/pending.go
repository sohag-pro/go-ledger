package domain

import (
	"encoding/json"
	"time"
)

// PendingStatus is a pending transaction's lifecycle state (ADR-025).
// PendingStatusPending is the only non-terminal status; every pending moves
// to exactly one of the four terminal statuses at most once.
type PendingStatus string

const (
	// PendingStatusPending is a held transaction awaiting a decision.
	PendingStatusPending PendingStatus = "pending"
	// PendingStatusApproved is a terminal status: the held transaction was
	// approved and replayed through the ordinary posting path.
	// TransactionID names the transaction it produced.
	PendingStatusApproved PendingStatus = "approved"
	// PendingStatusRejected is a terminal status: an approver rejected the
	// held transaction. No money ever moves for it.
	PendingStatusRejected PendingStatus = "rejected"
	// PendingStatusCancelled is a terminal status: the creator withdrew the
	// held transaction before any decision was made.
	PendingStatusCancelled PendingStatus = "cancelled"
	// PendingStatusExpired is a terminal status: PENDING_TTL elapsed with no
	// decision, and the background sweep (SweepExpiredPending) closed it out.
	PendingStatusExpired PendingStatus = "expired"
)

// Valid reports whether s is one of the five defined statuses.
func (s PendingStatus) Valid() bool {
	switch s {
	case PendingStatusPending, PendingStatusApproved, PendingStatusRejected, PendingStatusCancelled, PendingStatusExpired:
		return true
	default:
		return false
	}
}

// PendingKind names which write path a held pending transaction's Payload
// will replay on approval (ADR-025): a plain post, an FX conversion, or a
// reversal.
type PendingKind string

const (
	// PendingKindPost holds an ordinary CreateTransaction request.
	PendingKindPost PendingKind = "post"
	// PendingKindConvert holds an FX conversion request.
	PendingKindConvert PendingKind = "convert"
	// PendingKindReverse holds a reversal request.
	PendingKindReverse PendingKind = "reverse"
)

// Valid reports whether k is one of the three defined kinds.
func (k PendingKind) Valid() bool {
	switch k {
	case PendingKindPost, PendingKindConvert, PendingKindReverse:
		return true
	default:
		return false
	}
}

// PendingTransaction is an over-threshold transaction held as intent
// (ADR-025): Payload is the original request (the legs, reference,
// effective time, and any convert/reverse parameters), stored as raw JSON
// and replayed unchanged through the ordinary posting path only once
// approved. Nothing in postings/transactions ever reflects a
// PendingTransaction until then; the core invariant (balances are only ever
// derived from postings) is untouched.
type PendingTransaction struct {
	ID       string
	TenantID string
	Kind     PendingKind
	Payload  json.RawMessage
	Status   PendingStatus
	// ThresholdCcy and ThresholdAmt record which currency and configured
	// threshold amount tripped the gate (ADR-025's largest-leg-per-currency
	// rule): informational, for display and audit. They play no part in
	// re-validation at approval time, which checks the payload against
	// current balances, not against the threshold that originally gated it.
	ThresholdCcy string
	ThresholdAmt int64
	CreatedBy    string
	CreatedAt    time.Time
	// DecidedBy, DecidedAt, and Reason are nil until a decision (approve,
	// reject, cancel, or the expiry sweep) is made; a decision sets all
	// three at once, exactly one time, whichever attempt wins the row lock
	// (see Repository.GetPendingForUpdate).
	DecidedBy *string
	DecidedAt *time.Time
	Reason    *string
	// TransactionID is nil until Status is PendingStatusApproved, at which
	// point it names the transaction the approval posted.
	TransactionID *string
}

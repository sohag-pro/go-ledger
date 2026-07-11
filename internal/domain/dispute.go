package domain

import (
	"strings"
	"time"
)

// MaxDisputeReasonLen bounds Dispute.Reason (Task 6.3, audit A9.2): the same
// established 256-character ceiling MaxPostingDescriptionLen and
// MaxTransactionReferenceLen already use for a free-form, client-supplied
// string.
const MaxDisputeReasonLen = 256

// DisputeStatus is a dispute's lifecycle state (Task 6.3, audit A9.2). A
// dispute starts open and is resolved at most once, either into
// DisputeResolvedReversed (a real reversal was posted through
// TransactionService.ReverseTransaction) or DisputeResolvedRejected (no
// money moves). Unlike AccountStatus there is no "unset means active"
// convention here: every dispute is created with an explicit status, so a
// zero-value DisputeStatus is simply invalid.
type DisputeStatus string

const (
	// DisputeOpen is the only status a dispute may be resolved from.
	DisputeOpen DisputeStatus = "open"
	// DisputeResolvedReversed is a terminal status: the disputed
	// transaction was reversed and ResolutionTransactionID names the
	// reversal.
	DisputeResolvedReversed DisputeStatus = "resolved_reversed"
	// DisputeResolvedRejected is a terminal status: the dispute was
	// rejected and no money moved.
	DisputeResolvedRejected DisputeStatus = "resolved_rejected"
)

// Valid reports whether s is one of the three defined statuses.
func (s DisputeStatus) Valid() bool {
	switch s {
	case DisputeOpen, DisputeResolvedReversed, DisputeResolvedRejected:
		return true
	default:
		return false
	}
}

// Terminal reports whether s is a resolved (non-open) status: a dispute in
// either terminal status has already been resolved once and Resolve must
// reject a second attempt (see ErrDisputeAlreadyResolved).
func (s DisputeStatus) Terminal() bool {
	return s == DisputeResolvedReversed || s == DisputeResolvedRejected
}

// Dispute is a tenant's dispute or chargeback claim against one of its own
// transactions (Task 6.3, audit A9.2), built on the reversal primitive (Task
// 4.2): opening a dispute records intent only, no money moves; resolving one
// either posts a real reversal (through TransactionService.ReverseTransaction,
// the normal posting path, never a raw insert) or rejects the dispute with no
// money movement at all.
type Dispute struct {
	ID       string
	TenantID string
	// TransactionID is the disputed transaction: it must belong to the same
	// tenant (enforced by the composite FK, migration 0029, and checked
	// up front by the service via Repository.GetTransaction before a
	// dispute is ever created).
	TransactionID string
	Status        DisputeStatus
	Reason        string
	// ResolutionTransactionID is the reversal posted on resolution, set
	// only when Status is DisputeResolvedReversed. nil for an open dispute
	// or one resolved as rejected.
	ResolutionTransactionID *string
	CreatedAt               time.Time
	// ResolvedAt is nil until the dispute is resolved (either way).
	ResolvedAt *time.Time
}

// Validate checks that a Dispute carries a transaction id, a non-empty
// reason within MaxDisputeReasonLen, and (when set) a recognized Status. An
// empty Status is valid here (mirroring Account.Status's "unset" convention)
// only for a Dispute being newly constructed before the service stamps
// DisputeOpen on it; a Dispute read back from storage always carries a
// concrete status.
func (d Dispute) Validate() error {
	if d.TransactionID == "" {
		return ErrInvalidDispute
	}
	if strings.TrimSpace(d.Reason) == "" {
		return ErrInvalidDispute
	}
	if len(d.Reason) > MaxDisputeReasonLen {
		return ErrDisputeReasonTooLong
	}
	if d.Status != "" && !d.Status.Valid() {
		return ErrInvalidDispute
	}
	return nil
}

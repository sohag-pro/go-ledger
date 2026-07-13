package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// HeldForApprovalError is returned by a write path when the transaction
// exceeded the approval threshold and was stored as a pending instead of
// posted (ADR-025). The pending was already committed by the time this is
// returned; the transport layer maps this to 202 Accepted with the pending
// resource (a later task, not this one).
type HeldForApprovalError struct{ Pending *domain.PendingTransaction }

func (e *HeldForApprovalError) Error() string {
	return "transaction held for approval: pending " + e.Pending.ID
}

// AsHeldForApproval extracts the pending if err is a HeldForApprovalError.
func AsHeldForApproval(err error) (*domain.PendingTransaction, bool) {
	var h *HeldForApprovalError
	if errors.As(err, &h) {
		return h.Pending, true
	}
	return nil, false
}

type approvalReplayKey struct{}

// withApprovalReplay marks ctx as an approval replay so the gate is skipped:
// the pending already cleared it once (that is the entire reason a decision
// service is replaying it now), and re-gating would just hold it again,
// looping forever instead of ever posting. Not yet called from this package:
// it is consumed by the approval decision service (Task 7), which replays a
// pending's payload through Post/Convert/ReverseTransaction with this ctx.
//
//nolint:unused // wired up by Task 7's approval decision service
func withApprovalReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, approvalReplayKey{}, true)
}

func isApprovalReplay(ctx context.Context) bool {
	v, _ := ctx.Value(approvalReplayKey{}).(bool)
	return v
}

// holdForApproval writes a pending_transactions row and its approval.requested
// lifecycle event in one transaction, then returns a HeldForApprovalError. It
// is called by Post, Convert, and ReverseTransaction after ApprovalConfig.Gate
// reports a transaction as over threshold: nothing about the original write
// (postings, the transaction row) is ever touched, only the pending is
// created.
//
// idempotencyKey is the caller's Idempotency-Key, or "" if the request
// carried none. ADR-025 section 6 (Lifecycle) promises that a gated create
// consumes its idempotency key against the pending it holds, so a replay of
// the same key returns the same pending rather than creating a second one:
// a client that retries an identical over-threshold create (for example
// after a dropped 202 response) must not end up with two pendings for the
// same request, since approving both would double-post the same money under
// two different derived approval keys. When idempotencyKey is "" (the
// caller supplied none), this dedup does not apply: every hold creates a
// fresh pending, exactly as before.
//
// This dedups on the key alone, not on the payload: a retry that reuses the
// same idempotency key but changes the payload gets back the ORIGINAL
// pending, not a 422 conflict the way idempotency_keys' fingerprint check
// would produce for an ordinary post. That is a deliberate, narrower
// guarantee than the ordinary idempotency path: building fingerprint
// comparison for pendings is out of scope here.
func (s *TransactionService) holdForApproval(
	ctx context.Context, tenantID, actor string, kind domain.PendingKind,
	payload any, ccy string, amt int64, idempotencyKey string,
) error {
	if idempotencyKey != "" {
		existing, err := s.repo.GetPendingByIdempotencyKey(ctx, tenantID, idempotencyKey)
		switch {
		case err == nil:
			return &HeldForApprovalError{Pending: existing}
		case errors.Is(err, domain.ErrPendingTransactionNotFound):
			// No existing pending for this key: proceed to hold a new one.
		default:
			return err
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	p := &domain.PendingTransaction{
		ID:           uuid.Must(uuid.NewV7()).String(),
		TenantID:     tenantID,
		Kind:         kind,
		Payload:      body,
		Status:       domain.PendingStatusPending,
		ThresholdCcy: ccy,
		ThresholdAmt: amt,
		CreatedBy:    actor,
	}
	if idempotencyKey != "" {
		p.IdempotencyKey = &idempotencyKey
	}
	err = s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, tx domain.Tx) error {
		if err := tx.InsertPendingTransaction(ctx, tenantID, p); err != nil {
			return err
		}
		return appendPendingEvent(ctx, tx, tenantID, "approval.requested", p, nil)
	})
	if err != nil {
		// A concurrent hold raced in between the check above and this
		// insert and won: read back ITS pending rather than surfacing a
		// duplicate-key error, so this retry still gets the same-pending
		// guarantee (see the doc comment above).
		if idempotencyKey != "" && errors.Is(err, domain.ErrDuplicatePendingIdempotencyKey) {
			existing, ferr := s.repo.GetPendingByIdempotencyKey(ctx, tenantID, idempotencyKey)
			if ferr == nil {
				return &HeldForApprovalError{Pending: existing}
			}
		}
		return err
	}
	return &HeldForApprovalError{Pending: p}
}

// payloadPosting is one leg of a held post or reversal request, capturing
// exactly what CreateTransaction needs to rebuild the same postings on
// approval (Task 6, ADR-025): the account, the signed amount and its
// currency (a posting carries its own currency, ADR-014, so there is no
// single top-level currency to hoist out), and the free-text description.
type payloadPosting struct {
	AccountID   string `json:"account_id"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
}

// postPayloadBody is the self-contained JSON shape a held Post request
// stores in PendingTransaction.Payload: every posting leg, plus the optional
// reference and effective_at a caller may have supplied. It captures the
// request PLAINTEXT (never the encrypted descriptions Post writes to
// storage): postPayload is called before Post's own encrypt-once block runs,
// so what a decision later replays is exactly what the original caller
// submitted.
type postPayloadBody struct {
	Postings    []payloadPosting `json:"postings"`
	Reference   *string          `json:"reference,omitempty"`
	EffectiveAt *time.Time       `json:"effective_at,omitempty"`
}

// postPayload builds the pending payload for a held Post request.
func postPayload(t *domain.Transaction) any {
	postings := make([]payloadPosting, 0, len(t.Postings))
	for _, p := range t.Postings {
		postings = append(postings, payloadPosting{
			AccountID:   p.AccountID,
			Amount:      p.Amount.Amount(),
			Currency:    string(p.Amount.Currency()),
			Description: p.Description,
		})
	}
	return postPayloadBody{
		Postings:    postings,
		Reference:   t.Reference,
		EffectiveAt: t.EffectiveAt,
	}
}

// convertPayloadBody is the self-contained JSON shape a held Convert
// request stores in PendingTransaction.Payload: the original request, not
// the four resolved legs it would have produced. A convert's legs depend on
// the FX rate in effect at post time (ADR-014), which may have moved by the
// time a decision replays this; storing the request lets the replay resolve
// a fresh rate the same way an ordinary Convert call would, rather than
// replaying a stale, possibly no-longer-accurate rate.
type convertPayloadBody struct {
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	SourceAmount  int64  `json:"source_amount"`
}

// convertPayload builds the pending payload for a held Convert request.
// ConvertRequest and convertPayloadBody happen to share the same field
// names, order, and types, so this is a plain type conversion, not a
// field-by-field copy.
func convertPayload(req ConvertRequest) any {
	return convertPayloadBody(req)
}

// reversePayloadBody is the self-contained JSON shape a held reversal
// request stores in PendingTransaction.Payload: just the id of the
// transaction being reversed. Unlike postPayloadBody, the reversal's own
// legs are not stored: they are fully determined by the original transaction
// (BuildReversal negates its postings), so replaying this only ever needs
// the one id, read fresh from the original at decision time exactly like a
// brand-new ReverseTransaction call would.
type reversePayloadBody struct {
	ReversedTransactionID string `json:"reversed_transaction_id"`
}

// reversePayload builds the pending payload for a held reversal request.
func reversePayload(originalID string) any {
	return reversePayloadBody{ReversedTransactionID: originalID}
}

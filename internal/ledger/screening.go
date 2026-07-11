package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// PrePostHook lets an external screening or transaction-monitoring system
// veto a transaction before it is posted (Task 6.1, audit A9.1). It is
// called synchronously in the posting path, with the fully-built
// transaction, BEFORE any row for it is written: returning a non-nil error
// rejects the post outright, and nothing is persisted (see Post and
// Convert's own doc comments for exactly where the call happens).
//
// This is a veto, not a hold: there is no pending-review state a rejected
// transaction moves into. An approval/hold workflow is a different feature
// (out of scope for this project, see CLAUDE.md's scope discipline
// section), and this hook does not implement one. A hook implementation
// that wants a "maybe" outcome has nowhere to put it: it must resolve to
// either allow (return nil) or reject (return a non-nil error) before this
// call returns.
//
// The two kinds of rejection are NOT the same to callers reading the error:
//   - An explicit compliance veto returns (or wraps) ErrScreeningRejected,
//     ideally as a *ScreeningRejectedError carrying a human-readable reason.
//     This maps to 422 Unprocessable Entity (REST) / codes.FailedPrecondition
//     (gRPC): the request is well-formed, screening just says no.
//   - Any OTHER error (a timeout, a connection failure, a 500 from the
//     screening service, or anything else that is not an explicit reject) is
//     treated as an AMBIGUOUS screening failure. The post is still rejected
//     (fail closed: an ambiguous "we don't know" must never be treated as an
//     implicit allow), but it maps to 503 Service Unavailable instead, since
//     the caller can reasonably retry once the screening system is healthy
//     again. See ErrScreeningUnavailable.
type PrePostHook interface {
	ReviewPost(ctx context.Context, tenantID string, t *domain.Transaction) error
}

// NoopPrePostHook allows every transaction. It is the default a
// TransactionService uses when constructed without WithPrePostHook, so a
// deployment that never wires in a screening system posts exactly as it did
// before this hook existed.
type NoopPrePostHook struct{}

// ReviewPost always returns nil: every post is allowed.
func (NoopPrePostHook) ReviewPost(context.Context, string, *domain.Transaction) error {
	return nil
}

// ErrScreeningRejected is the sentinel matched via errors.Is for any
// *ScreeningRejectedError, regardless of the reason a PrePostHook gave for
// rejecting a post (Task 6.1, audit A9.1). A transport layer maps it to 422
// Unprocessable Entity (REST) / codes.FailedPrecondition (gRPC): the request
// is otherwise well-formed, an external screening decision just vetoed it.
var ErrScreeningRejected = errors.New("ledger: screening rejected post")

// ScreeningRejectedError is what a PrePostHook implementation is expected to
// return (or wrap) to reject a post with a human-readable reason, for
// example "sanctions list match" or "velocity limit exceeded". It wraps
// ErrScreeningRejected so errors.Is(err, ErrScreeningRejected) matches
// regardless of the specific reason, the same wrap-a-sentinel shape
// *domain.AccountNotActiveError and *domain.PolicyViolationError already use
// one layer down.
type ScreeningRejectedError struct {
	Reason string
}

// Error implements the error interface, including Reason when the hook gave
// one so a client sees the exact cause rather than a generic message.
func (e *ScreeningRejectedError) Error() string {
	if e.Reason == "" {
		return "ledger: screening rejected post"
	}
	return "ledger: screening rejected post: " + e.Reason
}

// Unwrap lets errors.Is(err, ErrScreeningRejected) match regardless of Reason.
func (e *ScreeningRejectedError) Unwrap() error { return ErrScreeningRejected }

// ErrScreeningUnavailable is the sentinel a post/convert attempt fails with
// when a PrePostHook returns an error that is NOT ErrScreeningRejected (Task
// 6.1, audit A9.1): an infrastructure failure talking to the screening
// system (a timeout, a dropped connection, an unexpected 5xx), not an
// explicit veto. reviewPost below wraps the hook's own error alongside this
// sentinel, so errors.Is(err, ErrScreeningUnavailable) matches regardless of
// what the hook itself returned, while the original error is still
// reachable via errors.Unwrap for logs. A transport layer maps this to 503
// Service Unavailable: this is an ambiguous "we don't know", which must fail
// closed (the post is rejected, exactly like an explicit veto) but is
// presented to the caller as retryable, since the screening system, not the
// request itself, is what is unhealthy.
var ErrScreeningUnavailable = errors.New("ledger: screening system unavailable")

// reviewPost calls hook.ReviewPost and normalizes its result for callers
// (Post and Convert): nil is passed through, an error that is or wraps
// ErrScreeningRejected is passed through unchanged (so *ScreeningRejectedError
// survives errors.As at the transport layer), and any other error is wrapped
// with ErrScreeningUnavailable so a transport layer can map it to 503
// without needing to recognize every possible error a hook implementation
// might return. It is called exactly once per post/convert attempt, before
// any DB write: see Post and Convert's own call sites for exactly where.
func reviewPost(ctx context.Context, hook PrePostHook, tenantID string, t *domain.Transaction) error {
	err := hook.ReviewPost(ctx, tenantID, t)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrScreeningRejected) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrScreeningUnavailable, err)
}

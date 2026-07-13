package ledger_test

// Cheap, DB-free unit coverage for the small approval-hold helpers that the
// integration tests above never happen to exercise directly:
// HeldForApprovalError.Error()'s message format, AsHeldForApproval's false
// path (no error, or an error unrelated to the approval gate), and
// NewApprovalService's nil-logger default branch.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// TestHeldForApprovalError_ErrorIncludesPendingID checks that the error
// message identifies which pending it refers to, since a caller (or a log
// line) reading just the string still needs to know which row to look up.
func TestHeldForApprovalError_ErrorIncludesPendingID(t *testing.T) {
	t.Parallel()
	err := &ledger.HeldForApprovalError{Pending: &domain.PendingTransaction{ID: "pending-abc-123"}}
	msg := err.Error()
	if !strings.Contains(msg, "pending-abc-123") {
		t.Errorf("Error() = %q, want it to contain the pending id %q", msg, "pending-abc-123")
	}
}

// TestAsHeldForApproval covers all three branches: no error at all, an
// unrelated error that is not a HeldForApprovalError anywhere in its chain,
// and a HeldForApprovalError wrapped inside another error (proving it uses
// errors.As, not a direct type assertion).
func TestAsHeldForApproval(t *testing.T) {
	t.Parallel()

	t.Run("nil error", func(t *testing.T) {
		t.Parallel()
		pending, ok := ledger.AsHeldForApproval(nil)
		if ok || pending != nil {
			t.Errorf("AsHeldForApproval(nil) = (%v, %v), want (nil, false)", pending, ok)
		}
	})

	t.Run("unrelated error", func(t *testing.T) {
		t.Parallel()
		pending, ok := ledger.AsHeldForApproval(errors.New("some other failure"))
		if ok || pending != nil {
			t.Errorf("AsHeldForApproval(unrelated) = (%v, %v), want (nil, false)", pending, ok)
		}
	})

	t.Run("wrapped HeldForApprovalError", func(t *testing.T) {
		t.Parallel()
		want := &domain.PendingTransaction{ID: "pending-xyz"}
		wrapped := fmt.Errorf("post failed: %w", &ledger.HeldForApprovalError{Pending: want})
		pending, ok := ledger.AsHeldForApproval(wrapped)
		if !ok || pending != want {
			t.Errorf("AsHeldForApproval(wrapped) = (%v, %v), want (%v, true)", pending, ok, want)
		}
	})
}

// TestNewApprovalService_NilLoggerDoesNotPanic covers NewApprovalService's
// nil-logger branch (it falls back to slog.Default(), matching
// NewTransactionService's own convention): constructing one with a nil
// *slog.Logger must not panic, and must return a usable service.
func TestNewApprovalService_NilLoggerDoesNotPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewApprovalService with a nil logger panicked: %v", r)
		}
	}()
	svc := ledger.NewApprovalService(nil, nil, ledger.ApprovalConfig{}, nil)
	if svc == nil {
		t.Fatal("NewApprovalService(nil logger) = nil, want a non-nil service using the default logger")
	}
}

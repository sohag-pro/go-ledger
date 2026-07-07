package ledger_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// unbalancedTxn sums to a non-zero amount, so Transaction.Validate() rejects it
// before Post touches the repository. That lets the span assertions run without a
// database, while still exercising the span creation and error-recording paths.
func unbalancedTxn(t *testing.T) *domain.Transaction {
	t.Helper()
	d, _ := domain.NewMoney(250, "USD")
	c, _ := domain.NewMoney(-100, "USD")
	return &domain.Transaction{Postings: []domain.Posting{
		{AccountID: "acct-debit", Amount: d},
		{AccountID: "acct-credit", Amount: c},
	}}
}

func TestPostOpensSpanAndRecordsValidationError(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	// repo is nil on purpose: validation fails first, so it is never called.
	svc := ledger.NewTransactionService(nil, nil, tp.Tracer("test"))

	if _, err := svc.Post(context.Background(), "tenant-1", unbalancedTxn(t), nil); err == nil {
		t.Fatal("expected a validation error for an unbalanced transaction")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected exactly one span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name() != "ledger.PostTransaction" {
		t.Errorf("span name = %q, want ledger.PostTransaction", span.Name())
	}
	if span.Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status().Code)
	}
	if len(span.Events()) == 0 {
		t.Error("expected a recorded error event on the span")
	}
}

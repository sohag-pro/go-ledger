// Package ledger holds the application services that sit between transport
// (REST, gRPC) and the persistence port. Services own cross-cutting concerns:
// validation, transaction boundaries, metrics, and logging. They contain no SQL
// and no HTTP; the domain owns the rules and the adapter owns the storage.
package ledger

import (
	"context"
	"log/slog"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/metrics"
)

// TransactionService posts transactions to the ledger. It is the single entry
// point both the REST and gRPC layers will call to move money.
type TransactionService struct {
	repo domain.Repository
	log  *slog.Logger
}

// NewTransactionService returns a TransactionService backed by repo. If log is
// nil the default slog logger is used.
func NewTransactionService(repo domain.Repository, log *slog.Logger) *TransactionService {
	if log == nil {
		log = slog.Default()
	}
	return &TransactionService{repo: repo, log: log}
}

// Post validates t and persists it atomically inside a SERIALIZABLE transaction,
// retrying transparently on serialization conflicts. It records posting latency
// and outcome, and returns the domain validation error unchanged when t does not
// balance, so callers can map it to a 4xx rather than a 5xx.
func (s *TransactionService) Post(ctx context.Context, tenantID string, t *domain.Transaction) error {
	// Validate up front so an unbalanced or malformed transaction fails fast,
	// without touching the database.
	if err := t.Validate(); err != nil {
		metrics.PostDuration.WithLabelValues("invalid").Observe(0)
		return err
	}

	start := time.Now()
	err := s.repo.RunInTx(ctx, func(ctx context.Context, tx domain.Tx) error {
		return tx.CreateTransaction(ctx, tenantID, t)
	})
	elapsed := time.Since(start).Seconds()

	if err != nil {
		metrics.PostDuration.WithLabelValues("failed").Observe(elapsed)
		s.log.ErrorContext(ctx, "post transaction failed",
			"tenant_id", tenantID, "transaction_id", t.ID, "error", err)
		return err
	}

	metrics.PostDuration.WithLabelValues("committed").Observe(elapsed)
	s.log.InfoContext(ctx, "transaction posted",
		"tenant_id", tenantID, "transaction_id", t.ID, "postings", len(t.Postings))
	return nil
}

// Get returns a transaction and its postings, or domain.ErrTransactionNotFound.
func (s *TransactionService) Get(ctx context.Context, tenantID, id string) (domain.Transaction, error) {
	return s.repo.GetTransaction(ctx, tenantID, id)
}

// Package ledger holds the application services that sit between transport
// (REST, gRPC) and the persistence port. Services own cross-cutting concerns:
// validation, transaction boundaries, metrics, and logging. They contain no SQL
// and no HTTP; the domain owns the rules and the adapter owns the storage.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/metrics"
)

// TransactionService posts transactions to the ledger. It is the single entry
// point both the REST and gRPC layers will call to move money.
type TransactionService struct {
	repo   domain.Repository
	log    *slog.Logger
	tracer oteltrace.Tracer
}

// NewTransactionService returns a TransactionService backed by repo. If log is
// nil the default slog logger is used; if tracer is nil the global tracer is used.
func NewTransactionService(repo domain.Repository, log *slog.Logger, tracer oteltrace.Tracer) *TransactionService {
	if log == nil {
		log = slog.Default()
	}
	if tracer == nil {
		tracer = otel.Tracer("github.com/sohag-pro/go-ledger/internal/ledger")
	}
	return &TransactionService{repo: repo, log: log, tracer: tracer}
}

// Post validates t and persists it atomically inside a SERIALIZABLE transaction,
// retrying transparently on serialization conflicts. When idem is non-nil it also
// records the idempotency key and, if the same key was already used, replays the
// original transaction instead of writing a new one (returning replayed=true). A
// key reused with a different request body returns domain.ErrIdempotencyConflict.
// Every real post also writes one append-only audit row in the same transaction.
func (s *TransactionService) Post(ctx context.Context, tenantID string, t *domain.Transaction, idem *domain.Idempotency) (replayed bool, err error) {
	ctx, span := s.tracer.Start(ctx, "ledger.PostTransaction",
		oteltrace.WithAttributes(
			attribute.String("tenant_id", tenantID),
			attribute.Int("transaction.posting_count", len(t.Postings)),
			attribute.Bool("idempotency.present", idem != nil),
		),
	)
	defer span.End()

	if err := t.Validate(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "validation failed")
		metrics.PostDuration.WithLabelValues("invalid").Observe(0)
		return false, err
	}

	fingerprint := t.Fingerprint()
	start := time.Now()
	runErr := s.repo.RunInTx(ctx, func(ctx context.Context, tx domain.Tx) error {
		if err := tx.CreateTransaction(ctx, tenantID, t); err != nil {
			return err
		}
		if idem != nil {
			if err := tx.InsertIdempotencyKey(ctx, tenantID, idem.Key, fingerprint, t.ID); err != nil {
				return err
			}
		}
		after, err := json.Marshal(auditSnapshot(t))
		if err != nil {
			return err
		}
		return tx.AppendAudit(ctx, tenantID, domain.AuditEntry{
			Action:        domain.ActionTransactionCreated,
			TransactionID: t.ID,
			Actor:         tenantID,
			After:         after,
		})
	})
	elapsed := time.Since(start).Seconds()

	if runErr != nil {
		if idem != nil && errors.Is(runErr, domain.ErrDuplicateIdempotencyKey) {
			return s.replay(ctx, tenantID, idem.Key, fingerprint, t)
		}
		span.RecordError(runErr)
		span.SetStatus(codes.Error, "post failed")
		metrics.PostDuration.WithLabelValues("failed").Observe(elapsed)
		s.log.ErrorContext(ctx, "post transaction failed",
			"tenant_id", tenantID, "transaction_id", t.ID, "error", runErr)
		return false, runErr
	}

	metrics.PostDuration.WithLabelValues("committed").Observe(elapsed)
	s.log.InfoContext(ctx, "transaction posted",
		"tenant_id", tenantID, "transaction_id", t.ID, "postings", len(t.Postings))
	return false, nil
}

// replay resolves a duplicate idempotency key: if the stored fingerprint matches,
// it loads the original transaction into t and reports a replay; if not, the key
// was reused with a different body and it returns ErrIdempotencyConflict.
func (s *TransactionService) replay(ctx context.Context, tenantID, key, fingerprint string, t *domain.Transaction) (bool, error) {
	rec, err := s.repo.GetIdempotencyKey(ctx, tenantID, key)
	if err != nil {
		return false, err
	}
	if rec.Fingerprint != fingerprint {
		metrics.IdempotencyConflicts.Inc()
		return false, domain.ErrIdempotencyConflict
	}
	existing, err := s.repo.GetTransaction(ctx, tenantID, rec.TransactionID)
	if err != nil {
		return false, err
	}
	*t = existing
	metrics.IdempotencyReplays.Inc()
	s.log.InfoContext(ctx, "transaction replayed",
		"tenant_id", tenantID, "transaction_id", existing.ID, "idempotency_key", key)
	return true, nil
}

// auditSnapshot is the JSON-serializable view of a transaction stored in the
// audit log's after column. Using a map gives deterministic, sorted keys.
func auditSnapshot(t *domain.Transaction) map[string]any {
	currency := ""
	if len(t.Postings) > 0 {
		currency = string(t.Postings[0].Amount.Currency())
	}
	postings := make([]map[string]any, 0, len(t.Postings))
	for _, p := range t.Postings {
		postings = append(postings, map[string]any{
			"account_id":  p.AccountID,
			"amount":      p.Amount.Amount(),
			"description": p.Description,
		})
	}
	return map[string]any{
		"id":       t.ID,
		"currency": currency,
		"postings": postings,
	}
}

// Get returns a transaction and its postings, or domain.ErrTransactionNotFound.
func (s *TransactionService) Get(ctx context.Context, tenantID, id string) (domain.Transaction, error) {
	return s.repo.GetTransaction(ctx, tenantID, id)
}

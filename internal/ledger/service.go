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
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/metrics"
)

// TransactionService posts transactions to the ledger. It is the single entry
// point both the REST and gRPC layers will call to move money.
type TransactionService struct {
	repo       domain.Repository
	log        *slog.Logger
	tracer     oteltrace.Tracer
	fxProvider fx.Provider
}

// ServiceOption configures optional TransactionService dependencies that most
// callers (and most existing tests, which only ever call Post) do not need,
// so NewTransactionService's required parameters stay unchanged.
type ServiceOption func(*TransactionService)

// WithFXProvider sets the fx.Provider Convert uses to resolve the current
// rate for a currency pair. A TransactionService constructed without this
// option has a nil fxProvider; Convert reports a clear error rather than
// panicking on the nil interface call (see Convert).
func WithFXProvider(p fx.Provider) ServiceOption {
	return func(s *TransactionService) { s.fxProvider = p }
}

// NewTransactionService returns a TransactionService backed by repo. If log is
// nil the default slog logger is used; if tracer is nil the global tracer is used.
func NewTransactionService(repo domain.Repository, log *slog.Logger, tracer oteltrace.Tracer, opts ...ServiceOption) *TransactionService {
	if log == nil {
		log = slog.Default()
	}
	if tracer == nil {
		tracer = otel.Tracer("github.com/sohag-pro/go-ledger/internal/ledger")
	}
	s := &TransactionService{repo: repo, log: log, tracer: tracer}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Post validates t and persists it atomically inside a SERIALIZABLE transaction,
// retrying transparently on serialization conflicts. When idem is non-nil it also
// records the idempotency key and, if the same key was already used, replays the
// original transaction instead of writing a new one (returning replayed=true). A
// key reused with a different request body returns domain.ErrIdempotencyConflict.
// Every real post also writes one append-only audit row in the same transaction.
// Before any of that, t's postings are checked against tenantID's optional
// TenantPolicy guardrails (Task 2.4b, audit A3.4: max transaction amount, daily
// volume, currency allowlist); a tripped guardrail returns a
// *domain.PolicyViolationError.
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

	// Resolved BEFORE RunInTx, on its own connection, never from inside
	// RunInTx's closure: see enforceTenantPolicy's doc comment for the
	// connection-pool deadlock a second in-tx Repository call would risk
	// under a small pool.
	policy, err := tenantPolicy(ctx, s.repo, tenantID)
	if err != nil {
		span.RecordError(err)
		return false, err
	}

	fingerprint := t.Fingerprint()
	start := time.Now()
	runErr := s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, tx domain.Tx) error {
		if err := enforceTenantPolicy(ctx, tx, tenantID, policy, t.Postings); err != nil {
			return err
		}
		if err := tx.CreateTransaction(ctx, tenantID, t); err != nil {
			return err
		}
		if idem != nil {
			if err := tx.InsertIdempotencyKey(ctx, tenantID, idem.Key, fingerprint, domain.CurrentFingerprintScheme, t.ID); err != nil {
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
			return s.replay(ctx, tenantID, idem.Key, t)
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

// replay resolves a duplicate idempotency key: it recomputes t's fingerprint
// under the SCHEME THE STORED RECORD CARRIES (rec.Scheme), not necessarily
// the scheme this binary currently writes (domain.CurrentFingerprintScheme),
// so a fingerprint-scheme change never false-conflicts a key stored under an
// older scheme (Task 2.3, audit A1.6). If that recomputation matches the
// stored fingerprint, it loads the original transaction into t and reports a
// replay; if not, the key was reused with a different body and it returns
// ErrIdempotencyConflict. If rec.Scheme is not one this binary knows how to
// compute (for example a key written by a newer binary, then read by this
// one after a downgrade), it fails closed with ErrIdempotencyConflict rather
// than risk replaying a transaction it cannot verify the body of.
func (s *TransactionService) replay(ctx context.Context, tenantID, key string, t *domain.Transaction) (bool, error) {
	rec, err := s.repo.GetIdempotencyKey(ctx, tenantID, key)
	if err != nil {
		return false, err
	}
	expected, ok := domain.TransactionFingerprint(rec.Scheme, *t)
	if !ok || rec.Fingerprint != expected {
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
//
// Each posting carries its own currency (ADR-014): a cross-currency convert
// spans two currencies in one transaction, so there is no single top-level
// currency that could label every posting correctly, and stamping one from
// postings[0] (the pre-ADR-014 shape) would mislabel every leg in the other
// currency. When t.FX is set (a convert), the snapshot also carries the rate
// detail actually applied: the mid rate, spread, applied rate, source, and
// effective time, so the audit row is a full, self-contained record of what
// happened without needing to join back to the transaction row.
func auditSnapshot(t *domain.Transaction) map[string]any {
	postings := make([]map[string]any, 0, len(t.Postings))
	for _, p := range t.Postings {
		postings = append(postings, map[string]any{
			"account_id":  p.AccountID,
			"amount":      p.Amount.Amount(),
			"currency":    string(p.Amount.Currency()),
			"description": p.Description,
		})
	}
	snapshot := map[string]any{
		"id":       t.ID,
		"postings": postings,
	}
	if t.FX != nil {
		snapshot["fx"] = map[string]any{
			"source_amount":    t.FX.SourceAmount,
			"converted_amount": t.FX.ConvertedAmount,
			"mid_rate_e8":      t.FX.MidRateE8,
			"applied_e8":       t.FX.AppliedE8,
			"spread_bps":       t.FX.SpreadBps,
			"rate_source":      t.FX.RateSource,
			"effective_at":     t.FX.EffectiveAt,
			"rate_id":          t.FX.RateID,
		}
	}
	return snapshot
}

// Get returns a transaction and its postings, or domain.ErrTransactionNotFound.
func (s *TransactionService) Get(ctx context.Context, tenantID, id string) (domain.Transaction, error) {
	return s.repo.GetTransaction(ctx, tenantID, id)
}

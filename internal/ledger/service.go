// Package ledger holds the application services that sit between transport
// (REST, gRPC) and the persistence port. Services own cross-cutting concerns:
// validation, transaction boundaries, metrics, and logging. They contain no SQL
// and no HTTP; the domain owns the rules and the adapter owns the storage.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// DefaultIdempotencyTTL is how long an idempotency key blocks reuse when the
// service is constructed without WithIdempotencyTTL (Task 4.5, audit A1.4):
// generous enough that existing tests' within-test retries still replay, and
// the same default cmd/server falls back to for IDEMPOTENCY_TTL.
const DefaultIdempotencyTTL = 24 * time.Hour

// TransactionService posts transactions to the ledger. It is the single entry
// point both the REST and gRPC layers will call to move money.
type TransactionService struct {
	repo           domain.Repository
	log            *slog.Logger
	tracer         oteltrace.Tracer
	fxProvider     fx.Provider
	idempotencyTTL time.Duration
	prePostHook    PrePostHook
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

// WithIdempotencyTTL sets how long a stored idempotency key blocks reuse
// before GetIdempotencyKey starts treating it as absent (Task 4.5, audit
// A1.4). A TransactionService constructed without this option uses
// DefaultIdempotencyTTL. ttl <= 0 is ignored (falls back to the default)
// rather than stamping every key pre-expired.
func WithIdempotencyTTL(ttl time.Duration) ServiceOption {
	return func(s *TransactionService) {
		if ttl > 0 {
			s.idempotencyTTL = ttl
		}
	}
}

// WithPrePostHook sets the PrePostHook Post and Convert call synchronously,
// before either ever writes anything, to let an external screening or
// transaction-monitoring system veto the transaction (Task 6.1, audit
// A9.1). A TransactionService constructed without this option uses
// NoopPrePostHook, which allows every transaction: default behavior is
// unchanged for every existing caller and test that never wires one in.
func WithPrePostHook(h PrePostHook) ServiceOption {
	return func(s *TransactionService) { s.prePostHook = h }
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
	s := &TransactionService{repo: repo, log: log, tracer: tracer, idempotencyTTL: DefaultIdempotencyTTL, prePostHook: NoopPrePostHook{}}
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
// Every real post also writes one append-only audit_outbox row in the same
// transaction (ADR-017): the tamper-evident audit chain itself is built
// asynchronously by a single background chainer, not inside this call, so a
// transaction just posted is durable immediately but its audit-chain link
// appears a short time later (see internal/audit.Chainer).
// Before any of that, t's postings are checked against tenantID's optional
// TenantPolicy guardrails (Task 2.4b, audit A3.4: max transaction amount, daily
// volume, currency allowlist); a tripped guardrail returns a
// *domain.PolicyViolationError. Postings are also checked against each
// touched (non-system) account's status and optional minimum balance (Task
// 5.5, audit A1.5): a frozen or closed account returns a
// *domain.AccountNotActiveError, and a posting that would breach an
// account's floor returns a *domain.MinBalanceBreachError.
//
// Immediately before RunInTx (Task 6.1, audit A9.1), and only for a genuine
// new post, never for a replay (see the idempotency precheck above this
// call): the configured PrePostHook gets one synchronous chance to veto t.
// A rejection (ErrScreeningRejected) or an ambiguous hook failure
// (ErrScreeningUnavailable) both return here, before RunInTx ever opens a
// transaction, so nothing is written for either outcome; see PrePostHook's
// own doc comment for the fail-closed distinction between the two.
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

	// Computed under CurrentFingerprintScheme, and snapshotted as a plain
	// value (before) rather than re-read off *t later, both before RunInTx
	// runs: tx.CreateTransaction below resolves t.EffectiveAt's nil fallback
	// to the row's created_at as a side effect on this same *t, even on an
	// attempt that ultimately rolls back (a Go-level mutation is not undone
	// by a SQL ROLLBACK). Fingerprinting *t again after that point, as
	// replay() below would if it recomputed from *t directly, would hash a
	// resolved timestamp the client never supplied instead of the absent
	// marker the ORIGINAL successful post's fingerprint was computed and
	// stored with, corrupting the "v2" scheme's comparison (Task 4.3, audit
	// A1.3) on every legitimate retry that omits effective_at. Capturing
	// "before" here, ahead of any mutation, is what keeps that comparison
	// honest. ok is only false if CurrentFingerprintScheme itself is not
	// registered in TransactionFingerprint, a programmer error this binary
	// would make about itself, never about caller input.
	fingerprint, ok := domain.TransactionFingerprint(domain.CurrentFingerprintScheme, *t)
	if !ok {
		return false, fmt.Errorf("ledger: fingerprint scheme %q (CurrentFingerprintScheme) is not registered in TransactionFingerprint", domain.CurrentFingerprintScheme)
	}
	before := *t

	// Idempotency is resolved against any EXISTING stored key before ever
	// attempting to write, mirroring Convert's own precheck (see convert.go).
	// Post did not need this before Task 4.3: every prior unique constraint a
	// retry could trip (transactions_pkey, the one-reversal index) only ever
	// fired for a genuinely NEW transaction, never for an identical retry, so
	// letting RunInTx's own ErrDuplicateIdempotencyKey handling below catch
	// the retry was enough. transactions_tenant_reference_idx changes that:
	// an identical retry that reuses the SAME reference now races its OWN
	// prior success there too, and CreateTransaction runs before
	// InsertIdempotencyKey inside RunInTx, so that reference collision would
	// surface as ErrDuplicateReference and return a spurious conflict instead
	// of ever reaching the idempotency-key check that should have replayed
	// it. Checking here first avoids attempting that second insert at all for
	// the common (non-concurrent) retry. RunInTx's post-hoc
	// ErrDuplicateIdempotencyKey handling still exists below to catch the
	// remaining race window: two concurrent identical requests for a
	// brand-new key can both pass this precheck before either commits (see
	// TestPostIdempotentHammer).
	if idem != nil {
		_, err := s.repo.GetIdempotencyKey(ctx, tenantID, idem.Key)
		switch {
		case err == nil:
			return s.replay(ctx, tenantID, idem.Key, before, t)
		case errors.Is(err, domain.ErrIdempotencyKeyNotFound):
			// No existing key: proceed with a real post.
		default:
			span.RecordError(err)
			return false, err
		}
	}

	// Screening runs here, synchronously, and BEFORE RunInTx opens any
	// transaction (Task 6.1, audit A9.1): a rejection or an ambiguous hook
	// failure both return before a single row is written. This intentionally
	// happens after the idempotency precheck above (a replay returns before
	// reaching here), so an already-approved, already-posted transaction is
	// never re-screened on retry; only a genuinely new post is.
	if err := reviewPost(ctx, s.prePostHook, tenantID, t); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "screening rejected post")
		metrics.PostDuration.WithLabelValues("rejected").Observe(0)
		return false, err
	}

	start := time.Now()
	runErr := s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, tx domain.Tx) error {
		if err := enforceTenantPolicy(ctx, tx, tenantID, policy, t.Postings); err != nil {
			return err
		}
		if err := enforceAccountConstraints(ctx, tx, tenantID, t.Postings); err != nil {
			return err
		}
		if err := tx.CreateTransaction(ctx, tenantID, t); err != nil {
			return err
		}
		if idem != nil {
			if err := tx.InsertIdempotencyKey(ctx, tenantID, idem.Key, fingerprint, domain.CurrentFingerprintScheme, t.ID, s.idempotencyTTL); err != nil {
				return err
			}
		}
		after, err := json.Marshal(auditSnapshot(t))
		if err != nil {
			return err
		}
		return tx.AppendAuditOutbox(ctx, tenantID, domain.AuditEvent{
			Action:        domain.ActionTransactionCreated,
			TransactionID: t.ID,
			Actor:         tenantID,
			After:         after,
		})
	})
	elapsed := time.Since(start).Seconds()

	if runErr != nil {
		if idem != nil && errors.Is(runErr, domain.ErrDuplicateIdempotencyKey) {
			return s.replay(ctx, tenantID, idem.Key, before, t)
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

// replay resolves a duplicate idempotency key: it recomputes before's
// fingerprint under the SCHEME THE STORED RECORD CARRIES (rec.Scheme), not
// necessarily the scheme this binary currently writes
// (domain.CurrentFingerprintScheme), so a fingerprint-scheme change never
// false-conflicts a key stored under an older scheme (Task 2.3, audit A1.6).
// before is the pre-RunInTx snapshot Post captured, not a fresh read off *t:
// see Post's doc comment at the call site for why recomputing from *t here
// instead would corrupt the comparison for the "v2" scheme (Task 4.3, audit
// A1.3), whose fingerprint includes EffectiveAt, once tx.CreateTransaction's
// nil fallback has mutated it. If the recomputation over before matches the
// stored fingerprint, it loads the original transaction into t and reports a
// replay; if not, the key was reused with a different body and it returns
// ErrIdempotencyConflict. If rec.Scheme is not one this binary knows how to
// compute (for example a key written by a newer binary, then read by this
// one after a downgrade), it fails closed with ErrIdempotencyConflict rather
// than risk replaying a transaction it cannot verify the body of.
func (s *TransactionService) replay(ctx context.Context, tenantID, key string, before domain.Transaction, t *domain.Transaction) (bool, error) {
	rec, err := s.repo.GetIdempotencyKey(ctx, tenantID, key)
	if err != nil {
		return false, err
	}
	expected, ok := domain.TransactionFingerprint(rec.Scheme, before)
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
//
// When t is itself a reversal (t.ReversesTransactionID is set), the snapshot
// also carries reverses_transaction_id: the id of the original transaction it
// reverses. Without it, the reversal's audit row names only itself, so an
// auditor reading the tamper-evident chain in isolation cannot tell what got
// reversed without joining back to the transactions table, defeating the
// point of a self-contained snapshot. The field is omitted entirely for
// ordinary (non-reversal) transactions, so this stays byte-identical to the
// pre-existing snapshot for the create path and every previously written
// audit row's hash is unaffected.
//
// effective_at (Task 4.3, audit A1.3) is always included: by the time this
// runs, t.CreateTransaction has already resolved the read-time fallback to
// created_at (see postgres.txRepo.CreateTransaction), so the value here is
// never the zero time even for a caller that supplied none. reference is
// included only when set, the same optional-field convention
// reverses_transaction_id uses above: most transactions carry no external
// reference at all.
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
	if t.ReversesTransactionID != nil {
		snapshot["reverses_transaction_id"] = *t.ReversesTransactionID
	}
	if t.Reference != nil {
		snapshot["reference"] = *t.Reference
	}
	if t.EffectiveAt != nil {
		snapshot["effective_at"] = *t.EffectiveAt
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

// MaxExportRows bounds ExportTransactions (Task 4.4, audit A7.2): an export is
// a single unpaged call, unlike ListTransactions, so it needs its own ceiling
// rather than trusting a caller-supplied limit. 10000 rows is generous for the
// reconciliation exports this endpoint is for, while still ruling out an
// unbounded scan that could OOM the process on a tenant with a very long
// history.
const MaxExportRows = 10000

// ListTransactions returns up to limit of tenantID's transactions matching
// filter, newest first, keyset paged from after (Task 4.4, audit A7.2). It is
// a thin read-through to the repository, the same shape AuditService.ByAccount
// and AccountService.Statement already use: no cross-cutting logic belongs
// here beyond the pass-through itself.
func (s *TransactionService) ListTransactions(ctx context.Context, tenantID string, filter domain.TransactionFilter, after *domain.StatementCursor, limit int) ([]domain.TransactionListItem, error) {
	return s.repo.ListTransactions(ctx, tenantID, filter, after, limit)
}

// ExportTransactions returns every one of tenantID's transactions matching
// filter, newest first, up to MaxExportRows (Task 4.4, audit A7.2). Unlike
// ListTransactions this is not paged: it is the single bounded read behind
// GET /v1/transactions/export, REST-only (a streaming CSV export does not fit
// gRPC's single-response model well). truncated is true when the tenant's
// matching history is larger than MaxExportRows, in which case the export
// silently contains only the newest MaxExportRows rows rather than growing
// unbounded; a truncated export is logged at warn level so an operator can
// see it happened, and the caller surfaces it too (see the REST handler's
// Export-Truncated response header).
func (s *TransactionService) ExportTransactions(ctx context.Context, tenantID string, filter domain.TransactionFilter) (items []domain.TransactionListItem, truncated bool, err error) {
	rows, err := s.repo.ListTransactions(ctx, tenantID, filter, nil, MaxExportRows+1)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > MaxExportRows {
		s.log.WarnContext(ctx, "transaction export truncated",
			"tenant_id", tenantID, "max_export_rows", MaxExportRows)
		return rows[:MaxExportRows], true, nil
	}
	return rows, false, nil
}

package ledger

import (
	"context"
	"encoding/json"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/metrics"
)

// ErrNoFXProvider is returned by Convert when the TransactionService was
// constructed without WithFXProvider: there is no rate source to resolve the
// pair against.
var ErrNoFXProvider = errors.New("ledger: convert requires an fx provider (see WithFXProvider)")

// ConvertRequest is a request to move SourceAmount, denominated in the from
// account's own currency, into the to account's own currency, at the
// tenant's current FX rate for that pair (ADR-014). The target currency is
// always the to account's currency, never anything a client supplies
// directly: a client-controlled rate or target currency would be a theft
// vector.
type ConvertRequest struct {
	FromAccountID string
	ToAccountID   string
	SourceAmount  int64
}

// Convert exchanges req.SourceAmount from the from account's currency into
// the to account's currency at the tenant's current FX rate, and posts the
// four resulting legs (debit the from account, credit the source-currency FX
// clearing account, debit the destination-currency FX clearing account,
// credit the to account) atomically, inside the same SERIALIZABLE,
// per-tenant-serialized transaction Post uses (RunInTx, see ADR-012).
//
// Convert deliberately does NOT call Post. Post computes its idempotency
// fingerprint from the built postings (Transaction.Fingerprint), and has no
// channel to carry an FX snapshot into the transaction row. Reusing it here
// would mean: (a) the idempotency key would be keyed on postings that are
// only known after a rate has already been applied, so a retry submitted
// after the rate moved would rebuild a different converted amount, hash to a
// different fingerprint, and spuriously 409 a legitimate retry instead of
// replaying it; and (b) the fx_* snapshot columns would never be written.
// Convert instead resolves idempotency from the REQUEST (from account, to
// account, source amount), before ever calling the rate provider, and drives
// its own RunInTx body that writes the FX snapshot alongside the transaction.
//
// idem may be nil, in which case the conversion is posted without dedup,
// mirroring Post; a transport layer wiring this up for real traffic is
// expected to make the Idempotency-Key header mandatory (ADR-012), the same
// way it already does for a plain post.
func (s *TransactionService) Convert(ctx context.Context, tenantID string, req ConvertRequest, idem *domain.Idempotency) (*domain.Transaction, bool, error) {
	ctx, span := s.tracer.Start(ctx, "ledger.ConvertTransaction",
		oteltrace.WithAttributes(
			attribute.String("tenant_id", tenantID),
			attribute.Bool("idempotency.present", idem != nil),
		),
	)
	defer span.End()

	if s.fxProvider == nil {
		span.RecordError(ErrNoFXProvider)
		return nil, false, ErrNoFXProvider
	}
	// Reject zero and negative up front: zero would otherwise sail through
	// domain.Convert's dust guard (a zero source converts to a zero result,
	// and dust is only ever detected when a NONZERO source rounds to zero),
	// and a negative source would run the conversion in reverse under a
	// request that otherwise looks perfectly well-formed.
	if req.SourceAmount <= 0 {
		span.RecordError(domain.ErrNonPositiveConvertAmount)
		span.SetStatus(codes.Error, "non-positive source amount")
		return nil, false, domain.ErrNonPositiveConvertAmount
	}

	fingerprint := domain.ConvertRequestFingerprint(req.FromAccountID, req.ToAccountID, req.SourceAmount)

	// Idempotency is resolved from the request fingerprint, before the rate
	// lookup: a hit here replays the stored transaction without ever calling
	// the rate provider, so a retry cannot recompute a different converted
	// amount from a rate that has since moved.
	if idem != nil {
		rec, err := s.repo.GetIdempotencyKey(ctx, tenantID, idem.Key)
		switch {
		case err == nil:
			return s.convertReplay(ctx, tenantID, idem.Key, fingerprint, rec)
		case errors.Is(err, domain.ErrIdempotencyKeyNotFound):
			// No existing key: proceed with a real conversion.
		default:
			span.RecordError(err)
			return nil, false, err
		}
	}

	from, err := s.repo.GetAccount(ctx, tenantID, req.FromAccountID)
	if err != nil {
		span.RecordError(err)
		return nil, false, err
	}
	to, err := s.repo.GetAccount(ctx, tenantID, req.ToAccountID)
	if err != nil {
		span.RecordError(err)
		return nil, false, err
	}
	if from.ID == to.ID {
		return nil, false, domain.ErrSelfConversion
	}
	if from.Currency == to.Currency {
		return nil, false, domain.ErrSameCurrencyConversion
	}

	quote, spreadBps, err := s.fxProvider.Rate(ctx, from.Currency, to.Currency)
	if err != nil {
		span.RecordError(err)
		return nil, false, err
	}

	source, err := domain.NewMoney(req.SourceAmount, from.Currency)
	if err != nil {
		return nil, false, err
	}
	converted, appliedE8, err := domain.Convert(source, to.Currency, quote.MidRateE8, spreadBps)
	if err != nil {
		span.RecordError(err)
		return nil, false, err
	}

	clearingFrom, err := s.repo.GetOrCreateClearingAccount(ctx, tenantID, from.Currency)
	if err != nil {
		span.RecordError(err)
		return nil, false, err
	}
	clearingTo, err := s.repo.GetOrCreateClearingAccount(ctx, tenantID, to.Currency)
	if err != nil {
		span.RecordError(err)
		return nil, false, err
	}

	// Overflow-checked negation (Money.Neg errors on math.MinInt64, whose
	// negation does not fit int64), not a bare sign flip, per ADR-014.
	negSource, err := source.Neg()
	if err != nil {
		return nil, false, err
	}
	negConverted, err := converted.Neg()
	if err != nil {
		return nil, false, err
	}

	t := &domain.Transaction{
		Postings: []domain.Posting{
			{AccountID: from.ID, Amount: negSource, Description: "convert: debit source account"},
			{AccountID: clearingFrom.ID, Amount: source, Description: "convert: source currency clearing"},
			{AccountID: clearingTo.ID, Amount: negConverted, Description: "convert: destination currency clearing"},
			{AccountID: to.ID, Amount: converted, Description: "convert: credit destination account"},
		},
		FX: &domain.FXDetail{
			SourceAmount:    req.SourceAmount,
			ConvertedAmount: converted.Amount(),
			MidRateE8:       quote.MidRateE8,
			AppliedE8:       appliedE8,
			SpreadBps:       spreadBps,
			RateSource:      quote.Source,
			EffectiveAt:     quote.EffectiveAt,
			RateID:          quote.RateID,
		},
	}
	// Transaction.Validate groups postings by currency and checks each
	// currency's postings sum to zero (ADR-014); the four legs above are
	// built so USD (say) and EUR each net independently through the clearing
	// accounts. This is the same defense-in-depth Post applies: validate
	// before ever opening a transaction, not inside the retried unit of work.
	if err := t.Validate(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "validation failed")
		return nil, false, err
	}

	runErr := s.repo.RunInTx(ctx, tenantID, func(ctx context.Context, tx domain.Tx) error {
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
	if runErr != nil {
		if idem != nil && errors.Is(runErr, domain.ErrDuplicateIdempotencyKey) {
			// A concurrent convert for this tenant and key committed between
			// our precheck and this attempt's insert (the per-tenant RunInTx
			// mutex makes this a narrow, defense-in-depth window rather than
			// the common case). Replay it exactly as the precheck would have.
			rec, err := s.repo.GetIdempotencyKey(ctx, tenantID, idem.Key)
			if err != nil {
				span.RecordError(err)
				return nil, false, err
			}
			return s.convertReplay(ctx, tenantID, idem.Key, fingerprint, rec)
		}
		span.RecordError(runErr)
		span.SetStatus(codes.Error, "convert failed")
		s.log.ErrorContext(ctx, "convert failed", "tenant_id", tenantID, "error", runErr)
		return nil, false, runErr
	}

	s.log.InfoContext(ctx, "transaction converted",
		"tenant_id", tenantID, "transaction_id", t.ID,
		"from_account", from.ID, "to_account", to.ID)
	return t, false, nil
}

// convertReplay resolves a hit against the convert idempotency key: if the
// stored fingerprint matches the request's, it loads and returns the
// previously posted transaction with replayed=true; if the key was reused for
// a different request, it returns domain.ErrIdempotencyConflict.
func (s *TransactionService) convertReplay(ctx context.Context, tenantID, key, fingerprint string, rec domain.IdempotencyRecord) (*domain.Transaction, bool, error) {
	if rec.Fingerprint != fingerprint {
		metrics.IdempotencyConflicts.Inc()
		return nil, false, domain.ErrIdempotencyConflict
	}
	existing, err := s.repo.GetTransaction(ctx, tenantID, rec.TransactionID)
	if err != nil {
		return nil, false, err
	}
	metrics.IdempotencyReplays.Inc()
	s.log.InfoContext(ctx, "convert replayed",
		"tenant_id", tenantID, "transaction_id", existing.ID, "idempotency_key", key)
	return &existing, true, nil
}

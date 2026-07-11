// Package fx resolves FX rates for cross-currency conversion (ADR-014) and
// seeds the append-only fx_rates table from the FX_RATES environment
// variable at boot. It has no opinion on spread policy or how a converted
// amount is computed; that lives in domain.Convert. This package only
// answers "what is the current mid rate and spread for this pair."
package fx

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// Provider resolves the current mid rate (quote units per one base unit,
// scaled by domain.RateScale) and the spread (in basis points) to apply on
// top of it for a given currency pair. Implementations may serve this from
// storage, an external rate feed, or anything else; the conversion service
// depends only on this interface (see ADR-014, decision 4).
type Provider interface {
	// Rate returns the current mid quote for converting base into quote for
	// tenantID, plus the spread_bps to widen it by, or an error if no rate
	// is known for the pair in either direction. A tenant-specific rate for
	// the pair takes priority over the global default; a tenant with no
	// rate of its own resolves the global default (Task 2.4, audit A3.3;
	// see migration 0014 and the CurrentFXRate query).
	Rate(ctx context.Context, tenantID string, base, quote domain.Currency) (domain.FXQuote, int32, error)
}

// dbRateProvider is the v1 Provider: it reads the current row from fx_rates
// (populated by Seed, and in the future by a live rate feed writing into the
// same table). It is inverse-aware: a pair only needs to be stored in one
// direction, and the reverse is derived by inverting the mid.
type dbRateProvider struct {
	q *sqlc.Queries
}

// NewDBProvider builds a Provider backed by fx_rates. db may be a
// *pgxpool.Pool, a pgx.Tx, or anything else satisfying sqlc.DBTX.
func NewDBProvider(db sqlc.DBTX) Provider {
	return &dbRateProvider{q: sqlc.New(db)}
}

// Rate tries CurrentFXRate(tenantID, base, quote) first. If that pair is not
// stored (in either the tenant's own rows or the global default), it tries
// the reverse, CurrentFXRate(tenantID, quote, base), and inverts the mid so
// the result is still expressed as quote-per-base; the spread returned is
// the one stored on whichever row was actually found (a spread is a
// property of the quoted pair, not of the direction it happens to be
// requested in). Both lookups resolve a tenant-specific row for tenantID
// ahead of the global default (Task 2.4, audit A3.3), so the inverse pair
// honors a tenant's own rate too, not just the direct pair. If neither
// direction has a row, it returns domain.ErrFXRateNotFound.
func (p *dbRateProvider) Rate(ctx context.Context, tenantID string, base, quote domain.Currency) (domain.FXQuote, int32, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.FXQuote{}, 0, fmt.Errorf("fx: parse tenant id %q: %w", tenantID, err)
	}
	pgTenantID := pgtype.UUID{Bytes: tid, Valid: true}

	direct, err := p.q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{TenantID: pgTenantID, Base: string(base), Quote: string(quote)})
	if err == nil {
		return domain.FXQuote{
			Base: base, Quote: quote, MidRateE8: direct.MidRateE8,
			RateID: direct.ID, Source: direct.Source, EffectiveAt: direct.EffectiveAt,
		}, direct.SpreadBps, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.FXQuote{}, 0, fmt.Errorf("fx: lookup %s/%s: %w", base, quote, err)
	}

	// No direct quote: try a single inversion of the reverse pair. This is
	// deliberately not a multi-hop search through a third currency; v1
	// stores and inverts exactly the pairs the ledger needs (ADR-014).
	inverse, err := p.q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{TenantID: pgTenantID, Base: string(quote), Quote: string(base)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.FXQuote{}, 0, fmt.Errorf("%w: %s/%s", domain.ErrFXRateNotFound, base, quote)
		}
		return domain.FXQuote{}, 0, fmt.Errorf("fx: lookup %s/%s: %w", quote, base, err)
	}
	if inverse.MidRateE8 <= 0 {
		// fx_rates has a CHECK (mid_rate_e8 > 0), so this should be
		// unreachable in practice; guarded anyway so a division never runs
		// against zero.
		return domain.FXQuote{}, 0, fmt.Errorf("%w: %s/%s", domain.ErrNonPositiveRate, quote, base)
	}

	// If 1 quote-currency unit buys inverse.MidRateE8 / RateScale base-currency
	// units, then 1 base-currency unit buys RateScale / (inverse.MidRateE8 /
	// RateScale) = RateScale*RateScale / inverse.MidRateE8 quote-currency
	// units. RateScale*RateScale is 1e16, well inside int64 range, so this
	// stays exact integer division throughout.
	invertedMidE8 := (domain.RateScale * domain.RateScale) / inverse.MidRateE8

	return domain.FXQuote{
		Base: base, Quote: quote, MidRateE8: invertedMidE8,
		// Provenance is the row that was actually found and inverted: there is
		// no separate stored row for this direction (see FXQuote's doc comment).
		RateID: inverse.ID, Source: inverse.Source, EffectiveAt: inverse.EffectiveAt,
	}, inverse.SpreadBps, nil
}

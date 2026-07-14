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
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// fxHubCurrency is the fixed hub used to triangulate a cross-currency pair that
// has no direct or inverse rate of its own (ADR-022): a conversion between two
// non-USD currencies is priced through their USD legs.
const fxHubCurrency = domain.Currency("USD")

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
// (populated by Seed and by the live rate feed, both writing into the same
// table). It is inverse-aware: a pair only needs to be stored in one
// direction, and the reverse is derived by inverting the mid.
type dbRateProvider struct {
	q      *sqlc.Queries
	maxAge time.Duration // 0 disables the staleness check
}

// ProviderOption configures a dbRateProvider built by NewDBProvider.
type ProviderOption func(*dbRateProvider)

// WithMaxRateAge rejects any resolved rate whose effective_at is older than
// age, with domain.ErrFXRateStale (audit A: no FX rate staleness guard). A
// zero or negative age (the default) disables the check, preserving the prior
// behavior of pricing against whatever the latest row is.
func WithMaxRateAge(age time.Duration) ProviderOption {
	return func(p *dbRateProvider) { p.maxAge = age }
}

// NewDBProvider builds a Provider backed by fx_rates. db may be a
// *pgxpool.Pool, a pgx.Tx, or anything else satisfying sqlc.DBTX.
func NewDBProvider(db sqlc.DBTX, opts ...ProviderOption) Provider {
	p := &dbRateProvider{q: sqlc.New(db)}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// freshOrErr returns q unchanged when its effective_at is within maxAge (or the
// check is disabled), otherwise domain.ErrFXRateStale. Every success path in
// Rate routes its resolved quote through this so the staleness rule lives in
// one place, whether the quote came from a direct row, an inversion, or the
// USD hub.
func (p *dbRateProvider) freshOrErr(q domain.FXQuote) (domain.FXQuote, error) {
	if p.maxAge <= 0 {
		return q, nil
	}
	if time.Since(q.EffectiveAt) > p.maxAge {
		return domain.FXQuote{}, fmt.Errorf("%w: %s/%s rate from %s is older than %s",
			domain.ErrFXRateStale, q.Base, q.Quote, q.EffectiveAt.UTC().Format(time.RFC3339), p.maxAge)
	}
	return q, nil
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
		spread, sErr := p.resolveSpread(ctx, pgTenantID, direct.SpreadBps)
		if sErr != nil {
			return domain.FXQuote{}, 0, sErr
		}
		q, fErr := p.freshOrErr(domain.FXQuote{
			Base: base, Quote: quote, MidRateE8: direct.MidRateE8,
			RateID: direct.ID, Source: direct.Source, EffectiveAt: direct.EffectiveAt,
		})
		return q, spread, fErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.FXQuote{}, 0, fmt.Errorf("fx: lookup %s/%s: %w", base, quote, err)
	}

	// No direct quote: try a single inversion of the reverse pair. This is
	// deliberately not a multi-hop search through a third currency; v1
	// stores and inverts exactly the pairs the ledger needs (ADR-014).
	inverse, err := p.q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{TenantID: pgTenantID, Base: string(quote), Quote: string(base)})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.FXQuote{}, 0, fmt.Errorf("fx: lookup %s/%s: %w", quote, base, err)
		}
		// Neither the direct pair nor its inverse is stored. Try the USD hub
		// (ADR-022): a cross pair whose two USD legs are both configured.
		hq, hs, ok, herr := p.hubQuote(ctx, pgTenantID, base, quote)
		if herr != nil {
			return domain.FXQuote{}, 0, herr
		}
		if ok {
			q, fErr := p.freshOrErr(hq)
			return q, hs, fErr
		}
		return domain.FXQuote{}, 0, fmt.Errorf("%w: %s/%s", domain.ErrFXRateNotFound, base, quote)
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

	spread, sErr := p.resolveSpread(ctx, pgTenantID, inverse.SpreadBps)
	if sErr != nil {
		return domain.FXQuote{}, 0, sErr
	}
	q, fErr := p.freshOrErr(domain.FXQuote{
		Base: base, Quote: quote, MidRateE8: invertedMidE8,
		// Provenance is the row that was actually found and inverted: there is
		// no separate stored row for this direction (see FXQuote's doc comment).
		RateID: inverse.ID, Source: inverse.Source, EffectiveAt: inverse.EffectiveAt,
	})
	return q, spread, fErr
}

// leg is one directed rate a hub conversion uses: the mid (quote per base,
// scaled by RateScale), the spread of the row it came from, and that row's
// provenance.
type leg struct {
	midE8       int64
	spread      pgtype.Int4
	rateID      int64
	source      string
	effectiveAt time.Time
}

// legMid resolves base to quote for one hop the same way Rate resolves a direct
// pair: a stored base to quote row, or the inverse of a stored quote to base
// row. ok is false (with a nil error) when neither exists, so the caller can
// decline the hub cleanly.
func (p *dbRateProvider) legMid(ctx context.Context, tid pgtype.UUID, base, quote domain.Currency) (leg, bool, error) {
	d, err := p.q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{TenantID: tid, Base: string(base), Quote: string(quote)})
	if err == nil {
		return leg{midE8: d.MidRateE8, spread: d.SpreadBps, rateID: d.ID, source: d.Source, effectiveAt: d.EffectiveAt}, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return leg{}, false, fmt.Errorf("fx: lookup %s/%s: %w", base, quote, err)
	}
	inv, err := p.q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{TenantID: tid, Base: string(quote), Quote: string(base)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return leg{}, false, nil
		}
		return leg{}, false, fmt.Errorf("fx: lookup %s/%s: %w", quote, base, err)
	}
	if inv.MidRateE8 <= 0 {
		return leg{}, false, fmt.Errorf("%w: %s/%s", domain.ErrNonPositiveRate, quote, base)
	}
	inverted := (domain.RateScale * domain.RateScale) / inv.MidRateE8
	return leg{midE8: inverted, spread: inv.SpreadBps, rateID: inv.ID, source: inv.Source, effectiveAt: inv.EffectiveAt}, true, nil
}

// hubQuote prices base to quote through the USD hub (ADR-022): compose USD per
// base with quote per USD. The markup is the base-to-USD leg's spread. Returns
// ok false (nil error) when either leg is missing, or when base or quote is the
// hub itself (a USD leg would have resolved directly). Provenance is the base
// leg's row: there is no single stored row for the cross pair.
func (p *dbRateProvider) hubQuote(ctx context.Context, tid pgtype.UUID, base, quote domain.Currency) (domain.FXQuote, int32, bool, error) {
	if base == fxHubCurrency || quote == fxHubCurrency {
		return domain.FXQuote{}, 0, false, nil
	}
	fromLeg, ok, err := p.legMid(ctx, tid, base, fxHubCurrency)
	if err != nil || !ok {
		return domain.FXQuote{}, 0, false, err
	}
	toLeg, ok, err := p.legMid(ctx, tid, fxHubCurrency, quote)
	if err != nil || !ok {
		return domain.FXQuote{}, 0, false, err
	}
	// cross mid (quote per base) = fromLeg.midE8 * toLeg.midE8 / RateScale, in
	// big.Int so the intermediate product cannot overflow int64.
	cross := new(big.Int).Mul(big.NewInt(fromLeg.midE8), big.NewInt(toLeg.midE8))
	cross.Quo(cross, big.NewInt(domain.RateScale))
	if cross.Sign() <= 0 {
		return domain.FXQuote{}, 0, false, fmt.Errorf("%w: %s/%s via %s", domain.ErrNonPositiveRate, base, quote, fxHubCurrency)
	}
	if !cross.IsInt64() {
		return domain.FXQuote{}, 0, false, fmt.Errorf("fx: composed %s/%s rate via %s overflows int64", base, quote, fxHubCurrency)
	}
	// Widen by BOTH legs' spreads, not just the base leg's (audit A: hub spread
	// under-applied). A two-hop conversion crosses two markups; charging one
	// made cross pairs cheaper than the equivalent pair of direct conversions
	// and left an A->USD->B vs A->B arbitrage. Basis points add (the tiny
	// second-order cross term is immaterial at realistic spreads); the sum is
	// capped just under the fx_rates CHECK ceiling so it stays a valid spread.
	fromSpread, err := p.resolveSpread(ctx, tid, fromLeg.spread)
	if err != nil {
		return domain.FXQuote{}, 0, false, err
	}
	toSpread, err := p.resolveSpread(ctx, tid, toLeg.spread)
	if err != nil {
		return domain.FXQuote{}, 0, false, err
	}
	spread := fromSpread + toSpread
	if spread >= maxSpreadBps {
		spread = maxSpreadBps - 1
	}
	// The cross is only as fresh as its STALER leg: price it off the older
	// effective_at so the staleness guard trips if either USD leg is stale.
	effectiveAt := fromLeg.effectiveAt
	if toLeg.effectiveAt.Before(effectiveAt) {
		effectiveAt = toLeg.effectiveAt
	}
	return domain.FXQuote{
		Base: base, Quote: quote, MidRateE8: cross.Int64(),
		RateID: fromLeg.rateID, Source: fromLeg.source, EffectiveAt: effectiveAt,
	}, spread, true, nil
}

// resolveSpread turns a rate row's nullable spread into the concrete spread a
// conversion applies (ADR-020 precedence): a per-pair override if the row
// carries one, else the tenant's markup default, else the global default, else
// zero. Called for whichever row Rate actually found (direct or inverse), so
// the precedence lives in one place.
func (p *dbRateProvider) resolveSpread(ctx context.Context, tenant pgtype.UUID, rowSpread pgtype.Int4) (int32, error) {
	if rowSpread.Valid {
		return rowSpread.Int32, nil
	}
	return resolveMarkupDefault(ctx, p.q, tenant)
}

// resolveMarkupDefault resolves the markup default a conversion falls back to
// when a rate row carries no per-pair spread override: the tenant's own
// default if tenant is a valid scope AND that tenant's latest row is not
// cleared (its value is non-NULL), else the global default if it exists and
// is itself non-NULL, else zero.
//
// A tenant row can be CLEARED (default_spread_bps NULL): that means "this
// tenant no longer has its own override, follow the global default again"
// (see migration 0031 and ADR-020). Naively taking whatever
// TenantFXMarkupDefault returns without checking Valid would make a cleared
// row look like an explicit zero markup instead of "go look at the global
// default," which is the bug this two-step lookup exists to avoid. A NULL
// global row means no markup at all, i.e. zero: there is no further scope to
// fall back to.
//
// This is shared by dbRateProvider.resolveSpread (this file) and
// AdminService.resolveEffective (admin.go) so the two paths that resolve a
// conversion's effective spread, one at conversion time and one for the
// admin API's display of what a conversion would apply, can never drift
// apart on the precedence rule itself.
func resolveMarkupDefault(ctx context.Context, q *sqlc.Queries, tenant pgtype.UUID) (int32, error) {
	if tenant.Valid {
		t, err := q.TenantFXMarkupDefault(ctx, tenant)
		switch {
		case err == nil:
			if t.DefaultSpreadBps.Valid {
				return t.DefaultSpreadBps.Int32, nil
			}
			// The tenant's latest row is a clear (NULL): fall through to the
			// global default below, exactly as if no tenant row existed.
		case errors.Is(err, pgx.ErrNoRows):
			// No tenant row at all: fall through to the global default.
		default:
			return 0, fmt.Errorf("fx: resolve tenant markup default: %w", err)
		}
	}

	g, err := q.GlobalFXMarkupDefault(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("fx: resolve global markup default: %w", err)
	}
	if !g.DefaultSpreadBps.Valid {
		// The global scope itself is cleared: no markup default anywhere.
		return 0, nil
	}
	return g.DefaultSpreadBps.Int32, nil
}

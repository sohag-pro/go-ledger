package fx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// ErrInvalidFXInput is a caller error (bad currency, non-positive rate, spread
// out of range, base equal to quote). Handlers map it to 422.
var ErrInvalidFXInput = errors.New("fx: invalid input")

// ErrUnknownTenant is returned when a scoped write names a tenant that does not
// exist (a foreign-key violation). Handlers map it to 422.
var ErrUnknownTenant = errors.New("fx: unknown tenant")

const apiSource = "api"

// AdminService writes and reads the FX config tables for the admin API. It is
// the write-side companion to the read-only Provider; both wrap sqlc.Queries.
type AdminService struct {
	q *sqlc.Queries
}

// NewAdminService builds an AdminService backed by db, which may be a
// *pgxpool.Pool, a pgx.Tx, or anything else satisfying sqlc.DBTX.
func NewAdminService(db sqlc.DBTX) *AdminService {
	return &AdminService{q: sqlc.New(db)}
}

// RateView is one current effective rate row for the console, with the resolved
// spread a conversion would actually use.
type RateView struct {
	TenantID           string // "" for the global default
	Base, Quote        string
	MidRateE8          int64
	SpreadBps          *int32 // nil when the row uses the default markup
	EffectiveSpreadBps int32  // resolved: override, else default, else 0
	Source             string
	EffectiveAt        time.Time
}

// MarkupDefault is a single default markup value with its effective time.
type MarkupDefault struct {
	DefaultSpreadBps int32
	EffectiveAt      time.Time
}

// MarkupView is the current markup defaults for a scope: the global default
// and, when a tenant was asked for, that tenant's own override (nil if none).
type MarkupView struct {
	Global *MarkupDefault
	Tenant *MarkupDefault
}

func parseScope(tenantID string) (pgtype.UUID, error) {
	if tenantID == "" {
		return pgtype.UUID{}, nil // global
	}
	id, err := uuid.Parse(tenantID)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: tenant id %q", ErrInvalidFXInput, tenantID)
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

func validCurrency(c string) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func mapFKErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return ErrUnknownTenant
	}
	return err
}

// InsertRate appends a rate row. tenantID "" is the global default. spreadBps
// nil stores NULL (the row uses the markup default at conversion time).
func (s *AdminService) InsertRate(ctx context.Context, tenantID, base, quote string, midE8 int64, spreadBps *int32) (RateView, error) {
	tid, err := parseScope(tenantID)
	if err != nil {
		return RateView{}, err
	}
	if !validCurrency(base) || !validCurrency(quote) {
		return RateView{}, fmt.Errorf("%w: currency must be 3 uppercase letters", ErrInvalidFXInput)
	}
	if base == quote {
		return RateView{}, fmt.Errorf("%w: base and quote must differ", ErrInvalidFXInput)
	}
	if midE8 <= 0 {
		return RateView{}, fmt.Errorf("%w: mid_rate_e8 must be positive", ErrInvalidFXInput)
	}
	sp := pgtype.Int4{}
	if spreadBps != nil {
		if *spreadBps < 0 || *spreadBps >= 10000 {
			return RateView{}, fmt.Errorf("%w: spread_bps out of range", ErrInvalidFXInput)
		}
		sp = pgtype.Int4{Int32: *spreadBps, Valid: true}
	}
	row, err := s.q.InsertFXRate(ctx, sqlc.InsertFXRateParams{
		TenantID: tid, Base: base, Quote: quote, MidRateE8: midE8,
		SpreadBps: sp, Source: apiSource,
	})
	if err != nil {
		return RateView{}, mapFKErr(err)
	}
	eff, err := s.resolveEffective(ctx, tid, row.SpreadBps)
	if err != nil {
		return RateView{}, err
	}
	return toRateView(row.TenantID, row.Base, row.Quote, row.MidRateE8, row.SpreadBps, eff, row.Source, row.EffectiveAt), nil
}

// ListRates returns the current effective rate per pair for tenantID plus the
// global defaults, each with its resolved effective spread.
func (s *AdminService) ListRates(ctx context.Context, tenantID string) ([]RateView, error) {
	tid, err := parseScope(tenantID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListCurrentFXRates(ctx, tid)
	if err != nil {
		return nil, err
	}
	out := make([]RateView, 0, len(rows))
	for _, r := range rows {
		// Resolve against the REQUESTED scope (tid), not the winning row's own
		// tenant_id: ListCurrentFXRates can return a global-fallback row
		// (tenant_id NULL) for a tenant-scoped request, and resolving against
		// that NULL would skip the tenant's own markup default. This mirrors
		// InsertRate below and Provider.resolveSpread, which always resolve
		// against the requested tenant.
		eff, err := s.resolveEffective(ctx, tid, r.SpreadBps)
		if err != nil {
			return nil, err
		}
		out = append(out, toRateView(r.TenantID, r.Base, r.Quote, r.MidRateE8, r.SpreadBps, eff, r.Source, r.EffectiveAt))
	}
	return out, nil
}

// SetMarkup appends a markup-default row. tenantID "" is the global default.
func (s *AdminService) SetMarkup(ctx context.Context, tenantID string, bps int32) (MarkupDefault, error) {
	tid, err := parseScope(tenantID)
	if err != nil {
		return MarkupDefault{}, err
	}
	if bps < 0 || bps >= 10000 {
		return MarkupDefault{}, fmt.Errorf("%w: default_spread_bps out of range", ErrInvalidFXInput)
	}
	row, err := s.q.InsertFXMarkupDefault(ctx, sqlc.InsertFXMarkupDefaultParams{
		TenantID: tid, DefaultSpreadBps: bps, Source: apiSource,
	})
	if err != nil {
		return MarkupDefault{}, mapFKErr(err)
	}
	return MarkupDefault{DefaultSpreadBps: row.DefaultSpreadBps, EffectiveAt: row.EffectiveAt}, nil
}

// GetMarkup returns the current global default and, when tenantID is set, that
// tenant's own override (nil if none).
func (s *AdminService) GetMarkup(ctx context.Context, tenantID string) (MarkupView, error) {
	// Validate the scope before running any query, so a malformed tenant id
	// fails fast instead of after the global-default lookup has already run.
	var tid pgtype.UUID
	if tenantID != "" {
		var err error
		tid, err = parseScope(tenantID)
		if err != nil {
			return MarkupView{}, err
		}
	}
	var v MarkupView
	g, err := s.q.GlobalFXMarkupDefault(ctx)
	if err == nil {
		v.Global = &MarkupDefault{DefaultSpreadBps: g.DefaultSpreadBps, EffectiveAt: g.EffectiveAt}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return MarkupView{}, err
	}
	if tenantID != "" {
		t, err := s.q.TenantFXMarkupDefault(ctx, tid)
		if err == nil {
			v.Tenant = &MarkupDefault{DefaultSpreadBps: t.DefaultSpreadBps, EffectiveAt: t.EffectiveAt}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return MarkupView{}, err
		}
	}
	return v, nil
}

func (s *AdminService) resolveEffective(ctx context.Context, tid pgtype.UUID, rowSpread pgtype.Int4) (int32, error) {
	if rowSpread.Valid {
		return rowSpread.Int32, nil
	}
	d, err := s.q.CurrentFXMarkupDefault(ctx, tid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return d.DefaultSpreadBps, nil
}

func toRateView(tid pgtype.UUID, base, quote string, midE8 int64, sp pgtype.Int4, eff int32, source string, effAt time.Time) RateView {
	rv := RateView{
		Base: base, Quote: quote, MidRateE8: midE8,
		EffectiveSpreadBps: eff, Source: source, EffectiveAt: effAt,
	}
	if tid.Valid {
		rv.TenantID = uuid.UUID(tid.Bytes).String()
	}
	if sp.Valid {
		v := sp.Int32
		rv.SpreadBps = &v
	}
	return rv
}

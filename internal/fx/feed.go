package fx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// feedSource is the fx_rates.source value the live feed writes, distinct from
// envSource ("env") so a rate's provenance says where it came from.
const feedSource = "feed"

// DefaultFeedURL is Frankfurter (frankfurter.dev): a free, keyless, ECB-backed
// rate API that updates on business days. The feed appends fresh global rows so
// FX_MAX_RATE_AGE conversions keep working without a paid rate provider.
const DefaultFeedURL = "https://api.frankfurter.dev/v1/latest"

// DefaultFeedInterval is how often the feed polls when FX_FEED_INTERVAL is
// unset. Frankfurter refreshes at most daily, so a few times a day is ample.
const DefaultFeedInterval = 6 * time.Hour

// FeedConfig configures the live rate feed. Currencies are the quote currencies
// fetched against Base (for example Base=USD, Currencies=[EUR,GBP,JPY] writes
// USD/EUR, USD/GBP, USD/JPY); cross pairs are then priced by USD-hub
// triangulation off these fresh legs. SpreadBps, when non-nil, is written as an
// explicit per-row spread; when nil the rows carry NULL so the tenant/global
// markup default applies (ADR-020).
type FeedConfig struct {
	URL        string
	Base       domain.Currency
	Currencies []domain.Currency
	Interval   time.Duration
	SpreadBps  *int32
}

func (c FeedConfig) withDefaults() FeedConfig {
	if c.URL == "" {
		c.URL = DefaultFeedURL
	}
	if c.Base == "" {
		c.Base = fxHubCurrency
	}
	if c.Interval <= 0 {
		c.Interval = DefaultFeedInterval
	}
	return c
}

// Feed polls an external rate API on an interval and appends fresh global
// fx_rates rows. Writes go through the same append-only InsertFXRate path Seed
// uses, so the balance/currency triggers and the CurrentFXRate "latest wins"
// semantics are unchanged; a new row simply becomes the current rate.
type Feed struct {
	q      *sqlc.Queries
	client *http.Client
	log    *slog.Logger
	cfg    FeedConfig
}

// NewFeed builds a Feed writing through db (a *pgxpool.Pool). log falls back to
// slog.Default() when nil; every zero FeedConfig field falls back to its
// default.
func NewFeed(db sqlc.DBTX, log *slog.Logger, cfg FeedConfig) *Feed {
	if log == nil {
		log = slog.Default()
	}
	return &Feed{
		q:      sqlc.New(db),
		client: &http.Client{Timeout: 15 * time.Second},
		log:    log,
		cfg:    cfg.withDefaults(),
	}
}

// Run polls RefreshOnce every cfg.Interval until ctx is done, after one
// immediate refresh at startup so a fresh boot does not wait a full interval
// for its first rates. A refresh error is logged and the loop continues: a
// transient feed outage must not take the process down, and the staleness
// guard is what protects conversions if the outage outlasts FX_MAX_RATE_AGE.
func (f *Feed) Run(ctx context.Context) {
	if n, err := f.RefreshOnce(ctx); err != nil {
		f.log.LogAttrs(ctx, slog.LevelWarn, "fx feed: initial refresh failed", slog.String("error", err.Error()))
	} else {
		f.log.LogAttrs(ctx, slog.LevelInfo, "fx feed: refreshed rates", slog.Int("rows", n))
	}
	t := time.NewTicker(f.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := f.RefreshOnce(ctx); err != nil {
				f.log.LogAttrs(ctx, slog.LevelWarn, "fx feed: refresh failed", slog.String("error", err.Error()))
			} else if n > 0 {
				f.log.LogAttrs(ctx, slog.LevelInfo, "fx feed: refreshed rates", slog.Int("rows", n))
			}
		}
	}
}

// frankfurterResponse is the shape of a Frankfurter /latest response: a base
// currency and a map of quote currency to rate (quote units per one base unit).
type frankfurterResponse struct {
	Base  string             `json:"base"`
	Rates map[string]float64 `json:"rates"`
}

// RefreshOnce fetches the configured pairs and appends a fresh global row for
// each rate that changed since the last stored value. It returns the number of
// rows written. A pair whose latest stored mid already equals the fetched mid
// is skipped, so an unchanged daily rate does not append a new row every
// interval.
func (f *Feed) RefreshOnce(ctx context.Context) (int, error) {
	if len(f.cfg.Currencies) == 0 {
		return 0, nil
	}
	rates, err := f.fetch(ctx)
	if err != nil {
		return 0, err
	}

	var spread pgtype.Int4
	if f.cfg.SpreadBps != nil {
		spread = pgtype.Int4{Int32: *f.cfg.SpreadBps, Valid: true}
	}

	written := 0
	for _, quote := range f.cfg.Currencies {
		if quote == f.cfg.Base {
			continue
		}
		rate, ok := rates[string(quote)]
		if !ok || rate <= 0 {
			f.log.LogAttrs(ctx, slog.LevelWarn, "fx feed: no usable rate returned",
				slog.String("base", string(f.cfg.Base)), slog.String("quote", string(quote)))
			continue
		}
		midE8 := int64(math.Round(rate * float64(domain.RateScale)))
		if midE8 <= 0 {
			continue
		}

		// Skip when the current stored mid already matches: the feed appends
		// only real changes, mirroring Seed's own dedup.
		cur, err := f.q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{
			TenantID: pgtype.UUID{Valid: false}, Base: string(f.cfg.Base), Quote: string(quote),
		})
		if err == nil && cur.MidRateE8 == midE8 {
			continue
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return written, fmt.Errorf("fx feed: lookup %s/%s: %w", f.cfg.Base, quote, err)
		}

		if _, err := f.q.InsertFXRate(ctx, sqlc.InsertFXRateParams{
			// TenantID NULL: feed rows are the global default, like Seed's.
			Base:      string(f.cfg.Base),
			Quote:     string(quote),
			MidRateE8: midE8,
			Source:    feedSource,
			SpreadBps: spread,
			// EffectiveAt NULL: the query stamps now() from the DB clock, so a
			// fresh row is genuinely fresh against the staleness guard.
		}); err != nil {
			return written, fmt.Errorf("fx feed: insert %s/%s: %w", f.cfg.Base, quote, err)
		}
		written++
	}
	return written, nil
}

// fetch GETs the configured base's latest rates. The URL is a fixed operator
// -configured endpoint (not tenant input), so it is not an SSRF surface.
func (f *Feed) fetch(ctx context.Context) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.cfg.URL+"?base="+string(f.cfg.Base), nil)
	if err != nil {
		return nil, fmt.Errorf("fx feed: build request: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fx feed: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fx feed: unexpected status %d", resp.StatusCode)
	}
	var body frankfurterResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("fx feed: decode: %w", err)
	}
	return body.Rates, nil
}

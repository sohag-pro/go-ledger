package fx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// envSource is the fx_rates.source value written by Seed, so a rate row's
// provenance (the FX_RATES env var vs. a live rate feed, once one exists)
// stays visible in the row itself and in anything derived from it.
const envSource = "env"

// rateFracDigits is how many fractional digits mid_rate_e8 carries
// (domain.RateScale is 1e8, ten to the eighth).
const rateFracDigits = 8

// maxSpreadBps mirrors the fx_rates CHECK constraint and domain.Convert:
// spread_bps must be in [0, maxSpreadBps).
const maxSpreadBps = 10_000

// ErrMalformedFXRate is returned when an FX_RATES entry cannot be parsed at
// all: a missing separator or a currency code that is not three uppercase
// letters. A syntactically valid entry with a rate or spread out of range
// surfaces domain.ErrNonPositiveRate or domain.ErrInvalidSpread instead, so
// callers can tell "this isn't shaped like an entry" apart from "this entry
// names a value the ledger will not accept."
var ErrMalformedFXRate = errors.New("fx: malformed FX_RATES entry")

// Seed parses raw, the FX_RATES environment variable, and inserts one
// fx_rates row per entry via InsertFXRate, with effective_at set to now().
//
// raw is a comma-separated list of "BASE:QUOTE=rate/spreadBps" entries, for
// example "USD:EUR=0.9200/25,USD:BDT=110.50/50". An empty (or all
// whitespace) raw is a no-op: not every deployment configures static rates.
//
// Entries are processed one at a time, in order. The first entry that fails
// to parse or names an out-of-range rate or spread stops Seed and returns
// that error; entries before it in the same call, if any, have already been
// inserted (Seed does not roll them back, since each is an independent,
// already-durable append to an append-only table).
//
// fx_rates never gets an UPDATE from this function: InsertFXRate is always a
// plain INSERT (ADR-014's append-only history). What Seed guards against
// instead is piling up rows that say exactly what the current row already
// says, which matters because Seed is expected to run every time the
// process starts, not just once. Before inserting, it compares the parsed
// (mid_rate_e8, spread_bps) against CurrentFXRate for that pair: if they
// match, the row is redundant and Seed skips it; if they differ (including
// when there is no current row yet), Seed inserts, so a genuine change in
// FX_RATES between deploys still lands as a new row and the pair's history
// still grows the way a real rate change would.
func Seed(ctx context.Context, db sqlc.DBTX, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	q := sqlc.New(db)
	now := time.Now().UTC()

	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}

		e, err := parseEntry(field)
		if err != nil {
			return err
		}

		current, err := q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{Base: e.base, Quote: e.quote})
		switch {
		case err == nil:
			if current.MidRateE8 == e.midE8 && current.SpreadBps == e.spreadBps {
				continue // unchanged since the last seed: do not duplicate it
			}
		case errors.Is(err, pgx.ErrNoRows):
			// no current row for this pair yet: fall through to insert
		default:
			return fmt.Errorf("fx: seed lookup %s/%s: %w", e.base, e.quote, err)
		}

		if _, err := q.InsertFXRate(ctx, sqlc.InsertFXRateParams{
			Base:        e.base,
			Quote:       e.quote,
			MidRateE8:   e.midE8,
			SpreadBps:   e.spreadBps,
			Source:      envSource,
			EffectiveAt: now,
		}); err != nil {
			return fmt.Errorf("fx: seed insert %s/%s: %w", e.base, e.quote, err)
		}
	}
	return nil
}

// entry is one parsed FX_RATES field.
type entry struct {
	base, quote string
	midE8       int64
	spreadBps   int32
}

// parseEntry parses one "BASE:QUOTE=rate/spreadBps" field, for example
// "USD:EUR=0.9200/25".
func parseEntry(field string) (entry, error) {
	pair, rateAndSpread, ok := strings.Cut(field, "=")
	if !ok {
		return entry{}, fmt.Errorf("%w: %q (want BASE:QUOTE=rate/spreadBps)", ErrMalformedFXRate, field)
	}

	baseStr, quoteStr, ok := strings.Cut(pair, ":")
	if !ok {
		return entry{}, fmt.Errorf("%w: %q (want BASE:QUOTE=rate/spreadBps)", ErrMalformedFXRate, field)
	}
	base := domain.Currency(strings.TrimSpace(baseStr))
	quote := domain.Currency(strings.TrimSpace(quoteStr))
	if err := base.Validate(); err != nil {
		return entry{}, fmt.Errorf("%w: %q (base currency): %v", ErrMalformedFXRate, field, err)
	}
	if err := quote.Validate(); err != nil {
		return entry{}, fmt.Errorf("%w: %q (quote currency): %v", ErrMalformedFXRate, field, err)
	}
	if base == quote {
		return entry{}, fmt.Errorf("%w: %q (base and quote must differ)", ErrMalformedFXRate, field)
	}

	rateStr, spreadStr, ok := strings.Cut(rateAndSpread, "/")
	if !ok {
		return entry{}, fmt.Errorf("%w: %q (want rate/spreadBps)", ErrMalformedFXRate, field)
	}

	midE8, err := parseRateE8(strings.TrimSpace(rateStr))
	if err != nil {
		return entry{}, fmt.Errorf("%w: %q: %w", ErrMalformedFXRate, field, err)
	}

	spreadBps, err := parseSpreadBps(strings.TrimSpace(spreadStr))
	if err != nil {
		return entry{}, fmt.Errorf("%w: %q: %w", ErrMalformedFXRate, field, err)
	}

	return entry{base: string(base), quote: string(quote), midE8: midE8, spreadBps: spreadBps}, nil
}

// parseRateE8 turns a plain decimal rate string, for example "0.9200" or
// "110.50" or "1", into its domain.RateScale-scaled integer (92000000,
// 11050000000, 100000000). The scaling is pure string and integer work: it
// splits the string on ".", pads or truncates the fractional part to
// rateFracDigits, concatenates the two halves into one digit string, and
// converts that digit string to int64 with strconv.ParseInt (base 10, never
// a decimal-to-binary approximation). A rate is money-adjacent, and
// go-ledger's rule for anything on the money path is integer only (see
// domain.Convert); this function is where that rule starts, at the point
// the rate first enters the process from an operator-supplied string.
func parseRateE8(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty rate")
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" || !isDigits(intPart) {
		return 0, fmt.Errorf("rate %q is not a plain decimal number", s)
	}

	switch {
	case !hasFrac:
		fracPart = strings.Repeat("0", rateFracDigits)
	case !isDigits(fracPart):
		return 0, fmt.Errorf("rate %q is not a plain decimal number", s)
	case len(fracPart) > rateFracDigits:
		// More fractional digits than domain.RateScale can hold: the extra
		// digits are dropped rather than rounded, so a stored rate is never
		// larger than the value that was configured.
		fracPart = fracPart[:rateFracDigits]
	case len(fracPart) < rateFracDigits:
		fracPart += strings.Repeat("0", rateFracDigits-len(fracPart))
	}

	scaled, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("rate %q is out of range: %w", s, err)
	}
	if scaled <= 0 {
		return 0, domain.ErrNonPositiveRate
	}
	return scaled, nil
}

// parseSpreadBps parses a basis-point spread and rejects anything outside
// [0, maxSpreadBps), matching the fx_rates CHECK constraint.
func parseSpreadBps(s string) (int32, error) {
	if !isDigits(s) {
		return 0, fmt.Errorf("spread %q is not a plain whole number", s)
	}
	val, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("spread %q is out of range: %w", s, err)
	}
	if val >= maxSpreadBps {
		return 0, domain.ErrInvalidSpread
	}
	return int32(val), nil
}

// isDigits reports whether s is non-empty and every rune is an ASCII digit.
// It deliberately rejects "", a leading "+" or "-", and anything else that
// is not plain "0"-"9", so a caller never hands a sign or exponent marker
// into ParseInt expecting it to behave like a decimal number parser would.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

package domain

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"pgregory.net/rapid"
)

// Property: String renders and the sign and magnitude are recoverable, so the
// decimal rendering round-trips against a manual reconstruction.
func TestProp_MoneyStringRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		amt := rapid.Int64Range(math.MinInt64+1, math.MaxInt64).Draw(t, "amt")
		m, err := NewMoney(amt, "USD")
		if err != nil {
			t.Fatalf("NewMoney(%d): %v", amt, err)
		}
		// Reconstruct minor units from the rendered "X.YY USD" string.
		var whole, frac int64
		var cur string
		neg := m.String()[0] == '-'
		s := m.String()
		if _, err := fmtSscan(s, &whole, &frac, &cur); err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		got := whole*100 + frac
		if neg {
			got = -got
		}
		if got != amt {
			t.Fatalf("round trip: got %d want %d from %q", got, amt, s)
		}
	})
}

// Property: Add never silently overflows. For same-sign operands whose true sum
// exceeds int64 range, Add returns ErrOverflow; otherwise the result is exact.
func TestProp_MoneyAddNoSilentOverflow(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := rapid.Int64().Draw(t, "a")
		b := rapid.Int64().Draw(t, "b")
		ma, _ := NewMoney(a, "USD")
		mb, _ := NewMoney(b, "USD")
		sum, err := ma.Add(mb)
		// big.Int oracle for the true sum.
		over := (a > 0 && b > 0 && a > math.MaxInt64-b) ||
			(a < 0 && b < 0 && a < math.MinInt64-b)
		if over {
			if !errors.Is(err, ErrOverflow) {
				t.Fatalf("expected ErrOverflow for %d+%d, got %v", a, b, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected error for %d+%d: %v", a, b, err)
		}
		if sum.Amount() != a+b {
			t.Fatalf("sum: got %d want %d", sum.Amount(), a+b)
		}
	})
}

// Property: currency mismatch is always rejected regardless of amounts.
func TestProp_MoneyMismatchAlwaysRejected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := rapid.Int64Range(-1e15, 1e15).Draw(t, "a")
		b := rapid.Int64Range(-1e15, 1e15).Draw(t, "b")
		ma, _ := NewMoney(a, "USD")
		mb, _ := NewMoney(b, "EUR")
		if _, err := ma.Add(mb); !errors.Is(err, ErrCurrencyMismatch) {
			t.Fatalf("expected ErrCurrencyMismatch, got %v", err)
		}
	})
}

// fmtSscan parses "X.YY CUR" (X may carry a leading minus). Kept local to the
// test so the parsing intent is explicit.
func fmtSscan(s string, whole, frac *int64, cur *string) (int, error) {
	// Strip a leading minus; magnitude is what we reconstruct.
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	n, err := fmt.Sscanf(s, "%d.%02d %s", whole, frac, cur)
	_ = neg
	return n, err
}

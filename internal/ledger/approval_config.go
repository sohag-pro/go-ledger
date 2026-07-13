package ledger

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// ApprovalConfig is the env-resolved approval policy (ADR-025). Enabled off
// means the gate never fires and posting behaves exactly as before.
type ApprovalConfig struct {
	Enabled               bool
	Thresholds            map[string]int64 // currency to minor units
	RequireDifferentActor bool
	TTL                   time.Duration
}

// ParseApprovalThresholds parses "USD:100000,EUR:90000" into a currency to
// minor units map. Blank entries are skipped; an empty string is a valid
// empty map (no currency gated). It mirrors internal/fx/seed.go's
// comma/colon parse.
func ParseApprovalThresholds(raw string) (map[string]int64, error) {
	out := make(map[string]int64)
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		ccy, amtStr, ok := strings.Cut(field, ":")
		if !ok {
			return nil, fmt.Errorf("approval threshold %q: want CCY:amount", field)
		}
		amt, err := strconv.ParseInt(strings.TrimSpace(amtStr), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("approval threshold %q: %w", field, err)
		}
		out[strings.TrimSpace(ccy)] = amt
	}
	return out, nil
}

// Gate returns the currency with the largest overage over its configured
// threshold (the reason a transaction is held), or gated=false. It judges the
// single largest absolute leg per currency, matching the per-currency invariant.
func (c ApprovalConfig) Gate(postings []domain.Posting) (ccy string, amt int64, gated bool) {
	if !c.Enabled {
		return "", 0, false
	}
	maxLeg := make(map[string]int64)
	for _, p := range postings {
		cur := string(p.Amount.Currency())
		a := p.Amount.Amount()
		if a < 0 {
			a = -a
		}
		if a > maxLeg[cur] {
			maxLeg[cur] = a
		}
	}
	var bestCcy string
	var bestThreshold, bestOverage int64
	for cur, leg := range maxLeg {
		threshold, ok := c.Thresholds[cur]
		if !ok || leg <= threshold {
			continue
		}
		if over := leg - threshold; over > bestOverage {
			bestOverage, bestCcy, bestThreshold = over, cur, threshold
		}
	}
	if bestCcy == "" {
		return "", 0, false
	}
	return bestCcy, bestThreshold, true
}

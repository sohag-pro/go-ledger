package ledger

import (
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

func newUSDPosting(t *testing.T, amount int64) domain.Posting {
	t.Helper()
	m, err := domain.NewMoney(amount, "USD")
	if err != nil {
		t.Fatalf("NewMoney(USD): %v", err)
	}
	return domain.Posting{AccountID: "acct", Amount: m}
}

func newEURPosting(t *testing.T, amount int64) domain.Posting {
	t.Helper()
	m, err := domain.NewMoney(amount, "EUR")
	if err != nil {
		t.Fatalf("NewMoney(EUR): %v", err)
	}
	return domain.Posting{AccountID: "acct", Amount: m}
}

func TestParseApprovalThresholds(t *testing.T) {
	got, err := ParseApprovalThresholds(" USD:100000, EUR:90000 ")
	if err != nil {
		t.Fatal(err)
	}
	if got["USD"] != 100000 || got["EUR"] != 90000 {
		t.Fatalf("got %v", got)
	}
	if _, err := ParseApprovalThresholds(""); err != nil {
		t.Fatalf("empty is not an error: %v", err)
	}
	if _, err := ParseApprovalThresholds("USD:notint"); err == nil {
		t.Fatal("want parse error")
	}
}

func TestApprovalGate(t *testing.T) {
	cfg := ApprovalConfig{Enabled: true, Thresholds: map[string]int64{"USD": 100000}}
	usd := func(a int64) domain.Posting { return newUSDPosting(t, a) }
	eur := func(a int64) domain.Posting { return newEURPosting(t, a) }
	// under threshold: not gated
	if _, _, g := cfg.Gate([]domain.Posting{usd(600), usd(-600)}); g {
		t.Fatal("600 under 100000 must not gate")
	}
	// over threshold: gated on USD
	ccy, amt, g := cfg.Gate([]domain.Posting{usd(200000), usd(-200000)})
	if !g || ccy != "USD" || amt != 100000 {
		t.Fatalf("expected gate USD/100000, got %s/%d/%v", ccy, amt, g)
	}
	// disabled: never gates
	off := cfg
	off.Enabled = false
	if _, _, g := off.Gate([]domain.Posting{usd(200000), usd(-200000)}); g {
		t.Fatal("disabled config must not gate")
	}
	// currency without a threshold: not gated
	eurCfg := ApprovalConfig{Enabled: true, Thresholds: map[string]int64{"USD": 100000}}
	if _, _, g := eurCfg.Gate([]domain.Posting{newEURPosting(t, 500000), newEURPosting(t, -500000)}); g {
		t.Fatal("EUR has no threshold, must not gate")
	}
	// boundary (strict >): posting exactly equal to threshold must not gate
	boundaryConfig := ApprovalConfig{Enabled: true, Thresholds: map[string]int64{"USD": 100000}}
	if _, _, g := boundaryConfig.Gate([]domain.Posting{usd(100000), usd(-100000)}); g {
		t.Fatal("posting exactly equal to threshold (100000) must not gate")
	}
	// max-overage selection across currencies: largest overage wins
	multiCfg := ApprovalConfig{Enabled: true, Thresholds: map[string]int64{"USD": 100000, "EUR": 100000}}
	ccy, amt, g = multiCfg.Gate([]domain.Posting{usd(100500), eur(120000), usd(-100500), eur(-120000)})
	if !g || ccy != "EUR" || amt != 100000 {
		t.Fatalf("expected gate EUR/100000 (larger overage), got %s/%d/%v", ccy, amt, g)
	}
}

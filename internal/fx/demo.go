package fx

import "context"

// DemoRateDate is the date the demo prefill rates were captured (from
// open.er-api.com). They are a fixed snapshot, not updated in real time; they
// exist only so the demo's Exchange rates page is not empty and shows realistic
// numbers. The console surfaces this date in a note.
const DemoRateDate = "2026-07-12"

// demoMarkupBps is the 1 percent default markup prefilled for a demo tenant.
const demoMarkupBps int32 = 100

// demoRates are USD-based mid rates captured on DemoRateDate, stored as
// mid_rate_e8 (the decimal rate times 1e8). Only one direction per pair is
// stored; the provider derives the reverse (for example EUR to USD) itself.
var demoRates = []struct {
	Base, Quote string
	MidE8       int64
}{
	{"USD", "EUR", 87549400},    // 1 USD = 0.87549400 EUR
	{"USD", "MYR", 407046600},   // 1 USD = 4.07046600 MYR
	{"USD", "BDT", 12327355800}, // 1 USD = 123.27355800 BDT
	{"USD", "INR", 9554823100},  // 1 USD = 95.54823100 INR
}

// PrefillDemoRates writes the demo's starter FX config for one tenant: the
// USD-based rates above with no per-pair spread (so the markup default applies
// to all of them), plus a 1 percent tenant markup default. This is demo-only
// convenience, called by the demo seeder for the demo tenant and by
// create-tenant in demo mode for a newly created tenant.
//
// Idempotency is not required: the demo seeder clears the demo tenant's
// api-sourced rows before it re-prefills, and create-tenant runs once per
// tenant. All writes go through AdminService, so they are the same append-only,
// validated rows the admin API would write (source "api").
func PrefillDemoRates(ctx context.Context, svc *AdminService, tenantID string) error {
	for _, r := range demoRates {
		if _, err := svc.InsertRate(ctx, tenantID, r.Base, r.Quote, r.MidE8, nil); err != nil {
			return err
		}
	}
	bps := demoMarkupBps
	if _, err := svc.SetMarkup(ctx, tenantID, &bps); err != nil {
		return err
	}
	return nil
}

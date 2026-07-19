# ADR-022: USD-Hub FX Triangulation for Cross-Currency Pairs

This ADR reverses one narrow decision from ADR-014: that FX conversion never
routes through a third currency. It adds a single-hop triangulation through a
fixed hub currency (USD) so a conversion between two non-USD currencies works
when only each side's rate against USD is configured.

## Status

Accepted: 2026-07-12

## Context

ADR-014 built FX conversion around directly-stored rate pairs. The provider
resolves a rate for base to quote by looking up the stored pair, or inverting the
reverse pair, and it deliberately stops there: "this is deliberately not a
multi-hop search through a third currency; v1 stores and inverts exactly the
pairs the ledger needs."

That was the right call when an operator hand-configured every pair they needed.
It is the wrong call now that the demo (and the natural way an operator sets rates)
is USD-based: rates are configured as USD to EUR, USD to BDT, USD to MYR, and so
on. With only those, a conversion from EUR to BDT fails with "no rate for the
pair," even though the two rates needed to compute it are both present. The
operator sees a dead end for a conversion the system clearly has enough
information to price.

## Decision

### 1. When no direct or inverse pair exists, triangulate through USD

The provider gains a third fallback, tried only after the direct pair and the
inverse pair both miss. To resolve base to quote where neither is USD:

1. Find the base to USD rate (a stored base to USD row, or the inverse of a
   stored USD to base row). Call its mid "USD per base."
2. Find the USD to quote rate (a stored USD to quote row, or the inverse of a
   stored quote to USD row). Call its mid "quote per USD."
3. The cross mid is `USD_per_base * quote_per_USD / RateScale`, computed in
   big.Int and range-checked, so it stays exact integer arithmetic and cannot
   overflow.

If either leg is missing, the fallback declines and the conversion still returns
the same "no rate" error as before. So triangulation only ever adds successful
conversions; it never changes an existing one.

USD is a fixed hub for v1, not configurable. That matches how rates are actually
set here (USD-based) and keeps the fallback a single, well-understood hop rather
than a general shortest-path search across an arbitrary rate graph, which ADR-014
rightly did not want.

### 2. The markup is the base-to-USD leg's spread

A hub conversion applies one spread, resolved from the base to USD leg (the
"sell side" the customer is leaving). This is a deliberate, simple rule: the
customer converting EUR to BDT is charged the markup configured on EUR against
USD, not some composition of two spreads. Composing spreads would double-charge
the markup and is not what an operator setting "50 bps on EUR" expects. The
resolved spread still runs through the ADR-020 precedence chain (per-pair
override, then tenant default, then global default, then zero) on that base leg.

### 3. Provenance points at the base leg

`FXDetail` records a single `rate_id`. For a hub conversion it is the base to USD
row's id, and the source and effective time are that row's. There is no single
stored row for the cross pair, so the base leg is the honest anchor: it is the
row whose rate and spread most directly shaped the result. The snapshot still
records the concrete composed mid and the concrete spread applied, so the
conversion remains fully reproducible from `FXDetail` alone regardless of later
rate edits.

### 4. The console previews and labels the hub route

The convert page computes the same cross rate client-side for its live estimate,
and when a conversion is routed through USD it shows a short notification saying
so, so the operator understands why the rate looks like a composition rather than
a directly-configured pair.

## Consequences

- A conversion between any two currencies that each have a USD rate now works,
  with no extra configuration. EUR to BDT prices off USD to EUR and USD to BDT.
- Nothing changes for a pair that has its own direct or inverse rate: that path
  still wins, so an operator who wants a bespoke cross rate can still set one and
  it takes precedence over the hub.
- A hub conversion rounds twice: once when the two mids are composed into the
  cross mid, and once when `Convert` applies it to the amount. This is a
  deliberate, bounded imprecision for a convenience route; an operator who needs
  exactness on a specific cross pair sets that pair directly. The single-rounding
  guarantee still holds for every directly-configured pair.
- Reproducibility is unchanged: the composed mid and applied spread are
  snapshotted, so a recorded hub conversion always replays to the same numbers.

## Alternatives considered

- **Seed every cross pair.** Rejected: it pushes the composition onto whoever
  sets rates, multiplies the number of rows to keep current, and drifts out of
  sync the moment one USD leg changes. The hub composes from the current legs at
  conversion time, so it is always consistent with them.
- **A general multi-hop shortest-path search.** Rejected as over-built and exactly
  what ADR-014 warned against: it invites cycles, ambiguous routes, and arbitrage
  between paths. A single fixed USD hop is predictable and matches how rates are
  configured.
- **Composing both legs' spreads.** Rejected: it double-charges the markup and
  surprises an operator who set one number. One spread, from the base leg, is
  what "the markup on this currency" means.
- **A configurable hub currency.** Deferred: USD is the hub every demo and
  operator rate set uses today. A configurable hub is a small addition later if a
  non-USD-based deployment ever needs it; hardcoding USD now keeps the change
  minimal and the behavior obvious.

## Out of scope (v1)

- A configurable or multiple hub currency.
- Multi-hop routes longer than one intermediate currency.
- Choosing the cheaper of several possible routes (there is exactly one: USD).

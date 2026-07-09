# ADR-014: Multi-currency accounts and FX conversion

## Status

Accepted: 2026-07-09

Supersedes the v1 scope note (CLAUDE.md) that listed multi-currency and FX as out
of scope. The public roadmap was extended from 12 to 14 weeks, and Week 11 brings
multi-currency and FX into scope deliberately.

## Context

Until now the ledger is single-currency per transaction. `Money` carries a
`Currency` (ISO 4217), every `Account` has a currency, and a transaction stores
one currency shared by all its postings. Two database triggers enforce this:
`assert_txn_balanced` (all postings of a transaction sum to zero) and
`assert_posting_currency`, from ADR-005 (every posting's account currency must
equal the transaction's currency). So different accounts can hold different
currencies, but a single transaction cannot span them.

Week 11 adds cross-currency (FX) transactions: one transaction that moves value
between accounts of different currencies while double-entry still holds. A plain
USD to EUR transfer (debit a USD account, credit a EUR account) does not sum to
zero within either currency, so the model has to change.

The hard constraint this project has held since ADR-002: money is integer minor
units, never floating point, because a rounding ghost in the money path is
exactly the class of bug that destroys value silently. FX conversion multiplies
by a rate, which is where float usually creeps back in. It must not.

## Decision

### 1. Currency moves onto the posting; the invariant becomes per-currency

Every posting carries its own currency (denormalized from its account). The
double-entry invariant changes from "all postings sum to zero" to "for each
currency in the transaction, its postings sum to zero." A single-currency
transaction is the special case with one currency group.

- `Transaction.Validate()` groups postings by currency and checks each group nets
  to zero.
- `assert_txn_balanced` is rewritten to `GROUP BY currency`, each summing to zero.
- `assert_posting_currency` now checks `posting.currency = its account.currency`
  (it used to compare against the transaction's currency).
- The transaction-level `currency` column is dropped: a transaction can now span
  currencies, so a single transaction currency is no longer meaningful.

### 2. FX clearing accounts make each currency net to zero

A cross-currency move routes each currency leg through a system clearing account,
so every currency nets to zero on its own and the clearing accounts carry the
open FX position:

```
convert 100.00 USD to EUR, applied rate 0.9200 EUR per USD
  user_USD      -10000 USD        fx_clearing_USD  +10000 USD   (USD sum = 0)
  fx_clearing_EUR  -9200 EUR       user_EUR          +9200 EUR   (EUR sum = 0)
```

Because an account is single-currency but an FX position spans currencies, the
clearing is a set of per-tenant, per-currency system accounts (one per currency,
auto-created on first use). They use a distinct **system** account type: excluded
from user-facing balance listings, and expected to carry a permanent, often
negative, open position (that position, revalued at current rates, is the FX
exposure, and its drift is FX gain or loss). This is standard nostro-style FX
accounting, not a workaround. Clearing accounts are created lazily and
concurrency-safely: `INSERT ... ON CONFLICT DO NOTHING` against a `UNIQUE
(tenant_id, name)` constraint, so two concurrent first-time converts cannot create
duplicates.

### 3. The convert endpoint applies the rate server-side

`POST /v1/transactions/convert` (and the gRPC equivalent) takes
`{from_account, to_account, source_amount}` plus a mandatory idempotency key. The
target currency is derived from the `to` account, never from client input. Both
accounts must belong to the caller's tenant. The server looks up the rate,
applies the spread, computes the converted amount, and posts all four legs in one
atomic transaction. The client never supplies a rate: a client-controlled rate is
a money-theft vector.

Rejected at validation: same `from` and `to` account, a "conversion" where both
accounts share a currency, a missing rate pair, a non-positive rate, and a
conversion whose converted amount rounds to zero (dust: taking source and
crediting nothing is not a valid trade).

### 4. Rates live in an append-only table behind a provider seam

A new `fx_rates` table stores rates for reference and history:
`id, base, quote, mid_rate_e8 (BIGINT, mid scaled by 10^8), spread_bps (INT),
source, effective_at, created_at`. It is **append-only**: env seeding and any
future provider insert a new row with a new `effective_at`; nothing is updated in
place. "The current rate" for a pair is the row with the greatest
`(effective_at, id) <= now`, ordered `effective_at DESC, id DESC` so two rows
sharing an `effective_at` (a re-seed within the same second) resolve
deterministically to the last inserted. This preserves the history the DB store
exists to give: what rate was live at any past time is reproducible.

A `RateProvider` interface decouples rate origin from the conversion logic:

```go
type RateProvider interface {
    // Rate returns the current MID rate (quote per base, in the FXQuote, scaled by
    // 1e8) and the configured spread in basis points for that pair.
    Rate(ctx context.Context, base, quote domain.Currency) (domain.FXQuote, int32, error)
}
```

v1 is a `dbRateProvider` that reads the current row. `FX_RATES` in the environment
seeds the table at boot (inserting new effective rows). A future `apiRateProvider`
pulls mid rates from an external provider and inserts into the same table: one
interface, no change at the call sites.

The spread is the **ledger's own policy** (its markup), not the provider's data.
A real rate API returns a mid; the markup is a business decision. For v1 the
spread is stored alongside the mid in `fx_rates` (seeded from env), but the
conversion service, not the provider, is what applies it.

### 5. Spread is applied so the customer always gets the worse side

The spread is a bid/ask widening around the mid, and it must disadvantage the
customer in **both** directions, including when a pair is only stored one way and
the reverse is derived. The rule, order-of-operations exactly:

1. Normalize to quote-per-base: if the `base/quote` pair is stored, use its mid
   directly; if only `quote/base` is stored, invert it (`mid_qpb = 10^16 /
   mid_bpq`, integer) to get quote-per-base.
2. Apply the spread as a reduction of the quote the customer receives:
   `applied_e8 = mid_qpb_e8 * (10000 - spread_bps) / 10000`.

Reducing quote-out is always worse for the customer regardless of direction, so a
round trip (A to B to A) loses roughly two spreads plus rounding, and that loss is
exactly what accumulates in the clearing accounts.

One provenance subtlety for inverted lookups: the FX snapshot's `mid_rate_e8`
records the *inverted* mid actually applied, while its `fx_rate_id` points at the
reverse-direction `fx_rates` row, whose stored `mid_rate_e8` is the *non-inverted*
value. The conversion stays fully reproducible from the snapshot alone
(mid + spread + source through the decision-6 formula); the `fx_rate_id` link is
provenance to the source row, not the applied number. An auditor joining on
`fx_rate_id` should expect the stored row's mid to be the reciprocal of the applied
mid for an inverted pair. Both directions are unit
tested, and the round-trip reconciliation test asserts the clearing position
equals the expected spread-plus-residual.

### 6. Conversion is integer-only, single-step, banker's rounded, sign-symmetric

The rate and the amount are combined in a **single rounding step**, not
rate-round then amount-round, so no double-rounding bias creeps in:

```
converted_minor = bankers_round( source_minor * mid_e8 * (10000 - spread_bps)
                                / (10^8 * 10000) )
```

The reproducible truth of a conversion is `(mid_e8, spread_bps, source)` run
through this formula. `applied_e8` is stored too, but as an informational display
value (`bankers_round(mid_e8 * (10000 - spread_bps) / 10000)`), not the thing the
converted amount is derived from.

The intermediate `source_minor * mid_e8 * (10000 - spread_bps)` can reach ~10^30
and overflow int64, and even `mid_e8 * (10000 - spread_bps)` overflows for a
large-magnitude rate, so the whole computation is done in **`math/big.Int`**. This
also sidesteps the `math.MinInt64` negation trap that a `uint64(-a)` 128-bit path
would hit. `math/bits.Div64` is deliberately not used: its `hi < divisor`
precondition is easy to violate and get an arithmetically wrong quotient. There is
a database round trip on either side of this call, so the allocation cost of
`big.Int` is irrelevant. No `float64` anywhere in the money path, including rate
parsing (env rates are scaled to integers by string manipulation, never
`ParseFloat`).

Rounding is round-half-to-even (banker's), implemented on the integer quotient and
remainder and **symmetric across sign** (postings are frequently negative):
`-2.5 to -2`, `-3.5 to -4`, `2.5 to 2`, `3.5 to 4`. The tie boundaries, both signs,
the `MinInt64` source, and a result that overflows int64 (rejected, not wrapped)
are all unit tested. Banker's rounding is chosen over half-up because half-up
carries a systematic upward bias that accumulates directionally over many
conversions. A bad spread (outside `[0, 10000)` bps) is `ErrInvalidSpread`, a
non-positive rate `ErrNonPositiveRate`, and a conversion that rounds to zero
`ErrConversionDust`.

### 7. The applied rate is recorded immutably

Each FX transaction snapshots, immutably, the numbers actually used:
`source_amount, converted_amount, mid_rate_e8, spread_bps, applied_e8, source,
effective_at`, plus a foreign key to the `fx_rates` row for provenance. The
snapshot, not a later join to a mutable table, is the truth for a dispute: what
rate we applied, from where, at what time. The audit log (ADR-007) records the
conversion with the same rate detail.

### 8. New-account default currency is env-configured

Accounts already carry a non-null currency, so the migration backfills the new
`postings.currency` column from each posting's own account, not from a global
default. What `DEFAULT_CURRENCY` (env, default `USD`) governs is the default
currency for a newly created account when the caller does not specify one, and it
is what the demo seeder stamps on its seeded accounts. Operators set it to
whatever the deployment's data should default to.

The migration is ordered so the balanced trigger never sees a null or mixed
grouping: add `postings.currency` nullable, backfill from each posting's account,
set `NOT NULL`, then swap `assert_txn_balanced` to the per-currency version.

### 9. The idempotency fingerprint changes, and that is a breaking change

Dropping the transaction currency means the idempotency request fingerprint
(ADR-006), which hashed the transaction currency plus every posting, moves to
hashing each posting's currency. This is a breaking change: a client retrying a
request created *before* this deploy would produce a different fingerprint and be
rejected as "same key, different body" (409).

We accept this now without a migration. The service is still being built, no real
money flows through it, and the demo tenant is wiped every four hours, so there
are no durable in-flight keys to protect. A fingerprint-scheme version id (store
the scheme with the key, accept old-scheme keys under their old hash) is the
correct fix once real traffic exists; it is deferred to that point, recorded here
so the debt is explicit.

## Consequences

- The ledger models real cross-currency movement with double-entry intact per
  currency, and the FX position and its gain or loss live in auditable clearing
  accounts.
- Every aggregate is now per-currency; nothing may sum across currencies. A
  tenant's "total" is a vector of per-currency balances, not a scalar.
- The money path stays integer-only; the FX rate never introduces float, and the
  rounding residual is accounted for (it lands in clearing) rather than lost.
- Rate history is queryable and reproducible; provenance is on every FX
  transaction; the origin of rates is swappable without touching conversion code.
- The clearing accounts are write-hot per tenant, but same-tenant posts already
  serialize (ADR-012), so this adds no new contention class.
- The idempotency fingerprint change is breaking across this one deploy, accepted
  deliberately (see decision 9).
- Dropping `transactions.currency` ripples to every reader of it: the transaction
  sqlc queries, the REST and gRPC transaction response shapes (which move from one
  transaction currency to a currency per posting), the audit payload, the demo
  seeder, and the console. All are updated in the same change; none may still
  reference a transaction-level currency afterward.

## Alternatives considered

- **Rate-annotated two-leg transaction (no clearing account):** store the rate and
  validate the two legs as balanced *by the rate* instead of per-currency
  zero-sum. Rejected: it weakens the invariant from "sums to zero" to "balanced
  under the recorded rate," an FX-specific rule that is harder to enforce in the
  database and easier to get wrong. The clearing model keeps the strong invariant
  literally true.
- **Applying the rate in float64:** simplest to write. Rejected outright: it
  reintroduces floating point into the money path, the exact thing ADR-002
  removed.
- **Round-half-up:** simpler than banker's. Rejected: systematic directional bias.
- **Upserting rates in place:** simpler table. Rejected: destroys the rate history
  the DB store is meant to provide.
- **Keeping `transaction.currency` nullable** (null for FX txns): lower blast
  radius. Rejected in favor of dropping it for a single clean representation,
  accepting the fingerprint change (decision 9).
- **Client-supplied rate or client-built FX legs:** more flexible. Rejected: a
  client-controlled rate is a theft vector; the server owns the rate.

## Out of scope (v1)

Live rate-provider integration (the seam is built, the implementation is not),
FX gain-or-loss *reporting* (the position sits in clearing accounts; reporting is
Week 12), currency triangulation beyond direct and single-inverse lookup,
per-rate historical revaluation, bid/ask sourced from a real market (v1 is a
single configured spread around a mid), the idempotency fingerprint-scheme
versioning (decision 9), a rate max-age / staleness check (v1 env rates do not
expire; the current-rate query has no freshness bound), a database-level guard
making the FX snapshot columns immutable (v1 relies on the append-only, never-
updated convention rather than an enforced trigger), and surfacing the FX rate
detail on the transaction read path (`GET /v1/transactions/{id}`): the applied rate
is returned on the convert response and recorded in the audit log, but the
read-back of an existing transaction reports per-posting currency only, not the
`fx_*` snapshot.

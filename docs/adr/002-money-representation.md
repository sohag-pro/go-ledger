# ADR-002: Money Representation

## Status

Accepted: 2026-06-15

## Context

The ledger moves money between accounts and must answer balance queries
exactly. Floating point is unfit for money: 0.1 + 0.2 != 0.3 in IEEE-754, and
rounding error accumulates across millions of postings. We need an exact,
fast representation that maps cleanly to Postgres in Week 3.

Three candidates:

- **int64 minor units**: store the amount as a signed integer count of the
  currency's smallest unit (cents for USD). Exact, no allocation, maps to
  BIGINT. Must guard against overflow on arithmetic.
- **Arbitrary-precision decimal** (for example shopspring/decimal): exact and
  overflow-free, but every operation allocates a big.Int, and it maps to
  NUMERIC. More machinery than a single-currency v1 needs.
- **Custom decimal struct** (int64 coefficient + scale): full control, but we
  own the arithmetic, rounding, and a pile of tests for no v1 benefit.

## Decision

go-ledger represents money as `int64` minor units in a `Money` value type that
also carries its `Currency`. v1 is single-currency (USD), per the build plan's
scope discipline, so there is no per-currency scaling to track yet.

- `Money` is an immutable value type. Operations return new `Money` values.
- Arithmetic (`Add`, `Sub`, `Neg`) is overflow-guarded and returns an error on
  overflow rather than panicking or wrapping.
- Cross-currency arithmetic returns `ErrCurrencyMismatch`. The check exists now
  so multi-currency (a future, out-of-scope item) cannot silently corrupt sums.
- Maps to `BIGINT` in Postgres (Week 3) with no conversion.

## Consequences

### Positive

- Exact arithmetic with zero allocation on the hot path.
- Clean, lossless mapping to BIGINT; no NUMERIC parsing.
- Overflow is a typed, testable error, not undefined behavior.

### Negative

- int64 caps the representable amount near 9.2e16 minor units. Far beyond any
  realistic single-account balance, but arithmetic must still check, hence the
  overflow guard.
- Adding a second currency later means introducing per-currency exponents.
  Acceptable: multi-currency is explicitly out of scope for v1.

## Alternatives considered

- **float64**: rejected. Not exact; unacceptable for money.
- **shopspring/decimal**: rejected for v1. Allocation and NUMERIC overhead with
  no benefit while we are single-currency. Revisit if multi-currency lands.
- **Custom decimal struct**: rejected. Owning rounding and arithmetic is cost
  without v1 value.

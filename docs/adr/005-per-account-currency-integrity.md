# ADR-005: Per-Account Currency Integrity

## Status

Accepted: 2026-06-29
Superseded in part by ADR-014. This ADR's premise is that currency lives on the
transaction (ADR-003) and never on the posting, and it explicitly rejects
"denormalize currency onto postings" in Alternatives. Migration 0010 shipped
exactly that rejected alternative. The invariant this ADR introduced survives
in a rewritten form: `assert_posting_currency` now compares each posting's own
currency against its account's, rather than against the transaction's.

## Context

Currency lives in two places (ADR-003): every account has a currency, and every
transaction has a currency (the single currency shared by all its postings).
Postings themselves carry only a signed `amount`, not a currency, because the
domain already guarantees all postings of a transaction share one currency.

That leaves a gap. Nothing tied a transaction's currency to the currency of each
account it posts into. A USD transaction could post into an account whose currency
is EUR, and the database would accept it. Today the ledger is single-currency
(USD), so the gap is invisible, but it is exactly the kind of latent integrity
hole that turns into a real one the moment multi-currency, a second writer, or a
buggy import appears. A reviewer would not trust a ledger that lets a posting land
in an account of a different currency.

The domain cannot close this on its own: `Transaction.Validate` sees the postings
and their shared currency, but it does not know each referenced account's
currency, which lives in the database.

## Decision

Enforce it in the database. Migration 0003 adds an immediate `AFTER INSERT`
trigger on `postings` (`assert_posting_currency`) that, for each inserted posting,
looks up its account's currency and its transaction's currency and rejects the row
if they differ. *(Superseded by ADR-014: with currency now on the posting itself,
the trigger compares `posting.currency` against its account's currency. The rule
is the same; the source of the posting's currency changed.)*

- It is immediate, not deferred: the account and transaction both already exist
  when a posting is inserted (the foreign keys require it), so there is nothing to
  wait for.
- The lookups are equality on primary keys, so under SERIALIZABLE they take only
  narrow predicate locks on rows that posting transactions read but never write.
  The stress test confirmed this adds no measurable serialization contention.

This is defense in depth, consistent with how the balance invariant is handled
(ADR-004): the domain keeps postings in one currency, and the database guarantees
that currency matches each account, even against a direct write.

## Consequences

### Positive

- A posting can never move money in a currency the account does not hold. The
  guarantee holds regardless of what client or code path wrote the row.
- The schema is ready for multi-currency: when it lands, this invariant is already
  enforced rather than retrofitted onto live data.

### Negative

- A small per-posting read cost at insert time (two primary-key lookups). Negligible
  in practice and confirmed not to add serialization conflicts.
- The currency rule now lives in two places: the domain (postings share one
  currency) and the database (that currency matches the account). That redundancy
  is the point, but it is two places to keep in mind.

## Alternatives considered

- **Denormalize currency onto postings and use a composite foreign key**
  `postings(account_id, currency) -> accounts(id, currency)`: rejected. It would
  put currency back on the posting row, undoing the ADR-003 decision to keep a
  posting to a single signed amount, for no extra safety beyond what the trigger
  gives.

  *This rejection did not hold. Migration 0010 (ADR-014) put currency on the
  posting exactly as described here. The reasoning above was sound given its
  premise, which was that a transaction has one currency; multi-currency broke
  that premise. An FX transaction spans currencies, so there is no
  transaction-level currency left to compare a posting against, and the
  posting has to carry its own. Recorded here rather than quietly deleted,
  because a rejected alternative that later shipped is the most useful kind of
  entry in an ADR.*
- **Enforce only in application code** (check each account's currency in
  `CreateTransaction`): rejected. That is the very thing we want the database to
  guarantee; an application-only check is one forgotten code path away from a
  cross-currency posting.
- **Do nothing until multi-currency arrives**: rejected. Adding the constraint to
  a populated, multi-currency ledger later is far riskier than enforcing it now
  while the rule is trivially true.

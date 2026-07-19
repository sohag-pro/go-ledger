# ADR-023: Account Hierarchy and Rolled-Up Reporting

This ADR records adding a parent/child account hierarchy and rolled-up balances
to go-ledger (Week 12: account hierarchy and reporting). It keeps the core
invariant intact: balances stay derived from postings, never stored, and a
rollup is a query, not a maintained number.

## Status

Accepted: 2026-07-12

## Context

An account is flat today: identity plus classification (name, type, currency,
status), with its balance derived by summing its postings. Real charts of
accounts are trees. "Assets" contains "Cash" and "Bank"; "Bank" contains
"Checking" and "Savings". An operator wants to read the balance of "Bank" and
get the total across everything underneath it, not just its own direct postings.

The ledger is multi-currency with a per-currency zero-sum invariant, and its
first principle (ADR-001, ADR-003) is that a balance is never a stored, mutable
number: it is the sum of an append-only posting history. Any hierarchy feature
has to respect both.

## Decision

### 1. A self-referential parent_id, same-tenant and same-currency

`accounts` gains a nullable `parent_id uuid`. Same-tenant parentage is enforced
at the database level by a composite foreign key `(tenant_id, parent_id)
REFERENCES accounts (tenant_id, id)`, reusing the `UNIQUE (tenant_id, id)` the
table already has for postings. A cross-tenant parent simply cannot be
inserted, the same guarantee postings already rely on.

A child must share its parent's currency. A subtree is therefore single-currency,
which is what makes a rolled-up balance a single number rather than a map of
currency to amount. This matches the per-currency invariant: you cannot
meaningfully sum USD and EUR, so a tree that rolls up to one figure must be one
currency. Mixed-currency grouping is deliberately out of scope (see
alternatives).

### 2. Cycle prevention by a trigger

A `BEFORE INSERT OR UPDATE OF parent_id` trigger on `accounts` rejects three
things: a self-parent (`parent_id = id`), a cycle (walking the ancestor chain
from the proposed parent and finding the row itself), and a currency mismatch
against the parent. The trigger is the guarantee; the service also checks and
returns a clean error, but the database is the backstop that no code path can
bypass. The ancestor walk is itself bounded: it bails out past 10,000 hops
(migration 0032), so a chain corrupted by some future bug cannot spin the walk
forever. That hop ceiling is a backstop against a broken chain, not a product
limit on how deep a chart of accounts may be; no realistic hierarchy approaches
it.

### 3. Rollup is a recursive CTE at query time, not a stored column

A parent's rolled-up balance is computed on demand: a `WITH RECURSIVE` query
gathers the account and all of its descendants through `parent_id`, then sums
the postings of that whole set. Nothing is denormalized.

This is the central choice. The alternatives, a materialized rollup column
updated on every post and re-parent, or a closure table maintained on every
parent change, are both faster to read and both wrong for this codebase. A
stored rollup reintroduces exactly the mutable-balance state the whole ledger
was built to avoid, and it drifts the moment a post or a re-parent misses an
update. The recursive CTE keeps the balance derived and always correct, at a
query cost that is negligible at this scale (indexed reads over a tenant's
postings, the same shape as an ordinary balance read).

### 4. A parent can also carry its own postings

There is no rule that a parent must be an empty grouping node. A parent's
rolled-up balance is its own postings plus every descendant's. This keeps the
posting rules unchanged (any account can be posted to) and the rollup definition
simple (sum the whole subtree, root included). An operator who wants a pure
grouping node just does not post to it.

### 5. Rollups are additive display; the zero-proof stays on own balances

The trial-balance report gains a rolled-up balance per account alongside each
account's own balance. But the per-currency net that proves the books balance is
still computed from own (leaf-level) balances, because rollups double-count: a
parent's rollup already includes its children, so summing every account's rollup
would not be zero. The invariant proof is unchanged; the rollup is an extra,
convenience figure for reading the tree.

## API surface

- `POST /v1/accounts` accepts an optional `parent_id`.
- `POST /v1/accounts/{id}/parent` sets, changes, or clears an account's parent
  (re-parenting), cycle- and currency-guarded like creation.
- `GET /v1/accounts/{id}/balance?rollup=true` returns the rolled-up balance
  (own plus descendants); without the flag it returns the account's own balance
  exactly as before.
- `GET /v1/accounts/tree` returns the tenant's accounts as hierarchy rows
  (`parent_id`, depth, own balance, rolled-up balance), so a client can render
  the chart of accounts without walking it itself.
- `GET /v1/accounts` now returns `parent_id`.
- The trial-balance report account rows carry `rolled_up_balance`.

## Consequences

- An operator reads a parent balance and gets the whole subtree, with no stored
  state to drift and no maintenance on posting or re-parenting.
- The core invariant is untouched: balances remain derived, and the zero-proof
  still holds on own balances.
- A re-parent is a single `parent_id` update, guarded against cycles and
  currency mismatch by the trigger; rollups reflect it immediately because they
  are recomputed on read.
- Deep trees pay a recursive-CTE cost on a rollup read. At demo and
  single-operator scale this is negligible; a future high-volume deployment that
  needed constant-time rollups could add a closure table behind the same repo
  method without changing the API.

## Alternatives considered

- **Materialized rollup column.** Rejected: reintroduces stored mutable balance
  (against ADR-001/003) and drifts on any missed update.
- **Closure table.** Rejected for v1 as more moving parts than the scale needs;
  noted as the escape hatch if constant-time rollups ever matter.
- **Mixed-currency parents with per-currency rollups.** Rejected: a rollup would
  become a map of currency to amount, complicating every reader and the console,
  for a case the single-currency-subtree rule handles more simply.
- **Parents as pure grouping nodes (no direct postings).** Rejected: it adds a
  posting-time rule and a re-parenting rule for no real gain; an empty parent is
  just a parent nobody posts to.

## Out of scope (this week)

- Time-series analytics (transaction volume, balance-over-time). A follow-up.
- Mixed-currency grouping and multi-currency rollups.
- A closure table or any denormalized rollup.

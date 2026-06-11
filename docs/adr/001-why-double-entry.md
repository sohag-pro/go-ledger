# ADR-001: Why Double-Entry Accounting

## Status

Accepted: 2026-06-11

## Context

go-ledger is a payment ledger service: it must record money movements between
accounts and answer "what is the balance of account X?" correctly, every time,
under concurrent writes.

The naive approach is a single `balances` table with one row per account that
gets incremented and decremented in place. This is how many systems start, and
it fails in predictable ways:

- **No audit trail.** A mutable balance tells you *what* the balance is, not
  *how* it got there. Reconstructing history requires separate logging that
  inevitably drifts from the source of truth.
- **Money appears and disappears.** A single-sided update ("add $10 to A") has
  no structural guarantee that the money came from anywhere. Bugs create or
  destroy money silently.
- **Race conditions corrupt state.** Read-modify-write on a balance row under
  concurrency loses updates unless every code path remembers to lock
  correctly. The schema itself offers no protection.

Double-entry bookkeeping, in use since the 15th century, solves all three
structurally:

- Every transaction consists of two or more **postings** (ledger entries).
- Each posting debits one account and credits another.
- The sum of all postings in a transaction must equal zero.

## Decision

go-ledger models all money movement as double-entry transactions:

- `Transaction`: an atomic, immutable unit of money movement.
- `Posting`: a single signed entry against one account; a transaction has
  two or more postings.
- **Invariant:** Σ(postings) = 0 for every transaction. Enforced in the domain
  types (`Transaction.Validate()`), and later at the database level with a
  CHECK constraint.
- Postings are append-only. Balances are *derived* (sum of postings), never
  stored as mutable primary state.

## Consequences

### Positive

- Money can never be created or destroyed by a write path bug; an unbalanced
  transaction is rejected before it persists.
- The ledger is its own audit trail: every balance is explainable as a sum of
  immutable postings.
- Append-only postings sidestep update races entirely; concurrency control
  reduces to serializing transaction inserts (Week 4).
- The model matches what accountants, auditors, and payment partners expect.

### Negative

- Balance reads are aggregations, not single-row lookups. Acceptable for v1;
  if it becomes hot, a derived (and rebuildable) balance cache can be added;
  it stays a cache, never the source of truth.
- Slightly more conceptual overhead for API consumers: posting a payment means
  submitting balanced postings, not "set balance".

## Alternatives considered

- **Mutable balance table**: rejected. No audit trail, no structural
  integrity guarantee (see Context).
- **Event sourcing with a full event store**: rejected for v1. Double-entry
  postings already give us the append-only history we need without the
  operational complexity of projections and replay. Revisit only if product
  needs demand it (see scope discipline in the build plan).

# ADR-008: Covering Index for Account History Reads

## Status

Accepted: 2026-07-02

## Context

ADR-006 documents that the account statement is O(history) per page: because
balances are derived, never stored (ADR-003), the running-balance window scans
the account's full posting history within a single index range on
`(tenant_id, account_id)`, the two-column `postings_tenant_account_idx` from
`0001_initial.sql`. ADR-007 added a second reader on the same shape: `GET
/v1/accounts/{id}/audit` keyset-pages `(created_at, id)` through a join from
`audit_log` to the account's postings.

Both queries filter on `(tenant_id, account_id)` and then need their rows
ordered by `(created_at, id)`: the statement to return newest-first pages with
a correct running balance, the audit-by-account read to keyset-page in the
same order. The two-column index covers the filter but not the order, so
Postgres has to sort every page's matched rows after the index range scan. On
a busy account this sort runs on every single page request, competing for the
one CPU core the production VPS has.

A coverage/capacity audit flagged this as the read-side gap most likely to
show up first as the demo tenant's ~285 seeded transactions grow, and it is
cheap to close without touching query text or the derived-balance model.

## Decision

Migration `0007` replaces `postings_tenant_account_idx (tenant_id,
account_id)` with `postings_tenant_account_created_idx (tenant_id,
account_id, created_at, id)`.

The composite's leading two columns, `(tenant_id, account_id)`, still serve
the balance `SUM` and any other equality-only lookup, exactly as the old
index did. The trailing `(created_at, id)` means an index range scan on the
tenant/account prefix already returns rows in the exact order the statement
and audit-by-account queries need, so Postgres can walk the index directly
into a page of results instead of scanning then sorting. No sqlc query text
changes: the SQL was already selecting and ordering by these columns, it was
just the index that did not match.

The old two-column index is dropped in the same migration. Every query that
could use it can use the composite's leading prefix instead, so keeping both
would only add write-side index maintenance for no read benefit.

This does not change what is derived versus stored. Balances are still
computed by summing postings at read time; the index removes the per-page
sort, it does not cache or materialize a balance. The O(history) scan cost
ADR-006 already accepted is unchanged: this is a narrower fix for the sort
that sat on top of it.

## Consequences

### Positive

- Statement and audit-by-account pages no longer sort after the index scan;
  the index range scan already returns rows in the order the query needs.
- No application or query-text change: `internal/postgres` and its sqlc
  queries are untouched, only the schema.
- One index does the work of two: the composite serves every read the old
  two-column index served, plus the ordered scans, at one index's write cost
  instead of two.

### Negative

- The composite index is wider than the one it replaces (two extra columns
  per entry), so it costs slightly more to maintain per insert. On the
  production box's single core and 1GB of memory this is a small but real
  addition to write-side work; accepted, since posting throughput is not the
  bottleneck the audit flagged.
- Still O(history) per page in the sense that the scanned range grows with an
  account's full posting count, even though it is no longer sorted. A
  genuinely unbounded account still needs the materialized-rollup escape
  hatch ADR-006 already named, not another index.

## Alternatives considered

- **Keep both indexes**: rejected. The two-column index becomes fully
  redundant once the composite exists (same leading prefix), so this would
  just pay index maintenance twice for the same set of queries.
- **Materialized running balance, rebuilt from postings**: deferred. This is
  the actual escape hatch ADR-003 and ADR-006 already named for when
  O(history) itself becomes the problem, not just the sort on top of it. Out
  of scope for this change: it would introduce a second, cached number that
  has to reconcile with the postings, which is the exact drift ADR-003
  rejected a mutable balance column to avoid. Revisit only if index-scan cost
  itself, not the sort, becomes the bottleneck.
- **Do nothing**: rejected. Leaves an unbounded per-page sort competing for
  the single core on every statement and audit-by-account request, and the
  fix is a single, low-risk, additive migration.

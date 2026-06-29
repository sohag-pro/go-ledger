# ADR-004: Concurrency Control for Transaction Posting

## Status

Accepted: 2026-06-29

## Context

Posting a transaction writes a transaction row and two or more postings that must
sum to zero, and it must do so correctly when many requests post to the same
accounts at once. Two requests racing on the same account cannot be allowed to
produce a ledger that does not balance, lose a posting, or read a stale balance.

The schema (ADR-003) already makes postings append-only and balances derived, so
there is no mutable balance row to corrupt. What remains is making each posting
atomic and keeping the double-entry invariant intact under concurrency.

There are two classic strategies:

- **Pessimistic: lock the rows.** Run at READ COMMITTED and `SELECT ... FOR
  UPDATE` the involved account rows, in a fixed order to avoid deadlock, before
  writing. Conflicting transactions block rather than fail, so there is nothing
  to retry. Correctness rides on remembering to lock the right rows, in the right
  order, every time.
- **Optimistic: let the database detect conflicts.** Run at SERIALIZABLE.
  Postgres's Serializable Snapshot Isolation (SSI) tracks read/write dependencies
  and aborts a transaction with SQLSTATE 40001 if committing it would break
  serial-equivalent ordering. No explicit locks; conflicts surface as errors the
  application retries.

## Decision

go-ledger uses **SERIALIZABLE with automatic retry**. Correctness comes from the
database guaranteeing serial-equivalent execution, not from the application
remembering to take the right locks. A buggy or future code path cannot silently
skip a lock it never knew to take.

The pieces:

- **Service owns the unit of work.** `ledger.TransactionService.Post` validates
  the transaction, then runs the write through the repository's `RunInTx`, which
  begins a SERIALIZABLE transaction, runs the work, and commits.
- **The adapter owns isolation and retry.** `RunInTx` (in `internal/postgres`)
  begins each attempt at `pgx.Serializable`. A serialization failure (40001) or
  deadlock (40P01) can surface either inside the work or, commonly, only at
  COMMIT, so both are watched and the whole unit of work is replayed. Retries are
  bounded (25 attempts) with exponential backoff and full jitter; the jitter is
  what stops a crowd of conflicting transactions from retrying in lockstep and
  colliding again.
- **The invariant is also enforced in the database.** A `DEFERRABLE INITIALLY
  DEFERRED` constraint trigger (`assert_txn_balanced`, migration 0002) runs at
  COMMIT and rejects any transaction whose postings do not sum to zero. This is
  defense in depth: the domain checks the invariant first (`Transaction.Validate`)
  for a fast, clean error, but the trigger guarantees it holds even against a bad
  migration, a direct psql write, or a second service.
- **The trigger's read is indexed.** The trigger reads postings by
  `transaction_id`. Without an index that read is a scan, and under SSI a scan
  takes broad predicate locks that turn into a storm of false-positive
  serialization conflicts. Adding `postings_transaction_idx` was the single change
  that took the stress test from roughly 1 in 6 posts failing to zero. This is the
  non-obvious lesson of the week: under SERIALIZABLE, an unindexed read is not
  just slow, it manufactures conflicts.

`SELECT FOR UPDATE` is not used. The two approaches were not combined: belt and
suspenders here would mean taking explicit locks and running SERIALIZABLE and
retrying, which is more moving parts and redundant guarantees for no gain.

## Observability

Posting is instrumented with two Prometheus collectors: a histogram
`transaction_post_duration_seconds`, labeled by outcome, for latency, and a
counter `transaction_post_serialization_retries_total` so a climbing retry rate
makes write contention visible. These sit alongside the standard Go runtime and
process collectors.

The scrape endpoint is served on a separate server bound to loopback
(`127.0.0.1:9090` by default), not on the public API port. A metrics endpoint
leaks transaction volumes, latencies, and retry rates, so it is reached over the
host's private network or an SSH tunnel, never the public interface. The public
liveness check stays `GET /healthz`.

## Measured baselines

Stress test: 100 goroutines posting 10,000 balanced transactions against a shared
pool of 100 accounts. Result: all 10,000 commit, zero failures, and the sum of
every account balance is exactly zero.

One honest caveat about the concurrency level: the real ceiling on how many
posting transactions run at the database at once is the connection pool's
`MaxConns`, not the goroutine count. The test sets `MaxConns` to 25 explicitly and
logs it, so "100 goroutines" means up to 25 transactions truly concurrent at
Postgres with the rest queued on pool acquisition. That is still meaningful
contention on 100 shared accounts, and it is the number worth quoting rather than
the goroutine count.

Latency on the local test machine (Postgres 16 in a colima VM): p50 about 35 ms,
p99 about 68 ms. These are above the original p50 < 10 ms / p99 < 50 ms target.
The dominant cost is WAL fsync on the VM's virtualized disk: every commit waits
for a durable write, and a Lima VM's disk is slower than bare metal. The numbers
are reported as measured rather than tuned (for example by disabling
`synchronous_commit`), since durability is the point of a ledger. On bare-metal or
a real server disk the same code should land much closer to the target. Latency is
logged by the test, not asserted, so the suite does not flake on machine speed.

## Consequences

### Positive

- Correctness does not depend on the application taking the right locks; the
  database guarantees serial-equivalent execution.
- The double-entry invariant is enforced in two independent places: the domain
  and a deferred database trigger.
- Retry with jittered backoff absorbs contention transparently; under the test's
  sustained load every transaction eventually commits.
- `RunInTx` is a general unit-of-work seam, so Week 6 can write the audit-log
  entry inside the same transaction as the posting it records.

### Negative

- Under contention some transactions do extra work (retries), and pathological
  contention could still exhaust the retry budget and surface an error to the
  caller. That is the optimistic trade: conflicts cost retries, not blocked
  connections.
- SSI is sensitive to read patterns. Indexes that look optional for correctness
  (like the one on `transaction_id`) are load-bearing for concurrency. Future
  reads inside a posting transaction must be considered for their SSI footprint.
- `fn` passed to `RunInTx` must be safe to run more than once, since it can be
  replayed.

## Alternatives considered

- **READ COMMITTED + SELECT FOR UPDATE**: rejected. Pushes correctness onto the
  application to lock the right rows in the right order every time; a future code
  path that forgets reintroduces the race. Pessimistic locking also serializes
  access to hot accounts by blocking, rather than letting non-conflicting work
  proceed.
- **SERIALIZABLE + SELECT FOR UPDATE + retry**: rejected. Redundant guarantees and
  more moving parts than SERIALIZABLE alone needs.
- **Mutable balance with row locks**: rejected back in ADR-003; it reintroduces a
  second copy of the truth that can drift.
- **A non-deferred row CHECK**: impossible; the sum-to-zero invariant spans many
  posting rows and cannot be expressed in a single-row CHECK, hence the deferred
  constraint trigger.

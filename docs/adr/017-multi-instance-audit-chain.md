# ADR-017: Multi-instance audit chain (transactional outbox + single chainer)

Status: Accepted
Date: 2026-07-10
Referenced by ADR-015 (audit remediation, Phase 3). Closes audit finding A3.6
(Blocker, single-instance correctness cliff), with A5.1 (audit chain on the hot
path), A8.2 (two-instance contention test), and A4.3 (migrations to CI).

## Context

Every posted transaction extends its tenant's tamper-evident audit hash chain:
each `audit_log` row stores `prev_hash` (the previous row's `row_hash`) and its
own `row_hash = H(tenant, row, prev_hash)`. Verification walks the chain from the
genesis hash and recomputes each link, so any after-the-fact mutation of a past
row breaks every link after it (ADR-012, migration 0009).

Today that chain is extended **synchronously, inside the posting transaction**:
the post reads the tenant's current chain head, computes the new link, and inserts
the `audit_log` row in the same transaction that writes the postings. Correctness
under concurrency rests on two things: SERIALIZABLE isolation, and an **in-process
per-tenant mutex** (ADR-012) that serializes same-tenant posts so two of them
cannot read the same chain head and fork the chain.

That mutex lives in one process's memory. It is a **single-instance correctness
cliff** (audit A3.6, a Blocker): the moment a second app instance runs, two
instances can post the same tenant concurrently, both read the same head, and
write two rows that both claim the same `prev_hash`, forking the chain. SERIALIZABLE
alone does not save us, because the two inserts touch different new rows and need
not conflict on a serialization dependency the database can see. The service
therefore cannot be run as more than one instance without risking a corrupt audit
chain, which caps availability and throughput and blocks horizontal scaling.

We need the audit chain to stay correct with N app instances, and we would also
like the chain off the posting hot path (audit A5.1): today a hot tenant's
throughput is bounded by serialized chain extension.

## Decision

### 1. Decouple the chain with a transactional outbox and a single chainer

Stop extending the hash chain inside the post. Instead:

- **The post writes an outbox row, not a chain link.** Inside the same
  transaction that writes the postings, the service inserts an append-only
  `audit_outbox` row describing the event (tenant, action, transaction id, actor,
  before/after snapshot). This is atomic with the post: the event is recorded if
  and only if the transaction commits. No chain head is read, no `row_hash` is
  computed, so **the per-tenant mutex is removed from the hot path** and multiple
  instances can post the same tenant concurrently.
- **A single asynchronous chainer builds the chain.** One background worker drains
  `audit_outbox` in a well-defined total order, computes each tenant's hash chain,
  inserts the `audit_log` rows (the same schema and hashing as today), and marks
  the outbox rows processed. Because exactly one consumer ever extends the chain,
  there is no fork, regardless of how many instances produce events.

The audit chain becomes **eventually consistent**: a committed transaction is
durable immediately (postings + outbox row), and its audit-chain link appears a
short time later when the chainer processes it. This lag is bounded and monitored
(see 5).

### 2. Ordering: process only settled rows, in transaction-commit order

The hard part of any outbox is ordering. `audit_outbox.id` is a `bigserial`
assigned at insert time, but transactions commit out of order, so a naive
"process id greater than the last processed" can permanently skip a row that was
assigned a lower id but committed later. That would leave a committed event out of
the chain forever.

We solve it with transaction visibility, the standard correct approach:

- Each outbox row records the inserting transaction id:
  `txid bigint NOT NULL DEFAULT pg_current_xact_id()`.
- The chainer reads the oldest transaction still in flight,
  `pg_snapshot_xmin(pg_current_snapshot())`, and processes **only rows whose
  `txid` is below that xmin**. Such rows are guaranteed committed, and no
  still-running transaction can later reveal an earlier-ordered row that is not
  yet visible. It processes them ordered by `(txid, id)`.

This gives a single, deterministic total order over all committed events, with no
skipped-then-resurfaced row. Rows from aborted transactions simply never appear
below xmin as unprocessed work that matters (their txid is settled and they either
committed or rolled back; a rolled-back insert leaves no row). The chain records a
consistent total order over exactly the committed events, which is all
tamper-evidence requires (it does not need the one "true" real-time order of
concurrent posts, only a fixed order it can recompute).

The chained rows are ordered by a database-assigned `chain_seq` (a sequence the
single chainer writes in insert order), not by their UUID primary key: a UUIDv7
id is monotonic only within one process, so ordering the verify walk by it would
let a leader failover to a host with a skewed clock mint a lower id than the
current head and read as a fork. A database sequence, assigned by the one writer
in processing order, is monotonic and clock-skew-proof. As a defense in depth,
`audit_log` also carries the source `outbox_id` under a UNIQUE constraint, so if
two writers ever did run at once (the failure this ADR exists to prevent), the
second attempt to chain the same event fails the insert instead of forking the
chain silently. The chainer runs all of its work on the same connection that
holds the leader advisory lock, so losing that lock session fails the very next
query and drops leadership rather than draining blind against a lock it no longer
holds.

### 3. Exactly one chainer: leader election via advisory lock

The single-consumer guarantee is enforced with a **session-level Postgres
advisory lock** on a fixed key. On startup every instance runs a chainer loop that
first tries `pg_try_advisory_lock(key)`; the one that gets it is the leader and
does the work, the others idle and retry periodically. If the leader crashes or
disconnects, Postgres releases the session lock automatically and another instance
acquires it on its next attempt. This needs no external coordinator (no ZooKeeper,
no etcd): the database we already depend on is the coordinator. A single global
chainer is more than sufficient for this service's volume; per-tenant sharding of
the chainer is a future option the outbox order already supports, not something we
build now.

### 4. Remove the hot-path mutex; keep SERIALIZABLE as the backstop

The in-process per-tenant mutex existed to serialize chain extension. With the
chain gone from the post, it is removed from the posting path. SERIALIZABLE
isolation stays: it still protects the balance and idempotency invariants (the
idempotency primary key remains the exactly-once mutex for duplicate posts, and
the per-currency balance trigger still fires in-transaction). Concurrent
same-tenant posts now proceed in parallel up to what SERIALIZABLE and the
idempotency key allow, across any number of instances.

### 5. Verification and lag semantics

`audit/verify` walks `audit_log`, the chained rows, exactly as before, and its
tamper-evidence guarantee is unchanged for everything the chainer has processed. A
transaction committed but not yet chained is **pending**: it is in `audit_outbox`,
not yet in `audit_log`, so it is not yet covered by the chain. We make this
explicit rather than pretend it away:

- Verify reports the chained head and, alongside it, the count of pending
  (unprocessed) outbox rows, so a caller can see the chain is current or lagging.
- The outbox lag (oldest unprocessed row age, and unprocessed depth) is a metric
  and an alert (ADR-015 Phase 5 observability). A growing lag means the chainer is
  down or behind, which is an operational signal, not a data-loss event: the
  events are durable in the outbox and will be chained when it recovers.
- The restore-verify job (ADR-016) additionally checks that every committed
  transaction is eventually represented in the chain (no outbox row left
  unprocessed indefinitely), catching a chainer that silently stopped.

### 6. Migrations run in CI before deploy (A4.3)

With multiple instances, a schema change must land before the new code that needs
it, and no instance may run migrations racing another. Migrations move to a
dedicated pre-deploy CI step that runs `goose up` once against the database before
any instance rolls over, gated and ordered in the pipeline, rather than each
instance migrating on boot. This is recorded here because it is a direct
consequence of going multi-instance.

## Consequences

- The single-instance correctness cliff is removed: the service can run N
  instances without forking any tenant's audit chain. Horizontal scaling and
  rolling deploys become safe.
- The audit chain is now **eventually consistent** with a bounded, monitored lag.
  Consumers that need "is this transaction in the tamper-evident chain yet" must
  treat chaining as asynchronous. This is a real semantic change, called out in
  the API docs for verify.
- New moving parts: the `audit_outbox` table, the chainer worker, the leader
  advisory lock, and the xmin-based ordering. Each is simple on its own; together
  they are more than the old synchronous write. The complexity buys multi-instance
  correctness, which the old design could not provide at any complexity within one
  process.
- Backpressure is on the outbox, not the client: a slow or stopped chainer grows
  the outbox but never blocks or fails a post. Monitoring the outbox depth is now
  an operational must.
- The chainer is a single writer to `audit_log`; its throughput must exceed the
  aggregate post rate. For this service's volume one worker is ample; if it ever
  is not, the `(txid, id)` order supports partitioning the chainer by tenant
  without changing the model.
- A two-instance, same-tenant contention test (A8.2) is written first, against the
  current design, to record the failure it removes, and then to prove the new
  design sustains concurrent same-tenant posts across instances without chain
  forks. That test is the acceptance gate for this ADR.
- This ADR is about correctness under multiple instances, not about availability
  (a hot standby, failover). Availability remains the managed-Postgres growth path
  in ADR-016. Running multiple app instances does improve app-tier availability as
  a side effect, but the single Postgres remains the availability floor.

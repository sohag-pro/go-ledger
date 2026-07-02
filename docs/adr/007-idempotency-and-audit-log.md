# ADR-007: Idempotency Keys and the Immutable Audit Log

## Status

Accepted: 2026-07-02

## Context

Through Week 5 the ledger had a REST API but no protection against retries. A
`POST /v1/transactions` that times out may have already committed: the request
reached the server and posted, and only the response was lost. A correct client
retries, and without protection that retry posts a second transaction, moving
money twice. Week 6 adds an idempotency contract so a retry is safe, and an audit
log so every posting leaves a tamper-evident record of what happened.

Both tables (`idempotency_keys`, `audit_log`) were designed in ADR-003 and left
for this week; `RunInTx` was called out in ADR-004 as the seam the audit write
would use. This ADR records the decisions those earlier ADRs deferred.

Several choices had no obvious default.

## Decision

### The idempotency key row is inserted inside the posting transaction

The client sends an optional `Idempotency-Key` header. When present, the key row
is inserted into `idempotency_keys` inside the same SERIALIZABLE transaction
(`RunInTx`) that inserts the transaction, its postings, and the audit row. The
primary key `(tenant_id, idempotency_key)` is the exactly-once mutex: the first
request to commit the key wins, and any concurrent duplicate hits the unique
violation, rolls back its whole transaction (so its optimistically written
postings vanish too), and replays.

This was chosen over the obvious check-then-act approach (look the key up, and if
absent, post). Check-then-act has a race: two concurrent copies of the same
request both read "key absent," both post, and both create a transaction. The bug
only appears under concurrency, which is exactly when retries happen. Making the
check and the write one atomic step requires the database as the referee; the
unique primary key is that referee, so no application lock or separate
idempotency service is needed.

`InsertIdempotencyKey` maps the PK collision (constraint `idempotency_keys_pkey`)
to an internal `ErrDuplicateIdempotencyKey`. `TransactionService.Post` catches it
(only when a key was supplied), reads back the committed row, loads the original
transaction, and returns it with `replayed = true`, which the API surfaces as the
`Idempotent-Replayed` response header. Because Postgres blocks the second insert
until the first transaction resolves, the loser's follow-up read always sees the
committed winner; there is no visibility gap.

### A request fingerprint distinguishes a retry from key reuse

The stored key row holds a fingerprint of the original request:
`Transaction.Fingerprint()`, a SHA-256 over the currency and each posting's
account, signed amount, and description, in order, with the transaction id
excluded. On a repeat, a matching fingerprint is a genuine retry and gets the
replay; a different fingerprint means the client reused a key for a different
payment, and the service returns `ErrIdempotencyConflict`, mapped to HTTP 409.

The fingerprint length-prefixes each field rather than joining with a separator
byte. A separator scheme is forgeable when a field can contain that byte: two
different transactions could be crafted to collide, and a collision on a reused
key would let a different payment through as a "replay." Fixed-width length
prefixes make field boundaries impossible to fake regardless of content.

### The idempotency key is optional, not required

An absent header posts normally, exactly as before Week 6. This keeps the change
backward compatible (the console and seeder callers are unaffected) and matches
common payment APIs where the key is recommended but not mandatory. A keyless post
is still audited.

### The audit log is written in the same transaction and is create-only

Each posted transaction writes one `audit_log` row inside the same `RunInTx`
closure as the postings: `action = transaction.created`, `before` NULL, `after` a
JSON snapshot of the transaction, `actor` the tenant id (until auth lands). Because
the audit insert shares the transaction, a posting and its audit record commit
together or roll back together; there is no separate "log it later" step that could
fail on its own and leave a transaction nobody recorded. On the replay path the
transaction rolled back and no new posting happened, so no audit row is written:
the log records only real creations.

It is deliberately create-only. Postings are append-only, so there is no in-place
update to diff; a generic before/after framework would be speculative (`before`
stays NULL for now).

### Append-only is enforced by a database trigger, with one gated exception

A trigger on `audit_log` rejects UPDATE and DELETE (`BEFORE UPDATE OR DELETE ...
FOR EACH ROW`), so the log is immutable even against a direct `DELETE FROM
audit_log` in psql, not just by application convention. This is the same
defense-in-depth pattern as the balance and currency triggers (ADR-004, ADR-005).

The one sanctioned exception is the demo seeder, which resets the tenant every few
hours and must clear the log. The trigger allows the mutation only when the
transaction-local GUC `audit.allow_purge` is set to `on`; the seeder sets `SET
LOCAL audit.allow_purge = 'on'` at the start of its reset transaction. `SET LOCAL`
is transaction-scoped, so it cannot leak to other pooled connections, and the
application's normal path never sets it, so production stays immutable.

### Audit reads: by transaction unpaginated, by account keyset-paginated

`GET /v1/transactions/{id}/audit` returns a transaction's audit rows unpaginated,
because a transaction has a fixed, tiny number of them. `GET
/v1/accounts/{id}/audit` reuses the Week 5 keyset cursor on `(created_at, id)`,
because an active account can accumulate a row for every transaction that ever
touched it, and an unbounded query is a memory and latency risk on a real account.
The by-account query joins `audit_log` to the account's postings and is tenant
scoped on both the outer table and the subquery.

## Consequences

### Positive

- Retrying a payment is safe: 100 concurrent requests with the same key create
  exactly one transaction and one audit row, verified by an integration test that
  runs the real race against Postgres.
- Exactly-once needs no distributed lock or idempotency service; a unique
  constraint inside the existing transaction does it.
- Every posting leaves an immutable, transactionally-consistent audit record that
  the database itself refuses to alter.
- The change adds no API surface that can be misused: the key is optional and the
  audit reads are bounded.

### Negative

- The idempotency key table grows without bound in v1 (no TTL or expiry); the demo
  seeder clears it on reset, and real retention is a later concern.
- The audit `after` snapshot duplicates data already in the postings; accepted, as
  the point of an audit log is a self-contained record.
- The immutability trigger's gated branch currently permits any mutation (not just
  DELETE) when the GUC is set; only the seeder's DELETE uses it today, so a gated
  UPDATE would silently no-op. Tracked as a follow-up (fail closed on UPDATE).
- Replay and conflict paths are counted (`IdempotencyReplays`,
  `IdempotencyConflicts`) but not timed in the `PostDuration` histogram.

## Alternatives considered

- **Check-then-act idempotency (look up, then post)**: rejected. Races under
  concurrent retries and creates duplicate transactions exactly when it matters.
- **A separate idempotency middleware with its own table and short transaction**:
  rejected. The key insert would not share the posting's transaction, reopening a
  window where the posting commits but the key does not (or two requests both
  proceed); keeping the mutex inside the posting transaction is simpler and
  strictly correct.
- **Required idempotency key**: rejected for v1. A breaking change to the existing
  contract and to the console and seeder callers, for a guarantee clients can opt
  into per request.
- **Fingerprint by hashing the raw request body**: rejected. Whitespace, key
  order, or an echoed id would make identical payments look different and break the
  replay; hashing the semantic content is stable.
- **Separator-byte fingerprint framing**: rejected. Forgeable when a field can
  contain the separator, allowing a collision on a reused key; length-prefixing is
  collision-safe.
- **Audit log immutability by convention only**: rejected. A trigger enforces it at
  the database, consistent with the ledger's other invariants, so a buggy caller or
  a bad migration cannot rewrite history.
- **Generic before/after audit framework now**: deferred. Nothing mutates in place,
  so create-only with a NULL `before` is enough; a diff model is speculative.
- **Unpaginated by-account audit read**: rejected. Fine for the demo, but an
  unbounded query is a production memory and latency risk; keyset paging matches the
  statement endpoint.

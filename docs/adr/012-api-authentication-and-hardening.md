# ADR-012: API authentication, mandatory idempotency, input hardening, and a tamper-evident audit chain

## Status

Accepted: 2026-07-08

## Context

A security and correctness audit of the service (performed against the premise
"assume real money flows through this") found the accounting core sound but the
service perimeter wide open. The double-entry invariant is enforced twice, money
is integer minor units with no float, balances are derived from immutable
postings, and posting is atomic under SERIALIZABLE with a correctly-scoped
idempotency mutex. None of that is the problem. The problem is everything in
front of it.

The audit's top five findings:

1. **No authentication or authorization.** Every request acted as a single tenant
   read from config (`DEFAULT_TENANT_ID`). Anyone who could reach `/v1` could move
   money and read every balance.
2. **Idempotency was opt-in.** A client that omitted the `Idempotency-Key` header
   and retried after a dropped response would double-post.
3. **Unbounded input.** The `postings` array had a minimum but no maximum, and
   there was no request body-size limit, so one request could become an enormous
   transaction.
4. **No rate limiting, incomplete server timeouts.** Only `ReadHeaderTimeout` was
   set.
5. **The audit log was guarded but not tamper-evident.** An immutability trigger
   rejected UPDATE and DELETE, but the rows were not cryptographically chained, so
   a sufficiently privileged database role could rewrite history.

The hard constraint on any fix: the public try-it console at `/console` and the
Scalar playground must keep working with no login, and the demo tenant must keep
resetting every four hours (see the demo seeder). A naive "require auth
everywhere" breaks the demo, which is the point of the public deployment.

This ADR records the decisions that close all five findings while keeping the
demo intact.

## Decision

### API-key authentication, keys stored hashed in the database

Authentication is a bearer API key: `Authorization: Bearer glk_<random>`. A new
`api_keys` table holds one row per key: `id`, `tenant_id`, `name`, `key_hash`
(unique), an optional `rate_limit_rpm`, `created_at`, and a nullable
`revoked_at`. Only the SHA-256 hash of the key is stored; the plaintext is shown
once at creation and is never recoverable from the database. A leaked database
dump does not leak usable keys.

An authentication middleware reads the bearer token, hashes it, looks up an
unrevoked row, and puts the resolved `tenant_id` (and the key's rate limit) into
the request context. A missing or unknown or revoked key is a `401`. To keep the
hot path off the database on every request, resolved keys are cached in memory by
hash with a short TTL; revocation is therefore effective within that TTL, which
is an acceptable trade for a service at this scale.

The tenant is derived **only** from the key. No request field, header, or body
can set or override it. That is what makes tenant scoping the authorization
boundary: a key for tenant A can only ever act on tenant A, and the composite
foreign keys from Week 3 already make a posting into another tenant's account
impossible at the database. So "can this principal touch this account?" reduces
to "is this account in the key's tenant?", which the schema enforces for free. We
deliberately did not add per-account access-control lists: they would be
redundant with the tenant foreign keys and add a whole authorization surface for
no gain at this scope.

Only `/v1/*` requires a key. Liveness (`/healthz`), the landing page (`/`), the
console, the playground, static assets, the OpenAPI documents, and the loopback
metrics endpoint stay unauthenticated: they are either public by design or
already off the public interface. The gRPC surface moves the same money, so it
gets the same key check through its existing interceptor chain (ADR-009), reading
the token from request metadata. Leaving gRPC open would make the whole exercise
a REST-only speed bump.

### A public demo key keeps the console open, with one auth path

Rather than special-case the demo tenant as unauthenticated (two code paths, and
a permanently open write surface), authentication is uniform and the demo is
reached with a real, low-privilege **demo key**. It is provisioned at startup
from `DEMO_API_KEY` (a known, public value with a safe default), scoped to the
demo tenant, and carries a tighter rate limit than a normal key. The console and
the playground ship it. Exposing it is fine on purpose: it can only touch the
demo tenant, it is rate limited, and that tenant is wiped every four hours.

The demo key survives the wipe because the seeder resets tenant **data**
(accounts, transactions, postings, audit rows, idempotency keys) and never
touches the `api_keys` table. So after each four-hour reset the console keeps
working with the same key against a fresh ledger, exactly as before.

### Idempotency is mandatory on money-moving POSTs

`POST /v1/transactions` now requires an `Idempotency-Key`; its absence is a `400`
with a clear message. We rejected auto-deriving a key from the request
fingerprint when the header is missing: that would silently collapse two
genuinely separate but identical payments (same accounts, same amount) into one,
turning a real second payment into a false replay. Making the client name the key
keeps "these are the same request, retried" distinct from "these are two
payments that happen to look alike." The console generates a fresh UUID per post.

### Input hardening: bounded arrays, bounded bodies, complete timeouts

The `postings` array gets a maximum (100 legs), and the HTTP handler is wrapped
with a request body-size limit, so one request can no longer become an
arbitrarily large transaction or exhaust memory before validation. The HTTP
server gains the timeouts it was missing: `ReadTimeout`, `WriteTimeout`, and
`IdleTimeout`, alongside the existing `ReadHeaderTimeout`, so a slow client can no
longer hold a connection open indefinitely.

### Per-key rate limiting

Each key is rate limited independently: an in-memory token bucket per key hash,
with the limit taken from the key's `rate_limit_rpm` (a default applies when the
column is null). Over the limit is a `429` with `Retry-After`. The demo key is
set lower; a normal key uses the default; and the local load-test stack
provisions a high-limit key so the 500 RPS load test is exercising the ledger,
not the limiter. The default is configurable so production and the load stack can
differ without code changes.

### A per-tenant, tamper-evident audit chain

The audit log gains two columns, `prev_hash` and `row_hash`. Each row's hash is
`SHA-256` over its own content (tenant, action, transaction id, actor, before,
after, created_at) plus the previous row's hash, with every field length-prefixed
so no field's bytes can be mistaken for a boundary (the same framing the
idempotency fingerprint already uses). `prev_hash` is the previous audit row's
`row_hash` **for that tenant**, or a fixed genesis constant for a tenant's first
row. The chain is extended inside the same database transaction that posts the
ledger transaction, so a committed transaction always leaves the chain
consistent, and `created_at` is set by the application (not the database default)
so the hash is deterministic and verifiable.

The chain is per tenant, not global, for two reasons: it keeps tenants
decoupled, and it means the four-hour demo wipe restarts only the demo tenant's
chain from genesis without touching or invalidating any other tenant's history.

A new `GET /v1/audit/verify` endpoint walks the caller's tenant chain oldest
first, recomputes every row hash, and returns `{valid, checked, first_break_id}`,
so tamper-evidence is not just stored but checkable. This sits on top of, not
instead of, the existing immutability trigger: the trigger prevents casual
mutation, and the chain detects a privileged rewrite that bypasses it.

### Same-tenant posts serialize with an in-process mutex

The audit chain's read-then-insert above is a genuine same-tenant conflict.
Every post reads the tenant's latest `row_hash` and then inserts the next row,
in the same SERIALIZABLE transaction as the ledger posting, so two concurrent
same-tenant posts reading the same chain tail is a real read-write
antidependency. PostgreSQL aborts the loser with a serialization failure
(SQLSTATE 40001), and under sustained same-tenant concurrency the repeated
abort can exhaust the retry budget (25 attempts) and surface to the caller as
a `503`.

We first tried closing this with a per-tenant PostgreSQL session advisory
lock (`pg_advisory_lock`), taken on a connection checked out from the pool
before the SERIALIZABLE transaction began, so a blocked waiter's snapshot was
not fixed until the lock holder had committed. A review found this had two
problems specific to this service's own configuration, serious enough to
replace rather than patch:

1. The connection pool sets `lock_timeout` to 3 seconds. `pg_advisory_lock`
   waits are subject to `lock_timeout`, so a waiter under sustained
   contention could be aborted by Postgres with SQLSTATE 55P03. That code is
   not a serialization failure, was not retried, and was not mapped to
   `domain.ErrConflict`, so it surfaced as an opaque `500` instead of the
   intended `503`.
2. The lock was acquired on a connection checked out from the pool before the
   wait began, and held for the whole call, including every retry's backoff.
   A burst of same-tenant posts could check out and hold most or all of the
   pool's connections while merely waiting on the lock, starving every other
   tenant's posts of a connection. That defeats the entire point of scoping
   the lock per tenant: different tenants are supposed to stay fully
   parallel.

The fix, since go-ledger runs as a single VPS instance rather than a fleet, is
an in-process keyed mutex in the repository adapter
(`internal/postgres/repository.go`): one `sync.Mutex` per tenant id, held for
the whole `RunInTx` call but never around a checked-out database connection.
A post for a tenant that already has one in flight blocks on that Go mutex,
holding no connection while it waits; once unblocked it begins its own
attempt exactly as before the advisory lock existed, acquiring a connection
from the pool only when it actually runs, and releasing it between attempts
(including during backoff). Different tenants use different mutexes and never
wait on each other. The map of per-tenant mutexes grows with the number of
distinct tenants ever seen and is never evicted; at this service's tenant
count that stays small and cheap, so eviction was not built.

This closes both problems above: there is no database lock wait, so
`lock_timeout` and SQLSTATE 55P03 do not apply, and a waiting goroutine never
holds a pool connection, so a hot tenant's backlog cannot starve other
tenants of connections. The SERIALIZABLE retry loop is unchanged and remains
the correctness backstop, not the primary serialization mechanism for
same-tenant posts: on today's single instance it now runs only after the
in-process mutex has already made same-tenant execution one at a time, so it
should rarely see a same-tenant conflict at all. If go-ledger is ever run as
more than one instance, the in-process mutex only serializes posts within one
instance; the retry loop is what still guarantees correctness across
instances, since a same-tenant race between two different instances remains
possible (rare) and would retry rather than corrupt the chain.

This supersedes ADR-004's negative consequence "conflicts cost retries, not
blocked connections," specifically for same-tenant posting. That trade still
holds for the SERIALIZABLE mechanism itself and for any cross-instance
conflict, but for same-tenant posts on today's single instance the practical
cost is now waiting on an in-process mutex, not a retry, and not a blocked
database connection.

## Consequences

### Positive

- `/v1` and gRPC both require a key, the tenant comes only from the key, and
  cross-tenant access is impossible by construction. The perimeter is real.
- Retrying a payment is safe by default: a missing idempotency key is rejected up
  front rather than silently double-posting.
- One request can no longer exhaust memory or hold a connection open, and a single
  key cannot flood the service.
- The audit log is now cryptographically tamper-evident and verifiable through the
  API, not merely trigger-guarded.
- The public demo and its four-hour reset are unchanged: one uniform auth path, a
  public demo key that survives the wipe, no special-casing.

### Negative

- Every `/v1` request now does a key lookup; the in-memory cache keeps that off
  the database on the hot path but means revocation lags by up to the cache TTL.
- The audit chain requires same-tenant posts to serialize: one at a time, in
  the order they arrive. On today's single-instance deployment this is
  enforced by an in-process per-tenant mutex (see the Decision section above),
  not by a database lock, so different tenants stay fully parallel and a hot
  tenant's queued posts hold no database connection while they wait. It is
  still real coupling that a very high-throughput single tenant would feel as
  added latency, just not as blocked connections or a database lock timeout.
- Key management for real tenants is still manual this pass (insert a row, or a
  small CLI): there is no self-service key issuance UI, which is out of scope.
- The demo key is public by design. That is safe only because it is tenant-scoped,
  rate limited, and wiped, and those three properties must stay true.

## Alternatives considered

- **API keys in config instead of a table**: rejected. Config keys are not
  revocable or provisionable without a redeploy, and a hashed table is barely more
  code while supporting rotation and per-key rate limits.
- **JWT bearer tokens**: rejected for v1. Stateless tokens need an issuance path
  and a signing-key rotation story that a ledger with a handful of tenants does
  not need yet. API keys are the lower-ops fit.
- **Leaving the demo tenant unauthenticated**: rejected. It means two code paths
  and a permanently open write surface. A uniform auth path with a weak public key
  is simpler and safer.
- **Auto-deriving an idempotency key from the fingerprint**: rejected. It rejects
  two legitimately-identical separate payments as replays, which on a payment
  system is a correctness bug, not a safety feature.
- **Per-account access-control lists**: rejected as redundant. The tenant foreign
  keys already make cross-tenant access impossible, and the key already resolves
  to exactly one tenant.
- **A single global audit chain**: rejected. It couples all tenants into one
  hash sequence and makes the per-tenant four-hour wipe impossible without
  breaking the chain. Per-tenant chains keep the demo reset clean.
- **Cryptographic signing of each audit row (HMAC or asymmetric) instead of a
  hash chain**: deferred. A hash chain detects reordering, insertion, and
  deletion, which is the property the audit needed. Signing adds authenticity of
  the writer on top, which matters once there is more than one writer identity;
  that is a later concern.
- **A per-tenant PostgreSQL session advisory lock, to serialize same-tenant
  posts**: tried and reverted. It closed the audit-chain serialization storm
  but introduced two problems of its own: the lock wait was subject to the
  pool's `lock_timeout` and could abort with an unmapped SQLSTATE 55P03 (an
  opaque `500` instead of a `503`), and the lock was held on a connection
  checked out before the wait began, so a same-tenant burst could hold most or
  all of the pool's connections purely waiting, starving other tenants. An
  in-process per-tenant mutex has neither problem, since it never touches a
  database connection while waiting.

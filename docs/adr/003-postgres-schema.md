# ADR-003: Postgres Schema and Repository Layer

## Status

Accepted: 2026-06-29

## Context

Week 3 puts the domain model (ADR-001, ADR-002) onto disk. We need a schema for
`accounts`, `transactions`, and `postings`, a repository layer over it, and an
integration test that runs the full happy path against a real Postgres. Several
choices had no obvious default and would be costly to reverse once data exists,
so each is recorded here.

The hardest question is how balances are stored. Two shapes:

- **Mutable balance column**: each account row carries a `balance` updated in
  place on every posting. Reads are a single column lookup.
- **Append-only postings, derived balances**: postings are immutable rows;
  balance is `SUM(amount)` over an account. Nothing is updated in place.

A mutable balance is faster to read but reintroduces exactly the bug double-entry
exists to prevent: the stored number and the sum of postings can drift, and once
they drift there is no way to tell which is right. It also turns every posting
into a read-modify-write that must be serialized.

## Decision

Postings are append-only and balances are derived. This carries the core
invariant from ADR-001 into the database. The schema, with the reasoning for each
non-obvious choice:

- **Three tables now, not five.** The build plan lists five tables, but
  `idempotency_keys` (Week 6) and `audit_log` (Week 6) are designed and built in
  the weeks that use them, when their columns are actually known. `0001_initial`
  creates only `accounts`, `transactions`, and `postings`. Migrations are cheap;
  guessing a schema before its code exists is not.

- **Multi-tenant from day one.** Every table carries a `tenant_id`. There is no
  auth or tenant resolution yet (that comes later); the column and the foreign
  key discipline exist now because retrofitting tenancy onto a populated ledger
  is a migration no one wants to write. Repository methods are all scoped by
  tenant.

- **UUIDv7 primary keys, generated in the application.** Keys are `uuid` columns;
  the app generates a UUIDv7 before insert. v7 is time-ordered, so it keeps index
  locality close to a serial key while staying globally unique and decoupled from
  the database. Generating in Go (not `gen_random_uuid()`) means the caller knows
  the id without a round-trip and tests are deterministic.

- **Composite foreign keys enforce tenant integrity.** `accounts` and
  `transactions` carry a `UNIQUE (tenant_id, id)`. `postings` references both via
  `(tenant_id, account_id)` and `(tenant_id, transaction_id)`. A posting cannot
  point at an account or transaction in a different tenant: the database rejects
  it, not just application code.

- **Currency lives on the transaction, not the posting.** The domain already
  enforces that all postings of a transaction share one currency
  (`Transaction.Validate`). Storing it once on the transaction keeps a posting to
  a single signed `bigint amount`, and each posting's `Money` is reconstructed
  from the transaction's currency on read. Accounts also carry their own currency.

- **Account type is `text` with a CHECK, not a Postgres enum.** The five types
  are constrained by `CHECK (type IN (...))`. Readable in plain SQL, and adding or
  changing a type is an ordinary migration rather than the awkward `ALTER TYPE`
  dance an enum forces.

- **No balance CHECK constraint yet.** The sum-to-zero invariant is enforced in
  the domain (`Transaction.Validate`) for Week 3. The database-level CHECK lands
  in Week 4 alongside the transaction-posting service and its concurrency
  handling, where it belongs.

The repository follows a port-and-adapter split: `domain.Repository` is the port
(the domain owns the contract); `internal/postgres` is the adapter, built on pgx
and sqlc-generated queries. `CreateTransaction` validates the double-entry
invariant, then writes the transaction and all its postings in one database
transaction, so a half-written transaction can never exist. Tooling is goose for
migrations, sqlc for type-safe queries, and testcontainers-go for the integration
test (all locked in the build plan).

## Consequences

### Positive

- The stored ledger cannot disagree with itself: there is no second copy of the
  balance to drift from the postings.
- Tenant isolation is enforced by the database, not just by careful queries.
- The domain stays free of storage and uuid types; the port is a plain Go
  interface, so the service layer (Week 4) can be tested against a fake.
- UUIDv7 keeps write-side index locality without coupling ids to the database.

### Negative

- Balance reads are an aggregate (`SUM`) rather than a column read. Fine at v1
  scale; if it ever bites, the answer is a materialized rollup that is rebuilt
  from postings, never a mutable primary balance.
- The integration test needs a Docker daemon. It skips (does not fail) when none
  is reachable, so local `make test` stays green; CI runs Docker and exercises
  the real path.

## Alternatives considered

- **Mutable balance column**: rejected. Reintroduces drift between stored balance
  and posting history, the exact failure double-entry prevents, and serializes
  every posting behind a row update.
- **All five tables in `0001`**: rejected. Designs `idempotency_keys` and
  `audit_log` before the code that uses them; they get their own migrations in
  Week 6.
- **Single-tenant schema, add tenancy later**: rejected. Adding `tenant_id` and
  backfilling foreign keys on a live ledger is far more painful than carrying the
  column from the start.
- **DB-generated UUIDv4 (`gen_random_uuid()`)**: rejected. Random keys hurt index
  locality and force a read-back to learn the generated id.
- **Postgres enum for account type**: rejected. `ALTER TYPE` is more friction than
  a `text` column with a CHECK, for no real gain.

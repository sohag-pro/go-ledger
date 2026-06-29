# ADR-006: REST API Design

## Status

Accepted: 2026-06-29

## Context

Through Week 4 the ledger was exercised only by tests; the service had no network
surface. Week 5 puts a REST API in front of it: create accounts, post
transactions, and read accounts, balances, statements, and transactions. The
router is chi (a locked decision), and the project already generates its OpenAPI
spec from the handlers with huma, with a drift test and a self-hosted playground.

Several choices had no obvious default.

## Decision

### huma operations on chi

The endpoints are huma operations running on a chi router via the humachi adapter
(swapped from humago). Each endpoint is a typed operation: huma derives request
validation from struct tags, renders errors as RFC 7807 `application/problem+json`,
and generates the OpenAPI spec, all from the same handler. chi owns routing and
the middleware chain (request id, recovery, structured request logging).

This was chosen over plain chi handlers plus go-playground/validator plus
hand-written problem+json (the build plan's literal wording). huma already solves
validation, error formatting, and spec generation as one unit, and the existing
drift test and playground depend on the spec being generated from handlers. The
plan predates the huma decision; huma's validation supersedes the separate
validator cleanly. The cost is using huma's validation tags (`minLength`, `enum`,
`pattern`) rather than go-playground's, which is minor.

### Money as integer minor units

Amounts cross the wire as a signed integer count of minor units plus a currency
code (`{"amount": 10000, "currency": "USD"}`), mapping 1:1 to the internal `int64`.
No floating point and no decimal parsing at the boundary. This is what most
payment APIs (for example Stripe) do, and it keeps the API as exact as the ledger.

### Single default tenant for now

The schema is multi-tenant, but there is no auth yet. Rather than expose tenancy
in URLs or trust a client header, every request acts as a single default tenant
(`DEFAULT_TENANT_ID`, with a fixed fallback), injected by the API layer into each
service call. When auth lands it will populate the same tenant value from a token
instead, without changing any resource URL.

### Account statement with running balance and keyset paging

Beyond the planned endpoints, `GET /v1/accounts/{id}/statement` lists the
account's postings, newest first, each with the account's running balance as of
that posting (a window `SUM` over the account's history), and pages with keyset
(cursor) pagination on `(created_at, id)`. Keyset paging is stable under
concurrent inserts in a way offset paging is not.

Because balances are derived, never stored (ADR-003), the running-balance window
scans the account's full posting history per page (one index range on
`(tenant_id, account_id)`). That is O(history) per page, fine at v1 scale; the
future optimization, if it ever bites, is a materialized running balance rebuilt
from postings, never a mutable primary balance.

### Per-posting description

Each posting carries an optional free-text `description` (narration), bounded at
256 characters in both the domain and a database CHECK. It appears on every
posting in requests and responses, including statement entries.

### Server now owns its database, with startup migrations

`cmd/server` reads `DATABASE_URL` (required, fail-fast: a ledger API with no
database must not serve), opens the tuned pool, and applies the embedded goose
migrations on startup before serving. On a single instance this is the simplest
correct option: the binary that needs a column also creates it, atomically with
the deploy. If the project ever runs more than one instance, the same `goose.Up`
moves into a dedicated CI step to avoid a migration race.

Production Postgres runs natively on the VPS (a dedicated database and user,
listening on localhost, backed up with `pg_dump`), consistent with the
native-binary deploy. This is provisioned out of band before the deploy; see the
ops runbook.

### Error mapping

Domain errors map to status codes in one helper: not-found to 404, duplicate id to
409, an exhausted serialization conflict to 503, validation and invariant failures
to 422, and anything unrecognized to a 500 that does not leak internals.

## Consequences

### Positive

- One source of truth for handlers, validation, errors, and the spec; the
  published docs cannot drift from the running API.
- The API is as exact as the ledger (integer minor units), and statements give a
  real running balance with stable pagination.
- The transport stays thin: services hold the logic, handlers translate.
- Auth can be added later by changing only how the tenant is resolved.

### Negative

- huma owns request decode/encode, so a very custom response shape means working
  within its model.
- Statement pages cost a scan of the account's posting history while balances stay
  derived. Accepted for v1; a rollup is the escape hatch.
- The server now hard-depends on Postgres, so the deploy requires the database to
  be provisioned and `DATABASE_URL` set first.

## Alternatives considered

- **chi + go-playground/validator + hand-written problem+json**: rejected.
  Reintroduces three concerns huma already covers and risks spec drift.
- **Spec-first with oapi-codegen**: rejected. Drops the existing huma investment
  (playground, drift test, generator) and makes the spec a hand-maintained
  artifact, heavy for a solo build.
- **Decimal-string money in JSON**: rejected. Adds parsing and per-currency scale
  validation at the boundary for no gain over exact integer minor units.
- **Tenant in the URL or a trusted header now**: rejected. Bakes tenancy into the
  resource model or trusts an unauthenticated header; the default-tenant seam is
  cleaner to evolve into real auth.
- **Migrations as a separate step now**: deferred. Correct for multi-instance, but
  premature for a single VPS; startup migrations are simpler today.

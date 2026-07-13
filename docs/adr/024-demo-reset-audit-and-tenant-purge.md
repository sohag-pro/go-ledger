# ADR-024: Demo seeder writes audit events and purges visitor tenants

Status: Accepted
Date: 2026-07-13

This ADR records two changes to the demo seeder (`internal/seed`), the
in-process tool that resets and repopulates the public demo on a schedule. Both
close inconsistencies a visitor could see at go.sohag.pro. Neither touches the
core service path; the seeder remains a demo-only tool.

## Context

The demo seeder writes rows directly (with backdated `created_at`, which the
service does not allow) so a statement reads like real history. Two gaps
surfaced in use:

1. **Seeded data had no audit trail.** A transaction posted through the API
   writes an `audit_outbox` row in the same transaction (ADR-017); the single
   background chainer later drains that into the tamper-evident `audit_log`. The
   seeder inserted transactions and postings but wrote nothing to the outbox, so
   every seeded transaction was invisible in the audit view, and the audit chain
   was blank for the prefilled data a visitor sees first. Live posts had a chain;
   seeded posts did not. An inconsistency in the one feature the demo is meant to
   showcase.

2. **Visitor-created tenants accumulated forever.** In demo mode the admin panel
   is public (ADR-019), so any visitor can create tenants. The reset only ever
   touches the three fixed demo tenants (the personal budget, plus a bank and a
   company on fixed ids). Nothing removed the tenants visitors created, so they
   piled up across resets, and the safe-by-default api-key guard (ADR-015)
   actively refused to wipe any tenant holding its own key, which visitor tenants
   do.

## Decision

### 1. The seeder emits the same audit event a live post does

For every seeded transaction the seeder inserts one `transaction.created`
`audit_outbox` row, in the same seed transaction that writes the postings. Its
`occurred_at` is the transaction's backdated time, which the chainer copies into
`audit_log.created_at`, so the audit row lines up with the transaction it
records. The `after` snapshot mirrors the shape the service's `auditSnapshot`
produces (the transaction id plus a postings array of account, amount, currency,
description), so the console renders amount and account for seeded rows exactly
as it does for live ones.

The chainer then builds the chain for seeded data through the ordinary path. No
new code path hashes or chains anything; the seeder only feeds the existing
outbox. Seeded data is now indistinguishable from live data in the audit view
and verifies as one continuous chain.

### 2. The demo reset purges every non-demo tenant, deliberately bypassing the ADR-015 guard

Each reset now deletes every tenant that is not one of the three demo ids, along
with all of its data, before reseeding the demo tenants. The delete runs in one
transaction across every tenant-scoped table in foreign-key-safe order (rows
that reference `transactions` first, then `transactions`, then `accounts`, then
the independent tables, then the `tenants` row last), with the append-only
`audit_log` cleared under the same `audit.allow_purge` GUC the single-tenant
reset already uses. The nullable-`tenant_id` FX tables keep their global rows.

This purge does **not** honor the ADR-015 api-key guard. That guard exists so a
misconfigured `DEFAULT_TENANT_ID` pointed at a real tenant can never be wiped by
a demo reset. But visitor tenants hold their own api keys, and removing them is
the entire point here, so the guard would defeat the feature. The safety instead
lives at the call site: the purge runs only from `runSeeder`, which starts only
when `DEMO_MODE` and `SEED_ENABLED` are both on. It is never called from any
non-demo path, and it refuses to run with an empty keep set (which would match
every tenant), as a backstop against wiping the whole table by mistake.

## Consequences

- The audit view and chain now cover seeded data, so the demo's headline feature
  is consistent from the first page load, with no special-casing in the reader.
- The public demo stays clean: visitor-created tenants live at most until the
  next reset (default hourly, see `SEED_INTERVAL`), then vanish with all their
  data.
- The demo reset is now genuinely destructive to any tenant outside the demo
  set. Its blast radius is bounded entirely by the `DEMO_MODE + SEED_ENABLED`
  gate and the fixed keep set; a production deployment (demo mode off) never
  reaches the purge.
- The single-tenant `Seed` path and its api-key guard are unchanged. The relaxed
  guard applies only to the new whole-database purge.

## Alternatives considered

- **Write `audit_log` rows directly from the seeder.** Rejected: it would
  duplicate the chainer's per-tenant hashing and ordering logic (ADR-012,
  ADR-017) in a second place that could drift. Feeding the outbox reuses the one
  real implementation.
- **Purge with a grace period (delete only tenants older than one interval).**
  Rejected as needless complexity for a demo that resets frequently; a visitor's
  tenant surviving one extra cycle has no value.
- **Keep the api-key guard and skip tenants that hold keys.** Rejected: it would
  make the purge a no-op for exactly the tenants it exists to remove, since every
  visitor tenant created through the admin panel has a key.
- **Leave visitor tenants in place and just document it.** Rejected: the demo is
  a shared, public surface, and an ever-growing tenant list degrades it for the
  next visitor.

## Out of scope

- Any change to the production service path. Both decisions are demo-tooling
  only, gated behind demo mode.
- Per-tenant deletion as a first-class API operation. The purge is a bulk
  demo-reset primitive, not a `DELETE /v1/tenants/{id}` endpoint.

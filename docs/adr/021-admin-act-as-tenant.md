# ADR-021: Admin Act-as-Tenant for Cross-Tenant Operator Access

This ADR records letting an admin-scoped API key operate against a tenant other
than the one its key belongs to, by sending an `X-Act-As-Tenant` header, so the
operator console's tenant switcher can actually isolate the ledger and reporting
views to the selected tenant. It builds on ADR-012 (auth and scopes), ADR-019
(the operator console), and the row-level security added during the remediation.

## Status

Accepted: 2026-07-12

## Context

Every `/v1` request resolves its tenant from the API key, not from any request
field. The auth middleware hashes the bearer token, looks up the key, and puts
the key's `tenant_id` into the request context; `tenantFromCtx` reads only that,
and the write path sets the `app.tenant_id` GUC from it, which row-level security
then enforces. This is the right default: the tenant comes from the credential,
so a caller can never read or write another tenant's data by lying in a body or
query.

The operator console (ADR-019) has a tenant switcher in its top bar. It works for
the admin panels (keys, webhooks, policy) because those endpoints take an explicit
`tenant_id`. It does nothing for the ledger and reporting views (accounts,
transactions, balances, statements, trial balance, disputes), because those
endpoints take no tenant parameter at all: they are bound to the key's tenant. So
an operator using the console with one admin key selects "tenant B" and still sees
tenant A's accounts and transactions. The switcher looks broken.

The important observation is that an admin key is already effectively
platform-level. The `/v1/admin` surface lets any admin key create tenants, issue a
key for any tenant, and manage every tenant's policy and webhooks. An operator who
wants to read tenant B's ledger can already do it in two steps: issue a read key
for B via `POST /v1/admin/keys`, then call the ledger endpoints with that key. So
cross-tenant access is not a new capability. What is missing is a one-step,
ergonomic way to do the same thing the console can drive from its switcher.

## Decision

### 1. An admin key may act as another tenant via a request header

A `/v1` request may carry `X-Act-As-Tenant: <tenant uuid>`. The auth middleware,
after it resolves the key and checks scope, computes the effective tenant:

- If the header is present and non-empty AND the resolved key has the `admin`
  scope, the effective tenant is the header value.
- Otherwise the effective tenant is the key's own tenant, exactly as before.

The effective tenant is what goes into the request context, so `tenantFromCtx`,
the `app.tenant_id` GUC, and row-level security all use it. Every `/v1` endpoint,
read and write, then operates on the acted-as tenant with no per-endpoint change.

A malformed header (not a uuid) is a 400, so a typo fails loudly rather than
silently reading an empty tenant. A non-admin key that sends the header is ignored
and stays scoped to its own tenant: the gate fails safe to less access, never
more.

### 2. Read and write both, not read-only

The override applies uniformly to all `/v1` operations, including posting
transactions and creating accounts, not just reads. This matches what an operator
console is for (select a tenant, then operate on it) and avoids a fragile
per-endpoint carve-out. It grants no capability an admin key did not already have
through the two-step key-issuing path, so the blast radius is unchanged: an admin
key was always able to write to any tenant by first minting a key for it.

The `/v1/admin` endpoints are unaffected in practice: they target the tenant named
in their own `tenant_id` body or query, not the context tenant, so overriding the
context tenant does not change what they act on.

### 3. The acted-as tenant is not re-gated on status

The resolver already refuses a key whose OWN tenant is suspended or closed. The
act-as override does not re-check the acted-as tenant's status, because an operator
legitimately needs to view and act on a suspended or closed tenant (that is often
exactly why they are looking). Row-level security still isolates the data to that
tenant; a non-existent tenant id simply resolves to an empty result set rather than
an error, which is acceptable for a browse action.

### 4. The console uses one selected tenant for everything

The console's top-bar switcher now drives a single selected tenant that scopes both
the admin panels (via `tenant_id`) and the ledger and reporting views (via
`X-Act-As-Tenant`, sent on every `/v1` call when a tenant is selected). Switching
tenant re-renders the current view, so the whole console isolates to the selected
tenant at once. In demo mode the shared demo key is admin-scoped, so the switcher
works for an anonymous demo visitor; in production it works when the operator has
entered an admin key, and is inert (own tenant only) for a non-admin key.

## Consequences

- The console's tenant switcher isolates the ledger and reporting views, not just
  the admin panels. Selecting a tenant shows that tenant's accounts, transactions,
  balances, and reports.
- The tenant an operator acts on is now explicit in the request (the header) and in
  the access log (the effective tenant is logged), so "who did what, as which
  tenant" stays reconstructible.
- Cross-tenant access is gated on the `admin` scope in exactly one place (the auth
  middleware), rather than spread across endpoints, so the security-relevant
  decision is easy to audit.
- Row-level security remains the backstop: even the acted-as path sets the GUC and
  goes through the same policies, so a bug in the override cannot leak across
  tenants past RLS.

## Alternatives considered

- **A per-tenant key issued by the console on switch.** The console would mint a
  read key for the selected tenant and use it for ledger calls. Rejected: it
  creates keys as a side effect of browsing, leans on the rate-limited demo key to
  do the issuing, and litters each tenant with console-created keys. The header is
  stateless and leaves no residue.
- **A `tenant_id` query parameter on every read endpoint.** Rejected: it changes
  every operation's schema, is easy to apply inconsistently, and puts the
  cross-tenant decision in each handler instead of one gate. A single header
  handled in the middleware is one code path to reason about.
- **Honest UI, no switching.** Label the switcher admin-only and tell operators to
  paste a tenant's own key to browse it. Rejected: it is accurate to the old model
  but gives up the switching an operator obviously wants, and the two-step key path
  it points at is strictly more work than the header.

## Security note

The override is deliberately gated on the `admin` scope, which in this system is
already platform-level (it manages every tenant through `/v1/admin`). There is
still no platform-superadmin tier distinct from a tenant admin (see ADR-020's
security note), so any admin key can act as any tenant. For the single-operator v1
that is acceptable and matches the existing admin power. A future multi-operator
deployment that wanted per-operator tenant scoping would gate the act-as override
on a narrower, explicitly-granted set of tenants rather than on the blanket admin
scope; that is out of scope here and recorded so the boundary is deliberate.

## Out of scope (v1)

- Restricting which tenants a given admin key may act as (per-operator scoping).
- Re-checking the acted-as tenant's active status on the override path.
- An audit-log field distinguishing "acted as" from "own tenant" beyond the
  effective tenant already recorded.

# ADR-020: FX rates and markup as live admin config

Status: Accepted
Date: 2026-07-11

This ADR records moving FX rate configuration from a boot-time environment
variable to a live admin API, and adding a per-tenant and global markup default
that a conversion falls back to when a rate carries no explicit spread. It builds
directly on ADR-014 (multi-currency and FX) and reuses its append-only rate
history and provider seam.

## Context

ADR-014 introduced FX conversion. Rates live in an append-only `fx_rates` table:
each row is a full quote for a directed pair (base to quote), carrying a mid rate
(`mid_rate_e8`, integer hundred-millionths of a quote unit per base unit) and a
spread (`spread_bps`, basis points widened against the customer). The only way to
put a rate in that table is the `FX_RATES` environment variable, parsed once at
boot by `fx.Seed`, which inserts one row per entry. A provider (`fx.Provider`)
resolves the current rate for a conversion: it prefers a tenant-specific row over
the global default (`tenant_id IS NULL`, added in migration 0014) and derives the
inverse pair by inverting the mid when only the reverse direction is stored.

Two gaps follow from that design. First, an operator cannot change a rate without
editing the box's environment and restarting the service, which is slow, needs
shell access, and is invisible to anyone using the API or console. Second, the
spread is the operator's markup, their revenue on a conversion, but it is welded
to each rate row: to run "half a percent on everything" an operator has to repeat
the same `spread_bps` on every pair, and to change it they must re-enter every
pair. There is no notion of a default markup that applies across pairs.

The console (ADR-019) is now an operator surface with admin panels, so live FX
config is the natural next step: an operator should set rates and markup from the
same place they manage tenants, keys, webhooks, and policy.

## Decision

### 1. Rates and markup are set through the admin API, not only the environment

A new admin surface under `/v1/admin/fx` writes to the FX tables, using the same
`admin` scope as the rest of the operator API:

- `POST /v1/admin/fx/rates` inserts a rate row for a directed pair.
- `GET /v1/admin/fx/rates` returns the current effective rates.
- `POST /v1/admin/fx/markup` inserts a markup-default row.
- `GET /v1/admin/fx/markup` returns the current markup defaults.

`FX_RATES` is kept as a boot-time bootstrap. `fx.Seed` still runs at every start
and appends any changed rows, so a fresh deploy comes up with sane defaults and an
operator can still pin rates in config if they prefer. The API is purely
additive: it appends more rows to the same append-only history, it does not
replace the env path. This is why keeping the env var costs nothing and removing
it would only take a working fresh-deploy default away.

### 2. Markup gets a precedence chain, resolved at conversion time

The markup is no longer forced onto every rate row. `fx_rates.spread_bps` becomes
nullable: a non-null value is a per-pair override, and null means "use the
applicable default markup." A new append-only table, `fx_markup_defaults`, holds
one default per scope: `(id, tenant_id NULL for global, default_spread_bps,
source, effective_at, created_at)`, mirroring `fx_rates` exactly (server-stamped
`effective_at`, a `[0, 10000)` check on the bps, a current-row index).

When a conversion runs, the effective spread is resolved in this order:

1. the pair's `fx_rates.spread_bps`, if non-null (per-pair override);
2. the tenant's current `fx_markup_defaults` row;
3. the global `fx_markup_defaults` row (`tenant_id IS NULL`);
4. zero, if nothing above is set.

Resolution happens at conversion time, not at rate-insert time. That is the
central choice here. The alternative, freezing the default into each rate row when
the row is written, is simpler (no nullable column, no second lookup) but it makes
a standalone markup page a lie: setting a new default would change nothing until
the operator re-entered every rate. Convert-time resolution means "set 50 bps as
the default" immediately governs every pair that does not override it, which is
the whole point of a default.

Reproducibility is preserved the same way ADR-014 preserves it. `Convert` still
takes a single concrete `spreadBps`, and `FXDetail` still snapshots the exact mid
and spread a conversion used. The precedence chain only decides which concrete
spread is handed to `Convert`; once a conversion is recorded, later changes to a
default cannot rewrite what that conversion did. Mid-rate precedence (tenant row,
then global, then inverse) is unchanged.

### 3. Both rates and markup are settable at global and per-tenant scope

The API takes a scope: a global write leaves `tenant_id` null, a tenant write
sets it. This mirrors the resolution the provider already does (a tenant row wins
over the global default) rather than inventing a new model. The console exposes
this with the admin-tenant selector it already has, plus a Global/Tenant toggle,
so the same page sets a platform default and a specific tenant's override.

### 4. The console gets FX config in Settings

Two admin-gated cards on the console Settings view, alongside the existing admin
panels:

- **Exchange rates**: the current pairs with their mid and effective spread, and
  a form to add or update a pair (base, quote, mid rate as a decimal, an optional
  spread override). The console converts the decimal mid to `mid_rate_e8` for the
  API and back for display; no float ever reaches the money path, the API still
  takes and stores integers.
- **Markup fee**: the current global and tenant defaults, and a form to set a
  default in basis points (shown with a percent hint).

The console stays a thin client: it only calls `/v1/admin/fx`, holds no FX logic,
and the server remains the gate.

## Consequences

- An operator changes rates and markup live, from the API or console, with no
  redeploy and no shell access. The change is an append to history, so the full
  trail of what a rate was and when is preserved, exactly as ADR-014 intended.
- A default markup is set once and applies across pairs. Per-pair overrides still
  work for the pairs that need a different number.
- `fx_rates.spread_bps` is now nullable. Every pre-existing row keeps its concrete
  value and is treated as an explicit override, so no historical conversion
  changes meaning. New rows written without a spread store null and pick up the
  default.
- Conversions do a second small lookup (the markup default) only when a rate row's
  spread is null. The current-row lookups are single-row indexed reads on
  append-only tables, the same shape as the existing rate lookup.

## Alternatives considered

- **Keep FX in the environment only.** Rejected: it was the problem. No live
  update, needs shell access, invisible to API and console users.
- **Freeze the default markup into each rate row at insert time.** Rejected as the
  primary model (see decision 2): it defeats a standalone markup default. Kept only
  as the mental model for a per-pair override, which is exactly a frozen spread on
  one row.
- **A separate markup table keyed per pair.** Rejected as over-built: the default
  is meant to span pairs, and per-pair precision is already available through the
  nullable `spread_bps` override on the rate row itself.
- **Mutable rate rows (UPDATE in place).** Rejected: it breaks ADR-014's
  append-only history and the immutable provenance that FX conversions snapshot.

## Security note

The admin FX endpoints reuse the existing `admin` scope. There is no
platform-superadmin tier distinct from a tenant admin, so any admin key can write
global rates and the global markup default. For the single-operator v1 deployment
(one operator running the whole system) that is acceptable, and it matches how the
other global-ish admin actions already behave. A future multi-operator deployment
would want a distinct platform-admin scope for the global writes; that is out of
scope here and recorded so the gap is deliberate, not forgotten.

## Out of scope (v1)

- A platform-superadmin scope separate from tenant admin (see the security note).
- Live external rate feeds. `source` still records provenance ("env" or "api"), so
  a future feed slots in as another source without a schema change.
- Scheduling a rate or markup to take effect in the future. `effective_at` is
  server-stamped at write time; future-dating is possible in the schema but not
  exposed by the API here.

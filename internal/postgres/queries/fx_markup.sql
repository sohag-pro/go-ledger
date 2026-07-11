-- name: InsertFXMarkupDefault :one
-- fx_markup_defaults is append-only (ADR-020): a new default is a new row.
-- tenant_id NULL is the global default, a non-NULL tenant_id is that tenant's
-- override. effective_at is server-stamped via COALESCE(narg, now()) for the
-- same clock-skew reason InsertFXRate stamps server-side.
INSERT INTO fx_markup_defaults (tenant_id, default_spread_bps, source, effective_at)
VALUES ($1, $2, $3, COALESCE(sqlc.narg('effective_at')::timestamptz, now()))
RETURNING *;

-- name: CurrentFXMarkupDefault :one
-- The default markup for a conversion: the tenant's own default if it has
-- one, else the global default. tenant_id = $1 matches the tenant's rows,
-- tenant_id IS NULL always matches the global, and ORDER BY (tenant_id IS
-- NULL) sorts the tenant tier ahead of global, then latest effective.
SELECT id, tenant_id, default_spread_bps, source, effective_at, created_at
FROM fx_markup_defaults
WHERE (tenant_id = $1 OR tenant_id IS NULL)
  AND effective_at <= now()
ORDER BY (tenant_id IS NULL), effective_at DESC, id DESC
LIMIT 1;

-- name: GlobalFXMarkupDefault :one
-- The current global default only (tenant_id IS NULL), so the console can
-- show it distinctly from a tenant override.
SELECT id, tenant_id, default_spread_bps, source, effective_at, created_at
FROM fx_markup_defaults
WHERE tenant_id IS NULL AND effective_at <= now()
ORDER BY effective_at DESC, id DESC
LIMIT 1;

-- name: TenantFXMarkupDefault :one
-- The current default owned by exactly this tenant (no global fallback), so
-- the console can show whether the tenant has its own override at all.
SELECT id, tenant_id, default_spread_bps, source, effective_at, created_at
FROM fx_markup_defaults
WHERE tenant_id = $1 AND effective_at <= now()
ORDER BY effective_at DESC, id DESC
LIMIT 1;

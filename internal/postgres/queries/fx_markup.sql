-- name: InsertFXMarkupDefault :one
-- fx_markup_defaults is append-only (ADR-020): a new default is a new row.
-- tenant_id NULL is the global default, a non-NULL tenant_id is that tenant's
-- override. effective_at is server-stamped via COALESCE(narg, now()) for the
-- same clock-skew reason InsertFXRate stamps server-side. default_spread_bps
-- is a nullable narg: a NULL value inserts a cleared row (the tenant follows
-- the global default again), matching the nullable fx_rates.spread_bps
-- pattern.
INSERT INTO fx_markup_defaults (tenant_id, default_spread_bps, source, effective_at)
VALUES ($1, sqlc.narg('default_spread_bps')::integer, $2, COALESCE(sqlc.narg('effective_at')::timestamptz, now()))
RETURNING *;

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

-- name: InsertFXRate :one
-- fx_rates is append-only (ADR-014): a new quote is a new row, never an
-- update, so every rate ever applied to a transaction stays reconstructible.
-- tenant_id NULL makes this the global default rate for the pair; a non-NULL
-- tenant_id makes it that tenant's own rate (Task 2.4, audit A3.3), resolved
-- ahead of the global default by CurrentFXRate below.
--
-- effective_at is a nullable named param (sqlc.narg): when the caller omits
-- an explicit effective_at, COALESCE falls through to the DATABASE SERVER's
-- now(), never the calling process's clock. Stamping an "immediate" rate
-- with the caller's clock is what caused a real clock-skew bug (Task 2.4
-- remediation): CurrentFXRate below gates on "effective_at <= now()" using
-- the server's clock too, so if the caller's clock ran even slightly ahead,
-- a just-inserted row was transiently invisible and the query silently fell
-- through to the global default. Passing NULL here (rather than the CLI
-- host's time.Now()) makes the two clocks the same clock for the common,
-- unscheduled case; an explicit future effective_at (a scheduled rate) is
-- unaffected, since the caller still supplies it directly.
INSERT INTO fx_rates (tenant_id, base, quote, mid_rate_e8, spread_bps, source, effective_at)
VALUES ($1, $2, $3, $4, sqlc.narg('spread_bps')::integer, $5, COALESCE(sqlc.narg('effective_at')::timestamptz, now()))
RETURNING *;

-- name: CurrentFXRate :one
-- The latest quote for (base, quote) at or before now, preferring a
-- tenant-specific row over the global default (Task 2.4, audit A3.3):
-- tenant_id = $1 matches the tenant's own rows, and tenant_id IS NULL always
-- matches the global default, so a tenant with no row of its own still
-- resolves the global one. ORDER BY (tenant_id IS NULL) sorts a tenant-owned
-- row (false, i.e. 0) ahead of a global row (true, i.e. 1), so when both
-- exist the tenant's own wins regardless of which is more recently
-- effective; effective_at DESC, id DESC is then the same "latest, ties
-- broken by insertion order" tiebreak CurrentFXRate always used, applied
-- within whichever tier (tenant-owned or global) won.
SELECT *
FROM fx_rates
WHERE base = $2 AND quote = $3
  AND (tenant_id = $1 OR tenant_id IS NULL)
  AND effective_at <= now()
ORDER BY (tenant_id IS NULL), effective_at DESC, id DESC
LIMIT 1;

-- name: LatestEnvGlobalFXRate :one
-- The latest env-seeded global row for a pair, used by fx.Seed to decide
-- whether FX_RATES itself changed. Comparing against this (not the current
-- winner) means an admin-API-written row is never clobbered by a re-seed:
-- Seed only re-asserts an env rate when the FX_RATES entry differs from the
-- last thing Seed itself wrote for that pair.
SELECT id, tenant_id, base, quote, mid_rate_e8, spread_bps, source, effective_at, created_at
FROM fx_rates
WHERE base = $1 AND quote = $2 AND tenant_id IS NULL AND source = 'env' AND effective_at <= now()
ORDER BY effective_at DESC, id DESC
LIMIT 1;

-- name: ListCurrentFXRates :many
-- The current effective row per (base, quote) for a tenant plus the global
-- defaults: DISTINCT ON collapses each pair to one row, and the ORDER BY puts
-- a tenant-owned row (tenant_id IS NULL = false, sorts first) ahead of a
-- global row, then latest effective, matching CurrentFXRate's precedence.
-- Pass tenant_id NULL to list globals only (tenant_id = NULL is never true, so
-- only the tenant_id IS NULL rows match).
SELECT DISTINCT ON (base, quote)
    id, tenant_id, base, quote, mid_rate_e8, spread_bps, source, effective_at, created_at
FROM fx_rates
WHERE (tenant_id = $1 OR tenant_id IS NULL)
  AND effective_at <= now()
ORDER BY base, quote, (tenant_id IS NULL), effective_at DESC, id DESC;

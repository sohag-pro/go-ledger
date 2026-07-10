-- name: InsertFXRate :one
-- fx_rates is append-only (ADR-014): a new quote is a new row, never an
-- update, so every rate ever applied to a transaction stays reconstructible.
-- tenant_id NULL makes this the global default rate for the pair; a non-NULL
-- tenant_id makes it that tenant's own rate (Task 2.4, audit A3.3), resolved
-- ahead of the global default by CurrentFXRate below.
INSERT INTO fx_rates (tenant_id, base, quote, mid_rate_e8, spread_bps, source, effective_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
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

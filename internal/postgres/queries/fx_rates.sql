-- name: InsertFXRate :one
-- fx_rates is append-only (ADR-014): a new quote is a new row, never an
-- update, so every rate ever applied to a transaction stays reconstructible.
INSERT INTO fx_rates (base, quote, mid_rate_e8, spread_bps, source, effective_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: CurrentFXRate :one
-- The latest quote for (base, quote) at or before now. id DESC is the
-- deterministic tiebreaker when two rows share the same effective_at (for
-- example a re-seed within the same second), so "current" always resolves to
-- exactly one row.
SELECT *
FROM fx_rates
WHERE base = $1 AND quote = $2 AND effective_at <= now()
ORDER BY effective_at DESC, id DESC
LIMIT 1;

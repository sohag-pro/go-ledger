-- name: CreateTransaction :exec
-- currency lives on each posting now (ADR-014), not here: an FX transaction
-- spans two currencies, so there is no single transaction-level value left to
-- store. The fx_* columns are the immutable snapshot of the conversion
-- actually applied; all nullable, since a single-currency transaction (still
-- the common case) has none of this. reverses_transaction_id (Task 4.2,
-- audit A1.2) is likewise nullable: only a reversal carries it.
INSERT INTO transactions (
    id, tenant_id,
    fx_source_amount, fx_converted_amount, fx_mid_rate_e8, fx_spread_bps,
    fx_applied_e8, fx_rate_source, fx_effective_at, fx_rate_id,
    reverses_transaction_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: GetTransaction :one
SELECT id, tenant_id, created_at,
       fx_source_amount, fx_converted_amount, fx_mid_rate_e8, fx_spread_bps,
       fx_applied_e8, fx_rate_source, fx_effective_at, fx_rate_id,
       reverses_transaction_id
FROM transactions
WHERE tenant_id = $1 AND id = $2;

-- name: GetReversalOf :one
-- The reversal of a given original, if one exists (Task 4.2, audit A1.2):
-- transactions_one_reversal_idx (migration 0017) guarantees at most one row
-- can ever match, so this is a plain :one lookup, not a list.
SELECT id, tenant_id, created_at,
       fx_source_amount, fx_converted_amount, fx_mid_rate_e8, fx_spread_bps,
       fx_applied_e8, fx_rate_source, fx_effective_at, fx_rate_id,
       reverses_transaction_id
FROM transactions
WHERE tenant_id = $1 AND reverses_transaction_id = $2;

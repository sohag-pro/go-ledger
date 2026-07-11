-- name: CreateTransaction :one
-- currency lives on each posting now (ADR-014), not here: an FX transaction
-- spans two currencies, so there is no single transaction-level value left to
-- store. The fx_* columns are the immutable snapshot of the conversion
-- actually applied; all nullable, since a single-currency transaction (still
-- the common case) has none of this. reverses_transaction_id (Task 4.2,
-- audit A1.2) is likewise nullable: only a reversal carries it. reference and
-- effective_at (Task 4.3, audit A1.3) are both nullable client-supplied
-- fields. created_at is RETURNED (not just inserted): the caller uses it to
-- resolve effective_at's read-time fallback right after a fresh insert,
-- without a second round trip (see Repository.txRepo.CreateTransaction).
INSERT INTO transactions (
    id, tenant_id,
    fx_source_amount, fx_converted_amount, fx_mid_rate_e8, fx_spread_bps,
    fx_applied_e8, fx_rate_source, fx_effective_at, fx_rate_id,
    reverses_transaction_id, reference, effective_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING created_at;

-- name: GetTransaction :one
SELECT id, tenant_id, created_at,
       fx_source_amount, fx_converted_amount, fx_mid_rate_e8, fx_spread_bps,
       fx_applied_e8, fx_rate_source, fx_effective_at, fx_rate_id,
       reverses_transaction_id, reference, effective_at
FROM transactions
WHERE tenant_id = $1 AND id = $2;

-- name: ListTransactions :many
-- Filtered, keyset-paged list of a tenant's transactions, newest first (Task
-- 4.4, audit A7.2). from_ts/to_ts/reference are optional filters via
-- sqlc.narg: NULL disables that filter's clause (it becomes a no-op OR),
-- so this single query serves every filter combination rather than one
-- built dynamically per request. from_ts is inclusive (created_at >=),
-- to_ts is exclusive (created_at <): a half-open [from, to) window.
--
-- Keyset paged by (created_at, id) descending, the identical cursor shape
-- AccountStatement already uses: after_created_at/after_id are the keyset
-- position, a far-future timestamp and the max uuid for the first page.
-- page_limit is requested by the caller as one more than the page size it
-- actually wants, so a next page can be detected without a second round
-- trip; this query itself just returns up to page_limit rows in whatever
-- amount it is asked for.
SELECT id, tenant_id, created_at,
       fx_source_amount, fx_converted_amount, fx_mid_rate_e8, fx_spread_bps,
       fx_applied_e8, fx_rate_source, fx_effective_at, fx_rate_id,
       reverses_transaction_id, reference, effective_at
FROM transactions
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg('from_ts')::timestamptz IS NULL OR created_at >= sqlc.narg('from_ts'))
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR created_at < sqlc.narg('to_ts'))
  AND (sqlc.narg('reference')::text IS NULL OR reference = sqlc.narg('reference'))
  AND (created_at < sqlc.arg(after_created_at)
       OR (created_at = sqlc.arg(after_created_at) AND id < sqlc.arg(after_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: GetReversalOf :one
-- The reversal of a given original, if one exists (Task 4.2, audit A1.2):
-- transactions_one_reversal_idx (migration 0017) guarantees at most one row
-- can ever match, so this is a plain :one lookup, not a list.
SELECT id, tenant_id, created_at,
       fx_source_amount, fx_converted_amount, fx_mid_rate_e8, fx_spread_bps,
       fx_applied_e8, fx_rate_source, fx_effective_at, fx_rate_id,
       reverses_transaction_id, reference, effective_at
FROM transactions
WHERE tenant_id = $1 AND reverses_transaction_id = $2;

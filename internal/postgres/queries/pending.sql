-- name: InsertPendingTransaction :exec
-- Task 4 (ADR-025): status is NOT inserted explicitly (the column default
-- 'pending' applies), the same convention CreateDispute leaves status to its
-- own column default for.
INSERT INTO pending_transactions (id, tenant_id, kind, payload, threshold_ccy, threshold_amt, created_by)
VALUES (sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(kind), sqlc.arg(payload), sqlc.arg(threshold_ccy), sqlc.arg(threshold_amt), sqlc.arg(created_by));

-- name: GetPendingTransaction :one
SELECT id, tenant_id, kind, payload, status, threshold_ccy, threshold_amt, created_by, created_at, decided_by, decided_at, reason, transaction_id
FROM pending_transactions
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id);

-- name: GetPendingForUpdate :one
-- Task 4 (ADR-025): the row-locking read a decision (approve/reject/cancel)
-- takes before transitioning a pending, so two racing decisions cannot both
-- win; the loser's transaction blocks on this lock until the winner commits
-- or rolls back, then re-reads the now-terminal row. Called only from
-- inside RunInTx (via the Tx port), never as a standalone read.
SELECT id, tenant_id, kind, payload, status, threshold_ccy, threshold_amt, created_by, created_at, decided_by, decided_at, reason, transaction_id
FROM pending_transactions
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id)
FOR UPDATE;

-- name: ListPendingTransactions :many
-- Filtered, keyset-paged list of a tenant's pendings, newest first (Task 4,
-- ADR-025). status is an optional filter via sqlc.narg: NULL disables it
-- (every status is returned). Keyset paged by (created_at, id) descending,
-- the identical cursor shape ListDisputes already uses.
SELECT id, tenant_id, kind, payload, status, threshold_ccy, threshold_amt, created_by, created_at, decided_by, decided_at, reason, transaction_id
FROM pending_transactions
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (created_at < sqlc.arg(after_created_at)
       OR (created_at = sqlc.arg(after_created_at) AND id < sqlc.arg(after_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: UpdatePendingStatus :exec
-- Task 4 (ADR-025): the decision write. Always called after
-- GetPendingForUpdate has already locked and validated the row within the
-- same surrounding transaction, so this is a plain unconditional update, not
-- a guarded one the way ResolveDispute's UPDATE ... WHERE status = 'open'
-- is: the row lock is what prevents a second concurrent decision here, not
-- a WHERE clause on this statement.
UPDATE pending_transactions
SET status = sqlc.arg(status), decided_by = sqlc.arg(decided_by), decided_at = now(),
    reason = sqlc.narg(reason), transaction_id = sqlc.narg(transaction_id)
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id);

-- name: SweepExpiredPending :many
-- Task 4 (ADR-025): the TTL sweep, mirroring
-- SweepExpiredIdempotencyKeysBatch's role for idempotency keys. Runs
-- outside any tenant's RunInTx (a background goroutine with no tenant GUC
-- set, so the RLS policy's allow-when-unset branch lets it see every
-- tenant), on an interval, moving every still-pending row older than
-- older_than_seconds to 'expired' and decided_by 'system'. RETURNING the
-- full rows lets the caller emit one approval.expired lifecycle event per
-- row expired, without a second read. older_than_seconds is a float8 of
-- seconds, not a Postgres interval literal, the same
-- server-clock-independent-duration convention InsertIdempotencyKey's
-- ttl_seconds already uses.
UPDATE pending_transactions
SET status = 'expired', decided_at = now(), decided_by = 'system'
WHERE status = 'pending'
  AND created_at < now() - (sqlc.arg(older_than_seconds)::float8 * interval '1 second')
RETURNING id, tenant_id, kind, payload, status, threshold_ccy, threshold_amt, created_by, created_at, decided_by, decided_at, reason, transaction_id;

-- name: CreateDispute :exec
-- Task 6.3, audit A9.2: status is NOT inserted explicitly (the column
-- default 'open' applies), the same convention CreateAccount leaves status
-- to its own column default for.
INSERT INTO disputes (id, tenant_id, transaction_id, reason)
VALUES ($1, $2, $3, $4);

-- name: GetDispute :one
SELECT id, tenant_id, transaction_id, status, reason, resolution_transaction_id, created_at, resolved_at
FROM disputes
WHERE tenant_id = $1 AND id = $2;

-- name: ListDisputes :many
-- Filtered, keyset-paged list of a tenant's disputes, newest first (Task
-- 6.3, audit A9.2). status is an optional filter via sqlc.narg: NULL
-- disables it (every status is returned). Keyset paged by (created_at, id)
-- descending, the identical cursor shape ListTransactions and
-- AccountStatement already use.
SELECT id, tenant_id, transaction_id, status, reason, resolution_transaction_id, created_at, resolved_at
FROM disputes
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (created_at < sqlc.arg(after_created_at)
       OR (created_at = sqlc.arg(after_created_at) AND id < sqlc.arg(after_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ResolveDispute :one
-- Task 6.3, audit A9.2: the guarded transition out of 'open'. The WHERE
-- status = 'open' clause is what makes resolving an already-resolved (or
-- concurrently-being-resolved) dispute return zero rows rather than
-- silently overwriting a prior resolution: the caller
-- (postgres.Repository.ResolveDispute) treats a zero-row result as
-- domain.ErrDisputeAlreadyResolved.
UPDATE disputes
SET status = $3, resolution_transaction_id = $4, resolved_at = now()
WHERE tenant_id = $1 AND id = $2 AND status = 'open'
RETURNING id, tenant_id, transaction_id, status, reason, resolution_transaction_id, created_at, resolved_at;

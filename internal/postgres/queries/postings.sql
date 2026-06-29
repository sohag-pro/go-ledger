-- name: CreatePosting :exec
INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount)
VALUES ($1, $2, $3, $4, $5);

-- name: ListPostingsByTransaction :many
SELECT id, tenant_id, transaction_id, account_id, amount, created_at
FROM postings
WHERE tenant_id = $1 AND transaction_id = $2
ORDER BY created_at, id;

-- name: AccountBalance :one
SELECT COALESCE(SUM(amount), 0)::bigint AS balance
FROM postings
WHERE tenant_id = $1 AND account_id = $2;

-- name: CreateTransaction :exec
INSERT INTO transactions (id, tenant_id, currency)
VALUES ($1, $2, $3);

-- name: GetTransaction :one
SELECT id, tenant_id, currency, created_at
FROM transactions
WHERE tenant_id = $1 AND id = $2;

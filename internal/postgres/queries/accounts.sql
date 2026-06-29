-- name: CreateAccount :exec
INSERT INTO accounts (id, tenant_id, name, type, currency)
VALUES ($1, $2, $3, $4, $5);

-- name: GetAccount :one
SELECT id, tenant_id, name, type, currency, created_at
FROM accounts
WHERE tenant_id = $1 AND id = $2;

-- name: ListAccounts :many
SELECT id, tenant_id, name, type, currency, created_at
FROM accounts
WHERE tenant_id = $1
ORDER BY name, id
LIMIT $2;

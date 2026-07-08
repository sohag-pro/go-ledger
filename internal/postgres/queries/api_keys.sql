-- name: InsertAPIKey :exec
INSERT INTO api_keys (id, tenant_id, name, key_hash, rate_limit_rpm)
VALUES ($1, $2, $3, $4, $5);

-- name: GetAPIKeyByHash :one
SELECT id, tenant_id, name, rate_limit_rpm, created_at, revoked_at
FROM api_keys
WHERE key_hash = $1 AND revoked_at IS NULL;

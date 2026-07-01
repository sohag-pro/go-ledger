-- name: InsertIdempotencyKey :exec
INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, transaction_id)
VALUES ($1, $2, $3, $4);

-- name: GetIdempotencyKey :one
SELECT tenant_id, idempotency_key, fingerprint, transaction_id, created_at
FROM idempotency_keys
WHERE tenant_id = $1 AND idempotency_key = $2;

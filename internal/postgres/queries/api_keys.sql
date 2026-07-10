-- name: InsertAPIKey :exec
INSERT INTO api_keys (id, tenant_id, name, key_hash, rate_limit_rpm)
VALUES ($1, $2, $3, $4, $5);

-- name: GetAPIKeyByHash :one
-- Joins tenants so the resolver gets the tenant's current status alongside
-- the key in the same round trip (Task 2.1, ADR-015): gating needs no extra
-- query. The join is safe against a dangling reference: api_keys_tenant_fk
-- (migration 0011) guarantees every api_keys row's tenant_id has a tenants
-- row.
SELECT api_keys.id, api_keys.tenant_id, api_keys.name, api_keys.rate_limit_rpm, api_keys.created_at, api_keys.revoked_at,
       tenants.status AS tenant_status
FROM api_keys
JOIN tenants ON tenants.id = api_keys.tenant_id
WHERE api_keys.key_hash = $1 AND api_keys.revoked_at IS NULL;

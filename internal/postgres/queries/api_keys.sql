-- name: InsertAPIKey :exec
INSERT INTO api_keys (id, tenant_id, name, key_hash, rate_limit_rpm)
VALUES ($1, $2, $3, $4, $5);

-- name: GetAPIKeyByHash :one
-- Joins tenants so the resolver gets the tenant's current status alongside
-- the key in the same round trip (Task 2.1, ADR-015): gating needs no extra
-- query. The join is safe against a dangling reference: api_keys_tenant_fk
-- (migration 0011) guarantees every api_keys row's tenant_id has a tenants
-- row. scopes, expires_at, and last_used_at (Task 2.2) are returned as-is:
-- expiry and scope enforcement happen in the resolver and the transport
-- middleware, not in this query.
SELECT api_keys.id, api_keys.tenant_id, api_keys.name, api_keys.rate_limit_rpm, api_keys.created_at, api_keys.revoked_at,
       api_keys.scopes, api_keys.expires_at, api_keys.last_used_at,
       tenants.status AS tenant_status
FROM api_keys
JOIN tenants ON tenants.id = api_keys.tenant_id
WHERE api_keys.key_hash = $1 AND api_keys.revoked_at IS NULL;

-- name: TouchAPIKeyLastUsed :exec
-- Updates last_used_at for a single key by id. Called best-effort and
-- throttled from the auth resolver (Task 2.2): not every request, so this is
-- not a write on the hot path of every authenticated call.
UPDATE api_keys SET last_used_at = $2 WHERE id = $1;

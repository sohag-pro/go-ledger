-- name: InsertAPIKey :exec
-- scopes and expires_at (Task 2.2b) are now written on insert: the admin
-- surface (internal/admin) is what actually sets them to something other
-- than the column default. Every pre-2.2b caller (cmd/server's demo and
-- load-test key provisioning, and every repository test that predates
-- scopes) still works unchanged, because the Go-level repository method
-- defaults an empty Scopes slice to {read,post} before it ever reaches this
-- query, the same default the api_keys.scopes column itself carries.
INSERT INTO api_keys (id, tenant_id, name, key_hash, rate_limit_rpm, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetAPIKeyByID :one
-- Raw fetch by id, unfiltered by revoked_at: the admin surface (Task 2.2b)
-- uses this to look up an existing key (including an already-revoked one) by
-- id, for example to copy a key's tenant/name/scopes when rotating it. It
-- does not join tenants: callers that need the tenant's current status look
-- it up separately via GetTenant.
SELECT id, tenant_id, name, rate_limit_rpm, created_at, revoked_at, scopes, expires_at, last_used_at
FROM api_keys
WHERE id = $1;

-- name: ListAPIKeysByTenant :many
-- Every key for a tenant, oldest first, including revoked ones: the admin
-- surface's list view (Task 2.2b) is meant to show a tenant's full key
-- history, not just its live keys. Never selects key_hash: plaintext is
-- shown once at issue/rotate time and is never recoverable afterward, and
-- the hash itself has no business leaving the resolver's lookup path.
SELECT id, tenant_id, name, rate_limit_rpm, created_at, revoked_at, scopes, expires_at, last_used_at
FROM api_keys
WHERE tenant_id = $1
ORDER BY created_at, id;

-- name: RevokeAPIKey :execrows
-- COALESCE makes this idempotent: revoking an already-revoked key keeps its
-- original revoked_at instead of bumping it, and still reports one row
-- affected (not zero), so the admin service can tell "no such key" (0 rows)
-- from "already revoked" (1 row, unchanged) without a separate lookup.
UPDATE api_keys SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1;

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

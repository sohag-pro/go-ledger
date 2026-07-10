-- name: CreateTenant :exec
INSERT INTO tenants (id, name) VALUES ($1, $2);

-- name: GetTenant :one
SELECT id, name, status, settings, created_at FROM tenants WHERE id = $1;

-- name: ListTenants :many
SELECT id, name, status, settings, created_at FROM tenants ORDER BY created_at, id LIMIT $1;

-- name: SetTenantStatus :execrows
UPDATE tenants SET status = $2 WHERE id = $1;

-- name: SetTenantSettings :execrows
-- Task 2.4b (audit A3.4): a whole-document replace of the settings jsonb
-- column, used by admin.Service.SetTenantPolicy to write {"policy": {...}}.
UPDATE tenants SET settings = $2 WHERE id = $1;

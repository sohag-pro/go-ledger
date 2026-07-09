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

-- name: GetOrCreateClearingAccount :one
-- The per-tenant per-currency FX clearing account (ADR-014, is_system=true),
-- created lazily on first use. Keyed by (tenant_id, name): name is the
-- reserved, deterministic "fx.clearing.<CURRENCY>" string the caller builds,
-- so a second call for the same tenant and currency always resolves to the
-- same row instead of creating a duplicate. The ON CONFLICT arbiter is the
-- partial unique index accounts_system_name_uniq (migration 0010), which only
-- covers is_system rows, so this can never collide with an ordinary
-- user-named account. ins does nothing on conflict (no RETURNING row); the
-- second branch then fetches the row that already existed, guarded by
-- NOT EXISTS so it only runs when ins produced nothing.
WITH ins AS (
    INSERT INTO accounts (id, tenant_id, name, type, currency, is_system)
    VALUES ($1, $2, $3, $4, $5, true)
    ON CONFLICT (tenant_id, name) WHERE is_system DO NOTHING
    RETURNING id, tenant_id, name, type, currency, is_system, created_at
)
SELECT id, tenant_id, name, type, currency, is_system, created_at FROM ins
UNION ALL
SELECT id, tenant_id, name, type, currency, is_system, created_at
FROM accounts
WHERE tenant_id = $2 AND name = $3 AND is_system
  AND NOT EXISTS (SELECT 1 FROM ins)
LIMIT 1;

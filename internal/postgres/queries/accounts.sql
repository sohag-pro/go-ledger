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
-- user-named account.
--
-- On conflict this does a no-op DO UPDATE (id set to its own current value)
-- rather than DO NOTHING. DO NOTHING never RETURNs the conflicting row, which
-- an earlier version of this query worked around with a CTE plus a fallback
-- SELECT unioned into the same statement, guarded by NOT EXISTS. That
-- fallback ran against the single statement's original snapshot: when two
-- callers raced to create the same tenant's first clearing account for a
-- currency, the loser blocked on the conflict, and by the time it unblocked
-- (after the winner committed) its own fallback SELECT could still miss the
-- now-committed row, since the snapshot predated that commit. Both branches
-- of the UNION then returned zero rows to the loser, surfacing as "no rows in
-- result set" under concurrent Converts targeting the same pair. DO UPDATE
-- instead forces Postgres's own EvalPlanQual re-fetch of the current row
-- version as part of resolving the conflict, so RETURNING always yields
-- exactly one row, new or existing, in a single round trip with no second
-- snapshot to race against.
INSERT INTO accounts (id, tenant_id, name, type, currency, is_system)
VALUES ($1, $2, $3, $4, $5, true)
ON CONFLICT (tenant_id, name) WHERE is_system
    DO UPDATE SET id = accounts.id
RETURNING id, tenant_id, name, type, currency, is_system, created_at;

-- name: CreateAccount :exec
-- min_balance (Task 5.5, audit A1.5) is nullable: sqlc.narg leaves it unset
-- (NULL) when the caller passes no value, matching "no floor configured",
-- every account's behavior before this column existed. status is NOT
-- inserted explicitly: the column default ('active', migration 0022)
-- applies, the same way CreateTenant leaves status to the column default.
INSERT INTO accounts (id, tenant_id, name, type, currency, min_balance)
VALUES ($1, $2, $3, $4, $5, sqlc.narg(min_balance));

-- name: GetAccount :one
SELECT id, tenant_id, name, type, currency, status, min_balance, is_system, created_at
FROM accounts
WHERE tenant_id = $1 AND id = $2;

-- name: ListAccounts :many
SELECT id, tenant_id, name, type, currency, status, min_balance, is_system, created_at
FROM accounts
WHERE tenant_id = $1
ORDER BY name, id
LIMIT $2;

-- name: SetAccountStatus :execrows
-- Task 5.5, audit A1.5: freezes, closes, or reactivates one account. Scoped
-- to tenant_id like every other write here, so a caller can never flip a
-- status on another tenant's account by id alone.
UPDATE accounts SET status = $3
WHERE tenant_id = $1 AND id = $2;

-- name: AccountStatusFlags :many
-- Task 5.5, audit A1.5: each named account's current status, min_balance,
-- and is_system flag ONLY (no balance), read inside the caller's
-- SERIALIZABLE RunInTx body (see domain.Tx.AccountPostingStates). This is
-- deliberately split from AccountBalances below: this query touches only the
-- accounts table, which nothing in the posting path ever writes to (a post
-- inserts into transactions/postings/audit_outbox/idempotency_keys, never
-- accounts), so it can never be the read side of a SERIALIZABLE read-write
-- antidependency against a concurrent post. That is exactly what a combined
-- query joining postings would risk: reading every historical posting for a
-- hot account inside the same transaction as a concurrent INSERT into that
-- account's postings is precisely the kind of broad read-write overlap
-- SERIALIZABLE flags, and doing it unconditionally on every single post
-- (unlike the opt-in TenantDailyDebits check) reintroduced, under many-way
-- single-tenant concurrency onto a handful of accounts, the same class of
-- retry storm ADR-017 removed the audit chain read to get rid of (see
-- TestPostConcurrentStressSingleTenant). AccountBalances is now only ever
-- called for the subset of accounts that actually have a MinBalance set.
SELECT id, status, min_balance, is_system
FROM accounts
WHERE tenant_id = sqlc.arg(tenant_id) AND id = ANY(sqlc.arg(account_ids)::uuid[]);

-- name: AccountBalances :many
-- Task 5.5, audit A1.5: each named account's derived balance, read inside
-- the caller's SERIALIZABLE RunInTx body so it is consistent with the
-- CreateTransaction write that follows in the same transaction: two
-- concurrent posts that would each individually keep an account above its
-- floor, but together breach it, are a genuine read-write antidependency
-- SERIALIZABLE detects and aborts one of. Called ONLY for accounts that
-- AccountStatusFlags reported as having a MinBalance configured (see
-- AccountStatusFlags's own doc comment for why this query is kept separate
-- and only run when actually needed). The LEFT JOIN (rather than a subquery
-- per account) means an account with no postings yet still returns one row,
-- with balance COALESCEd to 0.
SELECT a.id, COALESCE(SUM(p.amount), 0)::bigint AS balance
FROM accounts a
LEFT JOIN postings p ON p.tenant_id = a.tenant_id AND p.account_id = a.id
WHERE a.tenant_id = sqlc.arg(tenant_id) AND a.id = ANY(sqlc.arg(account_ids)::uuid[])
GROUP BY a.id;

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
RETURNING id, tenant_id, name, type, currency, status, min_balance, is_system, created_at;

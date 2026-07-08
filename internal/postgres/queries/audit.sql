-- name: InsertAuditLog :exec
INSERT INTO audit_log (id, tenant_id, action, transaction_id, actor, before, after, created_at, prev_hash, row_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: GetLastAuditHash :one
-- The tenant's most recent row_hash, used to extend the per-tenant hash chain.
-- A fresh tenant (or one with no rows yet) surfaces as pgx.ErrNoRows; the
-- caller treats that as the chain's genesis (domain.AuditGenesisHash).
SELECT row_hash FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListAuditByTransaction :many
SELECT id, tenant_id, action, transaction_id, actor, before, after, created_at, prev_hash, row_hash
FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id) AND transaction_id = sqlc.arg(transaction_id)
ORDER BY created_at, id;

-- name: ListAuditByAccount :many
-- Keyset page of audit rows for every transaction with a posting touching the
-- account, newest first. after_created_at / after_id are the keyset position:
-- pass a far-future timestamp and the max uuid for the first page.
SELECT audit_log.id, audit_log.tenant_id, audit_log.action, audit_log.transaction_id,
       audit_log.actor, audit_log.before, audit_log.after, audit_log.created_at,
       audit_log.prev_hash, audit_log.row_hash
FROM audit_log
WHERE audit_log.tenant_id = sqlc.arg(tenant_id)
  AND audit_log.transaction_id IN (
    SELECT DISTINCT postings.transaction_id
    FROM postings
    WHERE postings.tenant_id = sqlc.arg(tenant_id) AND postings.account_id = sqlc.arg(account_id)
  )
  AND (audit_log.created_at < sqlc.arg(after_created_at)
       OR (audit_log.created_at = sqlc.arg(after_created_at) AND audit_log.id < sqlc.arg(after_id)))
ORDER BY audit_log.created_at DESC, audit_log.id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListAuditForVerify :many
-- Every audit row for the tenant, oldest first: the full walk used to
-- recompute and check the tamper-evident hash chain end to end.
SELECT id, tenant_id, action, transaction_id, actor, before, after, created_at, prev_hash, row_hash
FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY created_at, id;

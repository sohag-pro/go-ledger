-- name: InsertAuditLog :exec
INSERT INTO audit_log (id, tenant_id, action, transaction_id, actor, before, after)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListAuditByTransaction :many
SELECT id, tenant_id, action, transaction_id, actor, before, after, created_at
FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id) AND transaction_id = sqlc.arg(transaction_id)
ORDER BY created_at, id;

-- name: ListAuditByAccount :many
-- Keyset page of audit rows for every transaction with a posting touching the
-- account, newest first. after_created_at / after_id are the keyset position:
-- pass a far-future timestamp and the max uuid for the first page.
SELECT audit_log.id, audit_log.tenant_id, audit_log.action, audit_log.transaction_id,
       audit_log.actor, audit_log.before, audit_log.after, audit_log.created_at
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

-- name: InsertAuditLog :exec
-- outbox_id is the source audit_outbox row this audit_log row was chained
-- from (ADR-017 MINOR 3, migration 0016): a UNIQUE constraint on it means a
-- second attempt to chain the same outbox row fails this insert with a
-- unique violation instead of silently forking the chain. chain_seq is left
-- to its column DEFAULT (nextval), never supplied by the caller: it is what
-- makes chain order immune to any host's clock (see GetLastAuditHash below).
INSERT INTO audit_log (id, tenant_id, action, transaction_id, actor, before, after, created_at, prev_hash, row_hash, outbox_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: GetLastAuditHash :one
-- The tenant's most recent row_hash, used to extend the per-tenant hash chain.
-- Ordered by chain_seq, not id (ADR-017 IMPORTANT 2, migration 0016): id is a
-- UUIDv7, monotonic only within the ONE process that minted it, so a leader
-- failover to a different host with clock skew can mint an id LOWER than the
-- current head, and ordering by id would then return the wrong "latest" row
-- and corrupt the chain. chain_seq is a plain ascending sequence the single
-- chainer process advances on every insert, in the same order it assigns
-- row_hash values, so it stays correct across any failover regardless of
-- clock skew. created_at is copied from the ORIGINATING event's post time
-- (audit_outbox.occurred_at), which under concurrent posts across many
-- transactions is NOT guaranteed to be monotonic with the order those
-- transactions actually commit in (a transaction that starts later can
-- commit first); ordering by created_at would occasionally return the wrong
-- "latest" row too. A fresh tenant (or one with no rows yet) surfaces as
-- pgx.ErrNoRows; the caller treats that as the chain's genesis
-- (domain.AuditGenesisHash).
SELECT row_hash FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY chain_seq DESC
LIMIT 1;

-- name: ListAuditByTransaction :many
-- Ordered by id, not created_at: see GetLastAuditHash's comment (ADR-017) for
-- why id is the chain-consistent order and created_at is not.
SELECT id, tenant_id, action, transaction_id, actor, before, after, created_at, prev_hash, row_hash
FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id) AND transaction_id = sqlc.arg(transaction_id)
ORDER BY id;

-- name: ListAuditByAccount :many
-- Keyset page of audit rows for every transaction with a posting touching the
-- account, newest first. after_id is the keyset position: pass the max uuid
-- for the first page. Ordered and paged by id alone, not (created_at, id):
-- see GetLastAuditHash's comment (ADR-017) for why id, assigned by the
-- chainer in true chain-insertion order, is what must drive ordering here,
-- not created_at (copied from the original event's post time, which is not
-- guaranteed monotonic with commit order under concurrent posts).
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
  AND audit_log.id < sqlc.arg(after_id)
ORDER BY audit_log.id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListAuditForVerify :many
-- Every audit row for the tenant, in true chain order: the full walk used to
-- recompute and check the tamper-evident hash chain end to end. Ordered by
-- chain_seq, not id or created_at: see GetLastAuditHash's comment (ADR-017,
-- migration 0016) for why chain_seq, not id, is the failover-safe chain
-- order.
SELECT id, tenant_id, action, transaction_id, actor, before, after, created_at, prev_hash, row_hash
FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY chain_seq;

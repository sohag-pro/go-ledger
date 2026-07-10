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

-- name: ListAuditForVerifyPage :many
-- A bounded page of the tenant's audit rows in true chain order, strictly
-- after after_chain_seq (Task 5.3, audit A2.4): the streaming counterpart to
-- ListAuditForVerify above, which loads the entire chain into memory at
-- once. AuditService.Verify calls this in a loop, advancing
-- after_chain_seq to the last row's chain_seq on each page, until a page
-- comes back with fewer than page_limit rows, so memory use is bounded by
-- page_limit regardless of how long the chain has grown. Includes
-- chain_seq (unlike ListAuditForVerify) precisely so the caller can advance
-- the cursor without a second round trip. after_chain_seq is 0 to start
-- from genesis, or an anchor's chain_seq to start from a trusted checkpoint
-- (VerifyFromLatestAnchor).
SELECT id, tenant_id, action, transaction_id, actor, before, after, created_at, prev_hash, row_hash, chain_seq
FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id) AND chain_seq > sqlc.arg(after_chain_seq)
ORDER BY chain_seq
LIMIT sqlc.arg(page_limit);

-- name: GetAuditHead :one
-- The tenant's current head: chain_seq and row_hash of its latest audit_log
-- row (Task 5.3). See GetLastAuditHash's own comment (ADR-017) for why
-- chain_seq, not id or created_at, is the correct "latest" order; this is
-- the same lookup, just also returning chain_seq, which GetLastAuditHash's
-- only caller (the chainer) never needed since it only ever extends the
-- chain from a row_hash. Used to surface the live head alongside the last
-- off-box anchor (the verify-audit-chain endpoint, internal/api/audit.go)
-- and by VerifyFromLatestAnchor to fall back to a full verify when no
-- anchor exists yet. ErrNoRows means the tenant has no audit rows at all.
SELECT chain_seq, row_hash FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY chain_seq DESC
LIMIT 1;

-- name: InsertAuditAnchor :exec
-- Records tenantID's current chain head as a new off-box-anchored
-- checkpoint (Task 5.3, migration 0025). Called only by the periodic
-- anchor job (internal/audit.AnchorJob), never the request path: the job
-- runs with the RLS GUC unset (a cross-tenant worker, Task 5.4b), so this
-- insert is not scoped through withTenant the way a request-path write
-- would be.
INSERT INTO audit_anchors (tenant_id, chain_seq, row_hash)
VALUES (sqlc.arg(tenant_id), sqlc.arg(chain_seq), sqlc.arg(row_hash));

-- name: GetLatestAuditAnchor :one
-- The tenant's most recently recorded anchor (Task 5.3): the chain_seq,
-- row_hash, and timestamp the anchor job last logged off-box for this
-- tenant. ErrNoRows means no anchor has ever been recorded (a brand-new
-- tenant, or one that posted before the anchor job's first tick).
SELECT tenant_id, chain_seq, row_hash, created_at FROM audit_anchors
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY chain_seq DESC
LIMIT 1;

-- name: ListAuditHeads :many
-- One row per tenant with at least one audit_log entry: that tenant's
-- current chain head (chain_seq, row_hash). The anchor job (Task 5.3) reads
-- this once per tick rather than enumerating tenants and calling
-- GetAuditHead once per tenant (an N+1 round trip): it inserts one
-- audit_anchors row and emits one structured log line per tenant returned
-- here.
SELECT DISTINCT ON (tenant_id) tenant_id, chain_seq, row_hash
FROM audit_log
ORDER BY tenant_id, chain_seq DESC;

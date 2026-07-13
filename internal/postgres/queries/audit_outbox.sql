-- name: InsertAuditOutbox :exec
-- Writes one outbox row inside the caller's own transaction (ADR-017): the
-- event is durable if and only if the surrounding post/convert transaction
-- commits. occurred_at, txid, and created_at are all left to their column
-- defaults (the database server's now() and pg_current_xact_id(), see
-- migration 0015): no chain read, no hash computed, here.
--
-- transaction_id, subject_type, and subject_id are all nullable (ADR-025,
-- migration 0034): a non-transaction lifecycle event (for example
-- approval.rejected) carries subject_type/subject_id instead of a
-- transaction_id. hash_version records which row-hash preimage the caller
-- computed against, so the chainer and Verify recompute with the same one.
INSERT INTO audit_outbox (tenant_id, action, transaction_id, actor, before, after, subject_type, subject_id, hash_version)
VALUES (sqlc.arg(tenant_id), sqlc.arg(action), sqlc.narg(transaction_id), sqlc.arg(actor), sqlc.arg(before), sqlc.arg(after), sqlc.narg(subject_type), sqlc.narg(subject_id), sqlc.arg(hash_version));

-- name: AuditOutboxWatermark :one
-- The oldest transaction id still in flight, cast the same way audit_outbox.txid
-- is (xid8 has no direct cast to bigint). A row whose txid is strictly below
-- this watermark is guaranteed committed and safe to chain (ADR-017,
-- "Ordering: process only settled rows, in transaction-commit order").
SELECT pg_snapshot_xmin(pg_current_snapshot())::text::bigint;

-- name: ScanUnprocessedAuditOutbox :many
-- The chainer's batch read: unprocessed rows whose inserting transaction is
-- guaranteed settled (txid < the watermark passed in), oldest commit order
-- first. Ordering by (txid, id) is the total order ADR-017 defines: it is
-- stable because txid is not reused and id is a bigserial tiebreaker for the
-- (rare) case of equal txid.
--
-- Includes subject_type, subject_id, and hash_version (ADR-025, migration
-- 0034) so the chainer can copy them onto the audit_log row it builds and
-- hash with the row's own recorded version.
SELECT id, tenant_id, action, transaction_id, actor, before, after, occurred_at, txid, subject_type, subject_id, hash_version
FROM audit_outbox
WHERE processed_at IS NULL AND txid < sqlc.arg(xmin)
ORDER BY txid, id
LIMIT sqlc.arg(batch_limit);

-- name: MarkAuditOutboxProcessed :exec
-- Sets processed_at for one outbox row. The chainer calls this in the same
-- transaction as the audit_log insert it produced, so a crash between the two
-- is impossible: either both happen or neither does.
UPDATE audit_outbox SET processed_at = now() WHERE id = sqlc.arg(id);

-- name: CountPendingOutbox :one
-- Unprocessed outbox rows for one tenant: the lag the verify endpoint reports
-- alongside the chained head (ADR-017 section 5), so a caller can see whether
-- the chain is current or behind.
SELECT count(*) FROM audit_outbox
WHERE tenant_id = sqlc.arg(tenant_id) AND processed_at IS NULL;

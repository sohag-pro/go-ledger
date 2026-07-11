-- +goose Up
-- +goose StatementBegin

-- ADR-017 IMPORTANT 2 and MINOR 3 (see docs/adr/017-multi-instance-audit-chain.md
-- and the Task 3.2 fix report): two additions to audit_log, both purely
-- additive and neither ever hashed by domain.ComputeAuditRowHash, so every
-- already-stored row_hash stays byte-identical after this migration.
--
-- chain_seq is the chain's true, DB-assigned, skew-proof linearization key.
-- Ordering by audit_log.id (a UUIDv7) is only monotonic within the ONE
-- process that minted it: across a leader failover to a different host with
-- clock skew, a new leader can mint an id LOWER than the current head, and
-- GetLastAuditHash / ListAuditForVerify (ORDER BY id) would then see a fork
-- that never actually happened. chain_seq is a plain ascending sequence the
-- single chainer process advances on every insert, in the same order it
-- assigns row_hash values, so it is immune to any host's clock, wall or
-- otherwise.
ALTER TABLE audit_log ADD COLUMN chain_seq bigint;

-- Backfill deterministically, in the chain's actual historical order: the
-- same (created_at, id) order the pre-0015 index (audit_log_tenant_created_idx)
-- used, with id as the tiebreaker for equal created_at. This is a one-time
-- assignment; the chainer is the only writer from here on and always inserts
-- in true chain order, so a single global ascending sequence, not a
-- per-tenant one, stays correctly monotonic in insertion order for every
-- tenant.
--
-- audit_log_reject_mutation (migration 0006/0009) blocks any UPDATE on this
-- append-only table unless audit.allow_purge is 'on'; SET LOCAL scopes that
-- override to this migration's own transaction only, exactly like the
-- seeder's reset does, so no session outside this migration ever gets to
-- treat audit_log as mutable.
SET LOCAL audit.allow_purge = 'on';
UPDATE audit_log SET chain_seq = backfill.rn
FROM (
    SELECT id, row_number() OVER (ORDER BY created_at, id) AS rn
    FROM audit_log
) AS backfill
WHERE audit_log.id = backfill.id;

-- The sequence backing chain_seq from here on, started past every backfilled
-- value so the first chainer-assigned row never collides with a backfilled
-- one. OWNED BY ties its lifetime to the column: dropping the column (this
-- migration's Down) drops the sequence with it.
CREATE SEQUENCE audit_log_chain_seq_seq OWNED BY audit_log.chain_seq;
SELECT setval('audit_log_chain_seq_seq', COALESCE((SELECT max(chain_seq) FROM audit_log), 0) + 1, false);
ALTER TABLE audit_log ALTER COLUMN chain_seq SET DEFAULT nextval('audit_log_chain_seq_seq');
ALTER TABLE audit_log ALTER COLUMN chain_seq SET NOT NULL;

-- Every read that establishes chain order now goes through chain_seq
-- (internal/postgres/queries/audit.sql: GetLastAuditHash, ListAuditForVerify),
-- scoped per tenant. audit_log_tenant_id_idx (migration 0015) stays:
-- ListAuditByTransaction and ListAuditByAccount still page by id, which is
-- fine for their purpose (a display ordering within one transaction or one
-- account's history, not the chain's own linearization).
CREATE UNIQUE INDEX audit_log_chain_seq_uidx ON audit_log (chain_seq);
CREATE INDEX audit_log_tenant_chain_seq_idx ON audit_log (tenant_id, chain_seq);

-- outbox_id (ADR-017 MINOR 3, defense in depth): the audit_outbox row this
-- audit_log row was chained from. Nullable (pre-migration rows, and every
-- row the OLD synchronous AppendAudit path ever wrote, have no outbox row to
-- point at) but UNIQUE where present, so a second attempt to chain the SAME
-- outbox row, structurally impossible after the CRITICAL 1 fix (every drain
-- query now runs on the single lock-holding connection, so a lost lock
-- session aborts the in-flight drain instead of silently continuing it),
-- becomes a failed insert the chainer catches and treats as "already chained
-- by the true leader", not a second, forking, row_hash chain link. A UNIQUE
-- constraint (not a plain index) permits any number of NULLs, so it imposes
-- nothing on rows that predate this column.
ALTER TABLE audit_log ADD COLUMN outbox_id bigint;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_outbox_id_key UNIQUE (outbox_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE audit_log DROP CONSTRAINT audit_log_outbox_id_key;
ALTER TABLE audit_log DROP COLUMN outbox_id;
DROP INDEX audit_log_tenant_chain_seq_idx;
DROP INDEX audit_log_chain_seq_uidx;
ALTER TABLE audit_log DROP COLUMN chain_seq;

-- +goose StatementEnd

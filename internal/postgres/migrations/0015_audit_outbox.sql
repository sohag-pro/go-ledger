-- +goose Up
-- +goose StatementBegin

-- ADR-017: decouple the tamper-evident audit hash chain from the posting hot
-- path so the service is correct with more than one instance. A post now
-- writes this append-only outbox row, atomically with its postings, instead
-- of reading the tenant's chain head and extending audit_log directly. A
-- single background chainer (internal/audit.Chainer) later drains this table
-- in transaction-commit order and builds audit_log exactly as the old
-- synchronous path did (same schema, same hashing).
--
-- transaction_id references transactions(id), the same integrity audit_log
-- itself already has (migration 0006): the outbox row is always written
-- inside the same transaction that inserts the transactions row it
-- describes, so the reference is always satisfiable.
--
-- occurred_at and created_at both default to the database server's now(),
-- deliberately, not the application clock: unlike the old AppendAudit (which
-- had to stamp CreatedAt with the app clock because it hashed that exact
-- value before storing it, see internal/postgres/repository.go), nothing
-- here computes a hash at insert time, so there is no precision-matching
-- reason to prefer the app's clock, and the database's own now() sidesteps
-- any app/database clock-skew question entirely (the same reasoning
-- InsertFXRate already applies to its effective_at column, migration 0010).
-- The chainer later copies occurred_at, unmodified, into audit_log.created_at
-- (see the chainer's doc comment for why that reproduces today's row_hash
-- bit for bit).
--
-- before/after are json, not jsonb, for exactly the reason migration 0009
-- converted audit_log's own before/after from jsonb to json: the chainer
-- hashes before/after's exact bytes as read back from this table, and jsonb
-- does not guarantee a byte-exact round trip (its output routine reformats
-- the stored value, for example inserting a space after ':' and ','), so a
-- value inserted as '{"id":"x"}' would read back as '{"id": "x"}', a
-- different byte sequence, breaking the hash the moment it passed through
-- this table. json has no such reformatting: it stores and returns the
-- exact input text, so the bytes the chainer reads are the exact bytes the
-- post wrote, unchanged.
CREATE TABLE audit_outbox (
    id             bigserial   PRIMARY KEY,
    tenant_id      uuid        NOT NULL,
    action         text        NOT NULL,
    transaction_id uuid        NOT NULL REFERENCES transactions(id),
    actor          text        NOT NULL,
    before         json,
    after          json        NOT NULL,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    -- The inserting transaction's id, cast from xid8 (pg_current_xact_id's
    -- return type, PostgreSQL 13+) through text to bigint: there is no direct
    -- xid8->bigint cast. A real xid8 value would have to exceed 2^63-1 (over
    -- nine quintillion transactions) to overflow this bigint; this service is
    -- nowhere near that in its operational lifetime, so the cast is safe in
    -- practice. This is the chainer's ordering key (ADR-017 "Ordering:
    -- process only settled rows, in transaction-commit order"): it lets the
    -- chainer compare a row's inserting transaction against
    -- pg_snapshot_xmin(pg_current_snapshot()), the oldest still-in-flight
    -- transaction id, and only ever process rows guaranteed to be settled.
    txid           bigint      NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- NULL until the chainer chains it; set exactly once, never cleared.
    processed_at   timestamptz
);

-- The chainer's hot query: unprocessed rows ordered by (txid, id), the total
-- order ADR-017 defines over committed events. The partial index (only
-- unprocessed rows) stays small forever in steady state, since processed
-- rows drop out of it the moment they are marked, unlike a full index over
-- every row this table will ever hold.
CREATE INDEX audit_outbox_unprocessed_idx
    ON audit_outbox (txid, id) WHERE processed_at IS NULL;

-- Supports CountPendingOutbox (verify's reported lag, ADR-017 section 5):
-- unprocessed rows for one tenant.
CREATE INDEX audit_outbox_tenant_unprocessed_idx
    ON audit_outbox (tenant_id) WHERE processed_at IS NULL;

-- Replaces migration 0009's audit_log_tenant_created_idx (tenant_id,
-- created_at, id): every audit_log read now orders by id, not (created_at,
-- id) (internal/postgres/queries/audit.sql). audit_log.id is a UUIDv7 the
-- chainer assigns at chain-insertion time, via a generator guaranteed
-- strictly increasing across successive calls in that one process (see
-- google/uuid's NewV7), so it is the true chain order. created_at is copied
-- from the ORIGINATING event's post time (audit_outbox.occurred_at), which
-- under concurrent posts across many transactions is not guaranteed to be
-- monotonic with the order those transactions actually commit in (a
-- transaction that starts later can commit first): a single hot tenant
-- posting from many goroutines (or many instances) at once reliably produces
-- audit_outbox rows whose occurred_at values are out of commit order, so
-- ordering audit_log by created_at can return the wrong "latest" row and
-- silently corrupt the chain. id has no such problem: it reflects the order
-- rows actually landed in audit_log, which is always the chain's real order,
-- regardless of when the originating event happened to occur.
DROP INDEX audit_log_tenant_created_idx;
CREATE INDEX audit_log_tenant_id_idx ON audit_log (tenant_id, id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX audit_log_tenant_id_idx;
CREATE INDEX audit_log_tenant_created_idx ON audit_log (tenant_id, created_at, id);
DROP INDEX audit_outbox_tenant_unprocessed_idx;
DROP INDEX audit_outbox_unprocessed_idx;
DROP TABLE audit_outbox;

-- +goose StatementEnd

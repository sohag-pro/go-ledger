-- +goose Up
-- +goose StatementBegin

-- Idempotency keys. The primary key (tenant_id, idempotency_key) is the
-- exactly-once mutex: it is inserted inside the same transaction that posts the
-- ledger transaction, so under a concurrent retry storm exactly one insert wins
-- and the losers roll back their whole transaction. fingerprint is the SHA-256
-- of the original request's canonical form, used to reject a reused key that
-- carries a different body.
CREATE TABLE idempotency_keys (
    tenant_id       uuid        NOT NULL,
    idempotency_key text        NOT NULL,
    fingerprint     text        NOT NULL,
    transaction_id  uuid        NOT NULL REFERENCES transactions(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

-- Append-only audit log. One row per posted transaction in v1: action is
-- 'transaction.created', before is NULL (nothing mutates in place), after is a
-- JSON snapshot, actor is the tenant id until an auth layer lands.
CREATE TABLE audit_log (
    id             uuid        PRIMARY KEY,
    tenant_id      uuid        NOT NULL,
    action         text        NOT NULL,
    transaction_id uuid        NOT NULL REFERENCES transactions(id),
    actor          text        NOT NULL,
    before         jsonb,
    after          jsonb       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_tenant_txn ON audit_log (tenant_id, transaction_id);

-- Immutability: reject UPDATE and DELETE so the log is genuinely append-only,
-- not append-only by convention. The one exception is the demo seeder's reset,
-- which sets the transaction-local GUC audit.allow_purge to 'on' before it
-- clears the tenant. The application path never sets it, so in production the
-- log cannot be altered or deleted.
CREATE FUNCTION audit_log_reject_mutation() RETURNS trigger AS $$
BEGIN
    IF current_setting('audit.allow_purge', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only'
        USING ERRCODE = 'restrict_violation', CONSTRAINT = 'audit_log_append_only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_immutable
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW
    EXECUTE FUNCTION audit_log_reject_mutation();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER audit_log_immutable ON audit_log;
DROP FUNCTION audit_log_reject_mutation();
DROP TABLE audit_log;
DROP TABLE idempotency_keys;
-- +goose StatementEnd

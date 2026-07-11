-- +goose Up
-- +goose StatementBegin

-- Task 5.4a (audit A4.4): idempotency_keys, audit_log, and audit_outbox each
-- reference transactions(id) alone (migrations 0006 and 0015). transactions
-- also carries tenant_id, but the single-column FK never checks it, so in
-- principle a row in one of these tables could carry a tenant_id that does
-- not match the tenant_id of the transaction its transaction_id points at:
-- a cross-tenant reference the FK was silently blind to. The application
-- never writes such a row (every insert path sets both columns from the
-- same transaction it just posted), so this tightens a latent gap rather
-- than fixing an observed bug.
--
-- transactions carries UNIQUE (tenant_id, id) (migration 0001), so a
-- composite FK on (tenant_id, transaction_id) REFERENCES transactions
-- (tenant_id, id) is a valid target: it demands both columns agree with the
-- same transactions row, closing the gap. Each existing single-column FK is
-- dropped by its real (Postgres auto-generated) name before the replacement
-- is added, so the table never carries both at once.
ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_transaction_id_fkey;
ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_txn_fk
    FOREIGN KEY (tenant_id, transaction_id) REFERENCES transactions (tenant_id, id);

ALTER TABLE audit_log DROP CONSTRAINT audit_log_transaction_id_fkey;
ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_txn_fk
    FOREIGN KEY (tenant_id, transaction_id) REFERENCES transactions (tenant_id, id);

ALTER TABLE audit_outbox DROP CONSTRAINT audit_outbox_transaction_id_fkey;
ALTER TABLE audit_outbox
    ADD CONSTRAINT audit_outbox_txn_fk
    FOREIGN KEY (tenant_id, transaction_id) REFERENCES transactions (tenant_id, id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE audit_outbox DROP CONSTRAINT audit_outbox_txn_fk;
ALTER TABLE audit_outbox
    ADD CONSTRAINT audit_outbox_transaction_id_fkey
    FOREIGN KEY (transaction_id) REFERENCES transactions (id);

ALTER TABLE audit_log DROP CONSTRAINT audit_log_txn_fk;
ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_transaction_id_fkey
    FOREIGN KEY (transaction_id) REFERENCES transactions (id);

ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_txn_fk;
ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_transaction_id_fkey
    FOREIGN KEY (transaction_id) REFERENCES transactions (id);

-- +goose StatementEnd

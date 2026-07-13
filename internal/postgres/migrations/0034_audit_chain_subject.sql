-- +goose Up
-- +goose StatementBegin

-- ADR-025: let the tamper-evident chain carry non-transaction lifecycle events
-- (approval.*). transaction_id becomes nullable (a rejected pending never
-- becomes a transaction); subject_type/subject_id name what a non-transaction
-- event is about; hash_version records which row-hash preimage produced
-- row_hash so verification stays correct across mixed rows. Existing rows keep
-- hash_version=1 (the default) and their transaction_id, so nothing rehashes.
ALTER TABLE audit_outbox ALTER COLUMN transaction_id DROP NOT NULL;
ALTER TABLE audit_outbox ADD COLUMN subject_type text;
ALTER TABLE audit_outbox ADD COLUMN subject_id   uuid;
ALTER TABLE audit_outbox ADD COLUMN hash_version smallint NOT NULL DEFAULT 1;

ALTER TABLE audit_log ALTER COLUMN transaction_id DROP NOT NULL;
ALTER TABLE audit_log ADD COLUMN subject_type text;
ALTER TABLE audit_log ADD COLUMN subject_id   uuid;
ALTER TABLE audit_log ADD COLUMN hash_version smallint NOT NULL DEFAULT 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE audit_log DROP COLUMN hash_version;
ALTER TABLE audit_log DROP COLUMN subject_id;
ALTER TABLE audit_log DROP COLUMN subject_type;
ALTER TABLE audit_log ALTER COLUMN transaction_id SET NOT NULL;

ALTER TABLE audit_outbox DROP COLUMN hash_version;
ALTER TABLE audit_outbox DROP COLUMN subject_id;
ALTER TABLE audit_outbox DROP COLUMN subject_type;
ALTER TABLE audit_outbox ALTER COLUMN transaction_id SET NOT NULL;

-- +goose StatementEnd

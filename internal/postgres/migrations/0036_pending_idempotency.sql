-- +goose Up
-- +goose StatementBegin

-- ADR-025 section 6 (Lifecycle) promises that a gated create consumes its
-- idempotency key against the pending it holds, so a replay of the same
-- key returns the same pending rather than creating a second one.
-- idempotency_key is nullable: a held create with no Idempotency-Key header
-- at all (the client opted out) still gets a fresh pending every retry,
-- exactly like an ungated post with no key does; there is nothing to dedup
-- against. The partial unique index only applies where the key is present,
-- so many NULL rows for the same tenant never collide with each other.
ALTER TABLE pending_transactions ADD COLUMN idempotency_key text;

CREATE UNIQUE INDEX pending_transactions_idempotency_idx
    ON pending_transactions (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX pending_transactions_idempotency_idx;
ALTER TABLE pending_transactions DROP COLUMN idempotency_key;

-- +goose StatementEnd

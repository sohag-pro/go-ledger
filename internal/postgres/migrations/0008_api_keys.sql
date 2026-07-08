-- +goose Up
-- +goose StatementBegin

-- API keys authenticate every /v1 and gRPC request (ADR-012). The tenant is
-- resolved only from the key, never from a request field, so a key for one
-- tenant can never act on another. Only the SHA-256 hex of the plaintext key is
-- stored in key_hash; the plaintext itself is shown once at creation and is
-- never recoverable from the database. rate_limit_rpm is nullable: NULL means
-- the server default applies. revoked_at is nullable: NULL means the key is
-- active, and setting it (rather than deleting the row) keeps a revoked key's
-- history around for audit.
CREATE TABLE api_keys (
    id             uuid        PRIMARY KEY,
    tenant_id      uuid        NOT NULL,
    name           text        NOT NULL,
    key_hash       text        NOT NULL UNIQUE,
    rate_limit_rpm integer,
    created_at     timestamptz NOT NULL DEFAULT now(),
    revoked_at     timestamptz
);
CREATE INDEX api_keys_tenant_idx ON api_keys (tenant_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE api_keys;
-- +goose StatementEnd

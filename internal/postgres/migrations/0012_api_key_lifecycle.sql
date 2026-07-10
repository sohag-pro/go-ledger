-- +goose Up
-- +goose StatementBegin

-- Task 2.2 (audit A3.2, A2.3): give api_keys a lifecycle. Before this
-- migration a key was either live or revoked (revoked_at), with no way to
-- scope what it could do, no way to make it expire on its own, and no record
-- of whether it was even still in use.
--
-- scopes is NOT NULL with a default of {read,post}: every key issued before
-- this migration (including the demo and load-test keys provisioned by
-- cmd/server) keeps working exactly as it did, since read+post is the full
-- set of things a pre-2.2 key could do. admin is a new capability that no
-- existing key gets automatically; the admin surface that grants it is a
-- separate follow-up task (2.2b).
--
-- expires_at and last_used_at are both nullable: NULL means "never expires"
-- and "never used yet" respectively, matching the nullable revoked_at
-- convention this table already uses.
ALTER TABLE api_keys ADD COLUMN scopes       text[]      NOT NULL DEFAULT ARRAY['read','post'];
ALTER TABLE api_keys ADD COLUMN expires_at   timestamptz;
ALTER TABLE api_keys ADD COLUMN last_used_at timestamptz;

-- Fail closed at the schema level too, not just in application code: scopes
-- must be non-empty and every element must be one of the three known scopes.
-- cardinality(), not array_length(scopes, 1): array_length returns NULL (not
-- 0) for an empty array, and a NULL CHECK expression is *not* a violation in
-- Postgres, so an empty scopes array would silently pass. cardinality()
-- returns 0 for an empty array, closing that gap. scopes <@ ARRAY[...] reads
-- as "scopes is a subset of the given array".
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_valid
    CHECK (cardinality(scopes) >= 1
           AND scopes <@ ARRAY['read','post','admin']);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse order: drop the constraint before the columns it depends on.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_valid;
ALTER TABLE api_keys DROP COLUMN last_used_at;
ALTER TABLE api_keys DROP COLUMN expires_at;
ALTER TABLE api_keys DROP COLUMN scopes;

-- +goose StatementEnd

-- +goose Up
-- +goose StatementBegin

-- ADR-025: add the 'approve' scope. The api_keys_scopes_valid CHECK (migration
-- 0012) enumerated the three original scopes; recreate it with 'approve' added
-- so a key may carry it. Drop-then-add because a CHECK cannot be altered in
-- place.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_valid;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_valid
    CHECK (cardinality(scopes) >= 1
           AND scopes <@ ARRAY['read','post','approve','admin']);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_valid;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_valid
    CHECK (cardinality(scopes) >= 1
           AND scopes <@ ARRAY['read','post','admin']);

-- +goose StatementEnd

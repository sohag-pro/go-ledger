-- +goose Up
-- +goose StatementBegin

-- Adds the per-tenant tamper-evident hash chain to audit_log (ADR-012). Each
-- row's row_hash covers its own content and the previous row's row_hash for
-- the same tenant, so reordering, inserting, or deleting a row breaks the
-- chain from that point on (see domain.ComputeAuditRowHash for the exact
-- fields and encoding). prev_hash and row_hash are nullable because this is an
-- in-place migration: rows written before it carry no chain data and are
-- never backfilled, so they stay NULL and unchained forever. That is fine in
-- practice: the only tenant with pre-migration rows is the demo tenant, and
-- the seeder clears its audit_log (and the rest of its data) within four
-- hours of this migration landing, at which point its chain starts fresh from
-- genesis. Every row the application writes from here on always supplies
-- both columns.
ALTER TABLE audit_log ADD COLUMN prev_hash text, ADD COLUMN row_hash text;

-- Serves two reads: the tenant's latest row_hash (ORDER BY created_at DESC, id
-- DESC LIMIT 1, read inside the same transaction that extends the chain) and
-- the verify walk (ORDER BY created_at, id ASC over one tenant, oldest
-- first). id is the tiebreaker for rows sharing a created_at.
CREATE INDEX audit_log_tenant_created_idx ON audit_log (tenant_id, created_at, id);

-- row_hash covers before/after's exact bytes, so those bytes must read back
-- identical to what was hashed at write time. jsonb does not guarantee that:
-- its output routine reformats the stored value on every read (for example
-- inserting a space after ':' and ','), so a row inserted with
-- '{"id":"x"}' reads back as '{"id": "x"}', a different byte sequence, and
-- the chain could never verify. json has no such reformatting: it stores and
-- returns the exact input text. Nothing else in this schema queries these
-- columns with jsonb-only operators (they are opaque payloads, passed through
-- to API responses as raw JSON), so there is no query-side reason to keep
-- jsonb, and every reason from the hash chain to require the byte-exact
-- json.
ALTER TABLE audit_log
    ALTER COLUMN before TYPE json USING before::json,
    ALTER COLUMN after TYPE json USING after::json;

-- migration 0006's audit_log_reject_mutation always `RETURN OLD`. For DELETE
-- that is correct (a BEFORE DELETE trigger just needs to return any non-null
-- row to let the delete proceed, and OLD is what is being deleted anyway).
-- For UPDATE it is a latent bug: PostgreSQL writes back whatever row a BEFORE
-- UPDATE trigger returns, so returning OLD silently discards the new values.
-- The UPDATE reports success (rows affected) but nothing actually changes.
-- The seeder never hit this (it only ever DELETEs to reset a tenant), so it
-- went unnoticed, but the hash chain's tamper test needs a raw UPDATE through
-- this same gate to actually take effect. Replace the function so the gate
-- returns NEW for UPDATE and OLD for DELETE.
CREATE OR REPLACE FUNCTION audit_log_reject_mutation() RETURNS trigger AS $$
BEGIN
    IF current_setting('audit.allow_purge', true) = 'on' THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only'
        USING ERRCODE = 'restrict_violation', CONSTRAINT = 'audit_log_append_only';
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit_log_reject_mutation() RETURNS trigger AS $$
BEGIN
    IF current_setting('audit.allow_purge', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only'
        USING ERRCODE = 'restrict_violation', CONSTRAINT = 'audit_log_append_only';
END;
$$ LANGUAGE plpgsql;
ALTER TABLE audit_log
    ALTER COLUMN before TYPE jsonb USING before::jsonb,
    ALTER COLUMN after TYPE jsonb USING after::jsonb;
DROP INDEX audit_log_tenant_created_idx;
ALTER TABLE audit_log DROP COLUMN prev_hash, DROP COLUMN row_hash;
-- +goose StatementEnd

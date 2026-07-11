-- +goose Up
-- +goose StatementBegin

-- ADR-018 fix (audit remediation review of Task 6.2): migration 0027 gave
-- every tenant exactly ONE Data Encryption Key, ever. That made
-- internal/crypto.Cipher.Encrypt fail closed (ErrTenantKeyShredded) for
-- EVERY post/convert/reversal a tenant made after an erasure request,
-- forever, even though a conversion leg's label ("convert: debit source
-- account") and a reversal's narration ("reversal of <id>") are
-- system-generated, not personal data: a shred request would have quietly
-- bricked the tenant's ability to transact at all, not merely erased past
-- PII.
--
-- crypto_keys becomes VERSIONED: a tenant can hold a sequence of DEK
-- versions over time, keyed on (tenant_id, version). The existing
-- single-version row every tenant already has becomes version 1. Shredding
-- (see migration 0028's ShredCurrentCryptoKey query,
-- internal/postgres/crypto_keys.go) now destroys only the CURRENT (highest)
-- version and leaves every OLDER version's shredded state exactly as it
-- was; the tenant's very next Encrypt call mints a fresh version and keeps
-- working. Every ciphertext produced under the new scheme names its DEK
-- version in the stored string itself (internal/crypto.EncodingPrefix's doc
-- comment), so a decrypt always knows which version's key to unwrap.
ALTER TABLE crypto_keys ADD COLUMN version integer NOT NULL DEFAULT 1;

-- The single-column primary key becomes composite: (tenant_id, version).
-- Dropping crypto_keys_pkey does NOT drop the tenant_id -> tenants(id)
-- foreign key: that is always a separate constraint object from the
-- REFERENCES-on-a-PRIMARY-KEY-column shorthand migration 0027 used, so it
-- survives this ALTER untouched.
ALTER TABLE crypto_keys DROP CONSTRAINT crypto_keys_pkey;
ALTER TABLE crypto_keys ADD CONSTRAINT crypto_keys_pkey PRIMARY KEY (tenant_id, version);
ALTER TABLE crypto_keys ALTER COLUMN version DROP DEFAULT;

-- Fast "find this tenant's current version" lookups (every Encrypt call
-- that isn't a first-use or post-shred mint does one of these): version
-- DESC so "ORDER BY version DESC LIMIT 1" is an index-only scan.
CREATE INDEX crypto_keys_tenant_id_version_idx ON crypto_keys (tenant_id, version DESC);

-- The RLS policy (ENABLE + FORCE + one allow-when-unset tenant_isolation
-- policy, migration 0027) is untouched by any of the above: it is attached
-- to the TABLE, not to the primary key or any specific column, and neither
-- ENABLE ROW LEVEL SECURITY nor FORCE ROW LEVEL SECURITY nor the CREATE
-- POLICY statement is repeated here. crypto_keys_dek_xor_shredded (the CHECK
-- constraint keeping wrapped_dek and shredded_at mutually exclusive) is also
-- untouched: it still applies per row, regardless of that row's version.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Best-effort collapse back to migration 0027's single-row-per-tenant
-- shape: a tenant that minted more than one DEK version loses every version
-- but the first (down-migrations are a dev/rollback path, never run against
-- a production database that has actually accumulated multiple versions;
-- see the Makefile's migrate-down target and this repo's own migration
-- convention of a "clean" but not necessarily loss-free down path).
DELETE FROM crypto_keys WHERE version <> 1;

DROP INDEX crypto_keys_tenant_id_version_idx;
ALTER TABLE crypto_keys DROP CONSTRAINT crypto_keys_pkey;
ALTER TABLE crypto_keys ADD CONSTRAINT crypto_keys_pkey PRIMARY KEY (tenant_id);
ALTER TABLE crypto_keys DROP COLUMN version;

-- +goose StatementEnd

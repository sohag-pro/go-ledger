-- +goose Up
-- +goose StatementBegin

-- Task 6.2 (audit A9.3): crypto_keys holds one row per tenant with a Data
-- Encryption Key (DEK), wrapped (AES-256-GCM) by the deployment's master key
-- (LEDGER_MASTER_KEY, never stored in the database), used to encrypt posting
-- descriptions at rest (internal/crypto.Cipher). "Crypto-shredding": an
-- erasure request destroys the row's wrapped_dek (set NULL) and stamps
-- shredded_at, making every description ever encrypted under it permanently
-- unreadable, WITHOUT touching postings.description or audit_log.after: the
-- tamper-evident hash chain (ADR-012) hashes those stored bytes exactly as
-- written, so it stays verifiable after a shred (see
-- domain.Repository.ShredTenantCryptoKey and internal/crypto's own package
-- doc comment for the full argument).
--
-- The CHECK constraint keeps the two "shreddable" columns coherent: a live
-- key has a wrapped_dek and no shredded_at, a shredded key has shredded_at
-- and no wrapped_dek (Postgres's NULL-destroying UPDATE is what makes
-- "destroy the key" durable and unrecoverable: there is no soft-delete flag
-- to flip back). Exactly one of the two is ever true for a given row.
CREATE TABLE crypto_keys (
    tenant_id   uuid        PRIMARY KEY REFERENCES tenants (id),
    wrapped_dek bytea,
    created_at  timestamptz NOT NULL DEFAULT now(),
    shredded_at timestamptz,
    CONSTRAINT crypto_keys_dek_xor_shredded CHECK (
        (wrapped_dek IS NOT NULL AND shredded_at IS NULL) OR
        (wrapped_dek IS NULL AND shredded_at IS NOT NULL)
    )
);

-- Row-level security, the exact same shape migration 0024 established (Task
-- 5.4b) and every table added since (migration 0025's audit_anchors) has
-- followed: FORCE (the goledger role owns this table too, so ENABLE alone
-- would protect nothing), one allow-when-unset tenant_isolation policy. The
-- request path (internal/crypto.Cipher, called from the ledger/account/audit
-- services with app.tenant_id set via withTenant/RunInTx) is restricted to
-- its own tenant's key; a cross-tenant caller (there is none today, but the
-- backstop applies regardless) could never read or wrap/unwrap another
-- tenant's DEK even if it forgot a WHERE tenant_id somewhere.
ALTER TABLE crypto_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE crypto_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON crypto_keys
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- NO FORCE before DISABLE, then drop: relforcerowsecurity is a separate flag
-- from relrowsecurity, exactly the same two-step teardown migration
-- 0024's own Down section documents.
DROP POLICY tenant_isolation ON crypto_keys;
ALTER TABLE crypto_keys NO FORCE ROW LEVEL SECURITY;
ALTER TABLE crypto_keys DISABLE ROW LEVEL SECURITY;

DROP TABLE crypto_keys;

-- +goose StatementEnd

-- name: GetOrCreateCryptoKey :one
-- Task 6.2 (audit A9.3): returns tenant_id's wrapped DEK, creating a fresh
-- row wrapping the CALLER-GENERATED candidate wrapped_dek on first use. The
-- ON CONFLICT DO UPDATE is a no-op write (tenant_id set to its own current
-- value) purely to force RETURNING to yield the current row: the same
-- "GetOrCreateClearingAccount" trick internal/postgres/queries/accounts.sql
-- documents in depth, which resolves two callers racing to create the same
-- tenant's first key, even across processes, to exactly one winning DEK in a
-- single round trip.
--
-- A tenant whose key was already shredded (wrapped_dek NULL, shredded_at
-- set) is NOT revived by this call: the no-op update leaves both columns
-- untouched, so the existing (shredded) row comes back unchanged, and the
-- caller (internal/crypto.Cipher) must check shredded_at itself rather than
-- assume wrapped_dek is ever non-NULL just because a row was returned.
INSERT INTO crypto_keys (tenant_id, wrapped_dek)
VALUES ($1, $2)
ON CONFLICT (tenant_id) DO UPDATE SET tenant_id = EXCLUDED.tenant_id
RETURNING tenant_id, wrapped_dek, shredded_at;

-- name: GetCryptoKey :one
-- Read-only lookup for a decrypt (Task 6.2, audit A9.3): unlike
-- GetOrCreateCryptoKey, this never creates a row. A description already
-- carrying internal/crypto.EncodingPrefix implies a key was created at
-- encrypt time, so pgx.ErrNoRows here is a genuine inconsistency for the
-- caller to surface as an error, not the ordinary first-use case.
SELECT tenant_id, wrapped_dek, shredded_at FROM crypto_keys WHERE tenant_id = $1;

-- name: ShredCryptoKey :exec
-- Irreversibly destroys tenant_id's PII encryption key (Task 6.2, audit
-- A9.3): every posting description ever encrypted under this tenant's DEK
-- becomes permanently unreadable, while postings.description and
-- audit_log.after keep their exact stored ciphertext bytes, so the
-- tamper-evident hash chain (which hashes those bytes, never decrypts them)
-- stays verifiable. This is an INSERT ... ON CONFLICT, not a plain UPDATE,
-- so shredding a tenant that has NEVER encrypted anything yet (no
-- crypto_keys row) still leaves a permanent shredded tombstone: without one,
-- that tenant's very next encrypt would happily mint a brand-new, live DEK,
-- silently undoing an operator's already-issued shred request. Idempotent:
-- shredding an already-shredded tenant leaves shredded_at at its ORIGINAL
-- value (COALESCE keeps the first shred timestamp), matching
-- RevokeAPIKey's own idempotent-revoke convention (shredding twice is a
-- no-op success, not an error, and does not move the erasure timestamp).
INSERT INTO crypto_keys (tenant_id, wrapped_dek, shredded_at)
VALUES ($1, NULL, now())
ON CONFLICT (tenant_id) DO UPDATE
SET wrapped_dek = NULL, shredded_at = COALESCE(crypto_keys.shredded_at, now());

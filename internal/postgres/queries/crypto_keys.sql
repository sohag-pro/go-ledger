-- name: GetCurrentCryptoKey :one
-- ADR-018 (Task 6.2 fix): tenant_id's CURRENT (highest-version) key row,
-- whatever its shredded state. internal/crypto.Cipher.Encrypt uses this to
-- decide whether to reuse it (found && not shredded) or mint a fresh,
-- forward version (not found at all, or the current one is shredded): see
-- MintCryptoKeyVersion below. pgx.ErrNoRows here means the tenant has never
-- encrypted anything and never been shredded: a genuine first-use case, not
-- an error, for the caller to handle.
SELECT tenant_id, version, wrapped_dek, shredded_at FROM crypto_keys
WHERE tenant_id = $1
ORDER BY version DESC
LIMIT 1;

-- name: GetCryptoKeyVersion :one
-- Read-only lookup of ONE SPECIFIC DEK version, for a decrypt whose stored
-- ciphertext names the version it was sealed under (ADR-018). Never
-- creates a row: a description already carrying internal/crypto.EncodingPrefix
-- implies a key existed for that exact version at encrypt time, so
-- pgx.ErrNoRows here is a genuine inconsistency for the caller to surface as
-- an error, not the ordinary first-use case.
SELECT tenant_id, version, wrapped_dek, shredded_at FROM crypto_keys
WHERE tenant_id = $1 AND version = $2;

-- name: MintCryptoKeyVersion :one
-- Atomically creates tenant_id's crypto_keys row at the given version,
-- wrapping the CALLER-GENERATED candidate_wrapped_dek (ADR-018: only
-- internal/crypto.Cipher ever holds the master key needed to produce one).
-- pg_advisory_xact_lock serializes this against ShredCurrentCryptoKey for
-- the SAME tenant (both take the identical hashtextextended(tenant_id, 0)
-- key), so a version this call is in the middle of minting can never be the
-- exact version a concurrent shred call means to destroy, or vice versa;
-- the lock is released automatically at the end of this call's transaction
-- (withTenant's COMMIT/ROLLBACK).
--
-- The ON CONFLICT DO UPDATE is the same "force RETURNING to yield the
-- winning row" trick migration 0027's original GetOrCreateCryptoKey used at
-- the single-version grain (see also GetOrCreateClearingAccount): if two
-- callers race to mint the exact same (tenant_id, version) pair, for example
-- two concurrent first-use Encrypt calls for a brand-new tenant both
-- targeting version 1, one wins and the other's own candidate wrapped_dek is
-- silently discarded; both RETURNING the SAME winning row in one round
-- trip, so both callers end up using the identical DEK.
-- sqlc.arg(tenant_id), repeated, names the SAME parameter (one field in the
-- generated Params struct) at every use; the redundant ::uuid cast at each
-- use (not just the ::text one hashtextextended needs) is deliberate, not
-- decorative: Postgres assigns an untyped placeholder's type by unifying
-- every one of its uses in the statement, and the ::text cast below would
-- otherwise make the planner infer the WHOLE parameter as text, including
-- the INSERT's tenant_id (uuid) column, which then fails to bind ("column
-- tenant_id is of type uuid but expression is of type text"). Casting to
-- uuid explicitly at each use pins its type regardless of statement order.
WITH locked AS (
    SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(tenant_id)::uuid::text, 0))
)
INSERT INTO crypto_keys (tenant_id, version, wrapped_dek)
SELECT sqlc.arg(tenant_id)::uuid, sqlc.arg(version), sqlc.arg(wrapped_dek) FROM locked
ON CONFLICT (tenant_id, version) DO UPDATE SET tenant_id = EXCLUDED.tenant_id
RETURNING tenant_id, version, wrapped_dek, shredded_at;

-- name: ShredCurrentCryptoKey :exec
-- Irreversibly destroys tenant_id's CURRENT (highest-version) DEK (ADR-018):
-- every posting description ever encrypted under that version becomes
-- permanently unreadable, while postings.description and audit_log.after
-- keep their exact stored ciphertext bytes untouched, so the tamper-evident
-- hash chain (which hashes those bytes, never decrypts them) stays
-- verifiable. Money data and every OTHER tenant's keys are completely
-- unaffected. The tenant is NOT bricked: its next Encrypt call
-- (internal/crypto.Cipher) sees the current version shredded and mints the
-- next one, so posting, converting, and reversing all keep working.
--
-- Serialized against MintCryptoKeyVersion by the same per-tenant
-- pg_advisory_xact_lock (see that query's own comment): this can never
-- target a version a concurrent first-use mint is still in the middle of
-- creating, and a concurrent mint can never land a version this call has
-- already decided to shred out from under it.
--
-- A tenant with NO crypto_keys row at all yet (never encrypted anything)
-- still gets a permanent version-1 shredded tombstone (GREATEST(...,1)
-- below): without one, that tenant's very next Encrypt would happily mint a
-- brand-new, LIVE version 1, silently undoing an operator's already-issued
-- shred request. Idempotent: shredding an already-shredded current version
-- leaves its shredded_at at its ORIGINAL value (COALESCE), matching
-- RevokeAPIKey's own idempotent-revoke convention.
-- Same sqlc.arg(tenant_id), repeated, plus the same explicit ::uuid cast at
-- every use as MintCryptoKeyVersion, and for the same reason (see that
-- query's own comment): the ::text cast inside hashtextextended would
-- otherwise make the planner infer the whole parameter as text, breaking
-- the uuid comparison/insert below.
WITH locked AS (
    SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(tenant_id)::uuid::text, 0))
), target AS (
    SELECT GREATEST(COALESCE((SELECT MAX(version) FROM crypto_keys WHERE tenant_id = sqlc.arg(tenant_id)::uuid), 0), 1) AS version
    FROM locked
)
INSERT INTO crypto_keys (tenant_id, version, wrapped_dek, shredded_at)
SELECT sqlc.arg(tenant_id)::uuid, target.version, NULL, now() FROM target
ON CONFLICT (tenant_id, version) DO UPDATE
SET wrapped_dek = NULL, shredded_at = COALESCE(crypto_keys.shredded_at, now());

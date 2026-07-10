-- name: InsertIdempotencyKey :one
-- Inserts a fresh idempotency key with a server-computed expires_at (now() +
-- ttl_seconds, never the calling process's clock: Task 4.5, audit A1.4,
-- consistent with fx_rates.effective_at's own server-side stamping).
--
-- ON CONFLICT upserts rather than plain-inserts because a row for
-- (tenant_id, idempotency_key) can already be present and EXPIRED: the
-- lookup path (GetIdempotencyKey) treats an expired row as absent, so the
-- caller proceeds to post a brand-new transaction and lands here again with
-- the same key. Without the upsert that would hit the primary key and be
-- misread as a genuine duplicate (replaying a stale, no-longer-relevant
-- transaction). The "WHERE idempotency_keys.expires_at <= now()" guard on
-- the DO UPDATE means the replace only happens when the existing row has, in
-- fact, expired; a conflict against a still-live row leaves it untouched and
-- the update affects zero rows, so RETURNING yields no row and the caller
-- (postgres.txRepo.InsertIdempotencyKey) maps pgx.ErrNoRows to
-- domain.ErrDuplicateIdempotencyKey exactly as a plain unique-violation used
-- to, preserving replay-on-duplicate for the still-live case.
INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, fingerprint_scheme, transaction_id, expires_at)
VALUES ($1, $2, $3, $4, $5, now() + (sqlc.arg(ttl_seconds)::float8 * interval '1 second'))
ON CONFLICT (tenant_id, idempotency_key) DO UPDATE
    SET fingerprint        = EXCLUDED.fingerprint,
        fingerprint_scheme = EXCLUDED.fingerprint_scheme,
        transaction_id     = EXCLUDED.transaction_id,
        created_at         = now(),
        expires_at         = EXCLUDED.expires_at
    WHERE idempotency_keys.expires_at <= now()
RETURNING tenant_id;

-- name: GetIdempotencyKey :one
-- An expired row is treated as absent (Task 4.5, audit A1.4): the "AND
-- expires_at > now()" filter is what makes a key whose replay window has
-- passed behave exactly like a key that was never written, from the caller's
-- point of view, whether or not the background sweep has physically deleted
-- the row yet.
SELECT tenant_id, idempotency_key, fingerprint, fingerprint_scheme, transaction_id, created_at
FROM idempotency_keys
WHERE tenant_id = $1 AND idempotency_key = $2 AND expires_at > now();

-- name: SweepExpiredIdempotencyKeys :execrows
-- Deletes every idempotency key whose replay window has passed, across all
-- tenants (Task 4.5, audit A1.4): this is what keeps the table from growing
-- forever. Not tenant-scoped and not run inside RunInTx: it is a plain
-- maintenance statement a background goroutine calls on an interval (see
-- cmd/server's idempotency sweeper), not part of any request's unit of work.
DELETE FROM idempotency_keys WHERE expires_at < now();

-- +goose Up
-- +goose StatementBegin

-- Task 4.5 (audit A1.4): idempotency keys lived forever, so the table grew
-- without bound. expires_at gives every key a finite lifetime: a key past its
-- expiry is treated as absent (see GetIdempotencyKey), so the same
-- Idempotency-Key header can be reused for a brand-new request once its
-- replay window has passed, and a background sweep (internal/postgres's
-- SweepExpiredIdempotencyKeys, wired from cmd/server) periodically deletes
-- expired rows so the table does not grow forever.
ALTER TABLE idempotency_keys ADD COLUMN expires_at timestamptz;

-- Backfill existing rows so they age out too, instead of living forever
-- just because they predate this column. created_at + 24h matches
-- IDEMPOTENCY_TTL's own default (cmd/server), so a pre-existing key's
-- remaining lifetime after this migration lands is whatever was left of a
-- 24h window measured from when it was first written.
UPDATE idempotency_keys SET expires_at = created_at + interval '24 hours' WHERE expires_at IS NULL;

ALTER TABLE idempotency_keys ALTER COLUMN expires_at SET NOT NULL;

-- Supports both the lookup's "AND expires_at > now()" filter and the sweep's
-- "WHERE expires_at < now()" delete.
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idempotency_keys_expires_at_idx;
ALTER TABLE idempotency_keys DROP COLUMN expires_at;
-- +goose StatementEnd

-- +goose Up
-- +goose StatementBegin

-- Task 2.3 (audit A1.6): tag every stored idempotency key with the
-- fingerprint scheme that produced it. Before this column, the scheme was
-- implicit: a change to how a fingerprint is computed (domain.Fingerprint,
-- domain.ConvertRequestFingerprint) silently invalidated every key already
-- on disk, since the replay path recomputed under whatever scheme the
-- current binary happened to implement and compared it against a hash
-- produced by a possibly different scheme. Recording the scheme lets the
-- replay path recompute under the scheme that actually produced the stored
-- hash, so a future scheme change is non-breaking. Existing rows predate any
-- scheme concept and were all produced by the scheme this binary calls
-- 'v1', so they default to it.
ALTER TABLE idempotency_keys
    ADD COLUMN fingerprint_scheme text NOT NULL DEFAULT 'v1';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE idempotency_keys DROP COLUMN fingerprint_scheme;
-- +goose StatementEnd

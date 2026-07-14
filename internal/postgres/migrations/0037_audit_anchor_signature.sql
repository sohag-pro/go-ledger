-- +goose Up
-- +goose StatementBegin

-- A tamper-evidence hardening for the audit anchor (audit remediation): an
-- anchor is a checkpoint VerifyFromLatestAnchor trusts, but audit_anchors lives
-- in the same database a privileged attacker controls, so a consistent rewrite
-- of audit_log AND audit_anchors would pass verification. The signature is an
-- HMAC-SHA256 over (tenant_id, chain_seq, row_hash) keyed by an app-held secret
-- (AUDIT_ANCHOR_SIGNING_KEY) the database role does not hold, so an attacker
-- with only DB access cannot forge a valid anchor for a rewritten chain.
--
-- NULL is "unsigned": signing is opt-in, and rows written before it was enabled
-- (and every deployment that never sets a key) keep the prior behavior.
ALTER TABLE audit_anchors ADD COLUMN signature BYTEA;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE audit_anchors DROP COLUMN signature;

-- +goose StatementEnd

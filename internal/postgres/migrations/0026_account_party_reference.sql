-- +goose Up
-- +goose StatementBegin

-- Task 6.1 (audit A9.1): optional linkage metadata so an external KYC/party
-- system can tie an account back to a customer record it owns. Both columns
-- are nullable and carry no default, so every existing account keeps
-- party_reference and party_type NULL (equivalent to "no linkage supplied"),
-- posting and reading exactly as it did before this migration.
--
-- Neither column is validated by a CHECK constraint: the actual KYC/party
-- system is external to this service, and beyond a length cap (enforced in
-- application code, domain.Account.Validate) there is no format or taxonomy
-- for the database to usefully enforce. party_type is deliberately free text
-- rather than an enum ('individual', 'business', ...): the real taxonomy is
-- the external party system's, and a CHECK here would just have to be kept
-- in sync with it.

-- party_reference is the external customer/party id (KYC linkage), nullable.
ALTER TABLE accounts ADD COLUMN party_reference text;

-- party_type is a free-text classification of the linked party, for example
-- 'individual' or 'business', nullable.
ALTER TABLE accounts ADD COLUMN party_type text;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE accounts DROP COLUMN party_type;
ALTER TABLE accounts DROP COLUMN party_reference;
-- +goose StatementEnd

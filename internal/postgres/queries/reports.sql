-- name: TrialBalanceByCurrency :many
-- Task 6.3, audit A9.2: the double-entry balance proof. Each currency's net
-- posted total across every account in the tenant; in a correct ledger every
-- total is zero (ADR-001).
SELECT currency, SUM(amount)::bigint AS net
FROM postings
WHERE tenant_id = $1
GROUP BY currency;

-- name: TrialBalanceAccounts :many
-- Task 6.3, audit A9.2: every account's derived balance, including system
-- (FX clearing) accounts, which hold the FX position and are part of the
-- balance proof. The LEFT JOIN means an account with no postings yet still
-- returns one row, with balance COALESCEd to 0, the same shape
-- AccountBalances (postings.sql) already uses.
SELECT a.id, a.name, a.type, a.currency, a.is_system,
       COALESCE(SUM(p.amount), 0)::bigint AS balance
FROM accounts a
LEFT JOIN postings p ON p.tenant_id = a.tenant_id AND p.account_id = a.id
WHERE a.tenant_id = $1
GROUP BY a.id, a.name, a.type, a.currency, a.is_system
ORDER BY a.name, a.id
-- Bounded read (audit remediation): one more than ledger.MaxReportRows so the
-- service can refuse an over-large trial balance rather than build it unbounded
-- in memory. Keep in sync with ledger.MaxReportRows.
LIMIT 10001;

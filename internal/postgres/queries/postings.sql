-- name: CreatePosting :exec
INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount, currency, description)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListPostingsByTransaction :many
SELECT id, tenant_id, transaction_id, account_id, amount, currency, description, created_at
FROM postings
WHERE tenant_id = $1 AND transaction_id = $2
ORDER BY created_at, id;

-- name: AccountBalance :one
SELECT COALESCE(SUM(amount), 0)::bigint AS balance
FROM postings
WHERE tenant_id = $1 AND account_id = $2;

-- name: AccountStatement :many
-- Postings affecting an account, newest first, each with the running balance as
-- of that posting. The running balance is a window SUM over the account's full
-- posting history (the CTE); the keyset filter and limit then return one page.
-- after_created_at / after_id are the keyset position: pass a far-future
-- timestamp and the max uuid for the first page.
WITH entries AS (
    SELECT
        id,
        transaction_id,
        amount,
        description,
        created_at,
        (SUM(amount) OVER (ORDER BY created_at, id))::bigint AS running_balance
    FROM postings
    WHERE tenant_id = $1 AND account_id = $2
)
SELECT id, transaction_id, amount, running_balance, description, created_at
FROM entries
WHERE created_at < sqlc.arg(after_created_at)
   OR (created_at = sqlc.arg(after_created_at) AND id < sqlc.arg(after_id))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: TenantDailyDebits :many
-- Task 2.4b (audit A3.4): each currency's already-posted debit total for
-- today (date_trunc('day', now()), the DATABASE SERVER's clock, so it lines
-- up with the same clock that stamped created_at on every posting). Read
-- from inside the caller's SERIALIZABLE RunInTx body, under the per-tenant
-- in-process serialization (ADR-012), so a daily-volume policy check is race
-- free: two concurrent posts for the same tenant can never both read this
-- same total and both post believing they are under the cap. Only positive
-- amounts (debits, ADR-002) are summed: a transaction's credits are the
-- mirror image of its debits within each currency (the balance invariant),
-- so counting only debits avoids double-counting the same movement.
SELECT currency, SUM(amount)::bigint AS total
FROM postings
WHERE tenant_id = $1
  AND amount > 0
  AND created_at >= date_trunc('day', now())
GROUP BY currency;

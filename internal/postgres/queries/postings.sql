-- name: CreatePosting :exec
INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount, currency, description)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListPostingsByTransaction :many
SELECT id, tenant_id, transaction_id, account_id, amount, currency, description, created_at
FROM postings
WHERE tenant_id = $1 AND transaction_id = $2
ORDER BY created_at, id;

-- name: ListPostingsByTransactionIDs :many
-- Batch posting fetch for a page of transactions (Task 4.4, audit A7.2):
-- ListTransactions returns up to a page's worth of transaction rows, and
-- assembling each one's postings via ListPostingsByTransaction one at a time
-- would be N+1 queries for a full page. This fetches every posting for every
-- transaction id in the page in a single round trip; the caller groups rows
-- back by transaction_id (see Repository.ListTransactions). Ordered by
-- transaction_id then (created_at, id), matching ListPostingsByTransaction's
-- own per-transaction order, so grouping by transaction_id yields each
-- transaction's postings already in that transaction's insertion order.
SELECT id, tenant_id, transaction_id, account_id, amount, currency, description, created_at
FROM postings
WHERE tenant_id = sqlc.arg(tenant_id) AND transaction_id = ANY(sqlc.arg(transaction_ids)::uuid[])
ORDER BY transaction_id, created_at, id;

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

-- name: AccountStatementRange :many
-- The per-account period statement export (Task 6.3, audit A9.2): like
-- AccountStatement above, the running balance is a window SUM over the
-- account's FULL posting history (the CTE), so a filtered page still shows
-- each entry's real running balance, not one reset to the filtered window.
-- from_ts/to_ts are optional via sqlc.narg (NULL disables that bound, the
-- same half-open [from, to) convention ListTransactions uses); the outer
-- query applies the date filter and caps the result at row_limit, requested
-- by the caller as one more than the export cap it wants so a truncated
-- export can be detected without a second round trip.
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
WHERE (sqlc.narg('from_ts')::timestamptz IS NULL OR created_at >= sqlc.narg('from_ts'))
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR created_at < sqlc.narg('to_ts'))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

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

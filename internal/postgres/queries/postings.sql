-- name: CreatePosting :exec
INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount, description)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListPostingsByTransaction :many
SELECT id, tenant_id, transaction_id, account_id, amount, description, created_at
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

# go-ledger Bruno collection

An API collection that exercises every v1 endpoint end to end. [Bruno](https://www.usebruno.com)
is a fast, offline, git-friendly API client; the requests are plain `.bru` files
checked into the repo.

## Run it

1. Start the server (needs Postgres): `make run` with `DATABASE_URL` set.
2. Open this folder in Bruno and select the **Local** environment
   (`baseUrl = http://localhost:8080`).
3. Run the requests in order (they are numbered). Earlier requests stash ids
   (`cashId`, `revenueId`, `transactionId`) into variables that later requests use,
   so the happy path flows start to finish: create two accounts, post a balanced
   transaction, then read the account, balance, statement, and transaction back.

Each request carries assertions, so "Run Collection" is a quick smoke test of the
whole API.

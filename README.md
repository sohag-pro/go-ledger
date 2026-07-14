# go-ledger

[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/sohag-pro/go-ledger/badges/coverage.json)](https://github.com/sohag-pro/go-ledger/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/sohag-pro/go-ledger)](https://goreportcard.com/report/github.com/sohag-pro/go-ledger)

**A production-grade, multi-tenant, white-label double-entry payment ledger, built in public.**

Money never appears or disappears. It only moves. go-ledger takes that 500-year-old rule of double-entry bookkeeping and enforces it in code: every transaction is a set of two or more postings whose amounts sum to zero, per currency. Balances are never stored as mutable state; they're derived by summing an immutable, append-only history of postings. If a transaction doesn't balance, it never persists. The service is multi-tenant and white-label from the ground up: any number of tenants, each with their own accounts, keys, and policy, isolated from each other by Postgres row-level security.

Try it live at [go.sohag.pro](https://go.sohag.pro), read [The Ledger Book](#the-ledger-book) for the whole build story, or jump to [the 5-minute tour](#a-5-minute-tour) below.

## The Ledger Book

The whole build is collected into a book: every weekly essay, the senior-level interview questions that stress-test each design decision, and the architecture decision records, in one volume. Free to read, in whatever format you like.

- **PDF:** [the-ledger-book.pdf](https://github.com/sohag-pro/go-ledger/raw/main/the-ledger-book.pdf)
- **EPUB:** [the-ledger-book.epub](https://github.com/sohag-pro/go-ledger/raw/main/the-ledger-book.epub)

Both live at the repo root and rebuild from the blog and Q&A sources. Design decisions themselves live in [docs/adr](docs/adr); start with [ADR-001: why double-entry](docs/adr/001-why-double-entry.md).

## Run it in one command

The fastest way to see a real, populated ledger is Docker Compose. Two profiles are built for newcomers; only run one at a time, both publish `localhost:8080` and `localhost:5432`:

```sh
docker compose --profile demo up
```

Seeded, public admin: a demo tenant with four personal-finance accounts and about 285 backdated transactions, admin panels unlocked with no key needed. The whole database resets every four hours, so treat it as a sandbox, not storage.

```sh
docker compose --profile local up
```

Empty, production-like: a fresh tenant with no data, and a random bootstrap admin key printed once to the app logs on first boot. Grab it from there (see [Get your admin access](#get-your-admin-access) below).

Either way, once it's up:

- **Console:** [http://localhost:8080/console](http://localhost:8080/console), a browser front end over the public API: tenants, keys, accounts, transactions, webhooks, policy, and reporting.
- **Playground:** [http://localhost:8080/playground](http://localhost:8080/playground), an interactive Scalar API explorer generated from the live OpenAPI spec.
- **Health check:** `curl localhost:8080/healthz` returns `{"status":"ok"}`.

## Run from source

Requires Go 1.26+ and a reachable Postgres. Point it at yours with `DATABASE_URL`, then:

```sh
make run
```

If you'd rather not set `DATABASE_URL` up front, build and run the binary directly from a terminal: with no `DATABASE_URL` set and a TTY attached, it walks you through an interactive first-run setup (host, port, database, user, password, or a full connection string), tests the connection, and optionally saves it to a `.env` file for next time.

```sh
make build
./bin/go-ledger
```

## Run a release binary

Once a version is tagged, cross-platform binaries for the server and `ledgerctl` (darwin, linux, windows) are attached to that [GitHub Release](https://github.com/sohag-pro/go-ledger/releases). If no release is published yet, build from source (above) or use Docker Compose. When a release binary is available, download the one for your platform, then:

```sh
./go-ledger
```

Same interactive first-run setup as above. Use `ledgerctl` alongside it for admin tasks (issuing keys, managing tenants) without going through the REST API or the console.

## Get your admin access

- **Demo:** nothing to do, admin panels in the console are public. The demo key `glk_demo_public_key_reset_every_4h` also works directly against the API if you'd rather use curl.
- **Local or a real deployment:** on first boot, if no admin-scoped key exists yet for the default tenant, the server generates one, stores only its hash, and prints the plaintext to the logs exactly once with a "save this now" notice. Copy it from there. If you missed it (or you're bringing up a tenant that isn't the default one), mint a fresh admin key with `ledgerctl`:

  ```sh
  export DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable
  ledgerctl key issue --tenant <default-tenant-id> --name admin --scopes admin
  ```

  Either way, paste the key into the console's "Admin API key" field (top of the page) to unlock the admin panels there, or send it as `Authorization: Bearer <api-key>` on any `/v1` request.

## A 5-minute tour

Every step below has a console click path and the equivalent `curl`/`ledgerctl`. All examples assume `localhost:8080` and an admin key in `$ADMIN_KEY`.

**1. Create a tenant.** Console: Tenants panel, "Create tenant". API:

```sh
curl -X POST localhost:8080/v1/admin/tenants \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "acme-corp"}'
```

**2. Issue a key** for the new tenant. Console: Keys panel, "Issue key". API:

```sh
curl -X POST localhost:8080/v1/admin/keys \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"tenant_id": "<tenant-id>", "name": "acme-api", "scopes": ["read", "post"]}'
```

or with `ledgerctl`:

```sh
ledgerctl key issue --tenant <tenant-id> --name acme-api --scopes read,post
```

The plaintext key is shown exactly once; save it as `$KEY`.

**3. Create an account.** Console: Accounts panel, "New account". API:

```sh
curl -X POST localhost:8080/v1/accounts \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "Checking", "type": "asset", "currency": "USD"}'
```

**4. Post a balanced transaction.** Postings must sum to zero per currency; here a $10 deposit debits the asset account and credits an income account. Console: Transactions panel, "New transaction". API:

```sh
curl -X POST localhost:8080/v1/transactions \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{
    "currency": "USD",
    "postings": [
      {"account_id": "<checking-account-id>", "amount": 1000, "description": "deposit"},
      {"account_id": "<income-account-id>", "amount": -1000, "description": "deposit"}
    ]
  }'
```

The `Idempotency-Key` header is required: retrying with the same key returns the original transaction instead of posting twice.

**5. Read the balance.** Console: click into the account. API:

```sh
curl localhost:8080/v1/accounts/<checking-account-id>/balance -H "Authorization: Bearer $KEY"
```

**6. Add a webhook** to get notified of future transactions. Console: Webhooks panel, "Add webhook". API:

```sh
curl -X POST localhost:8080/v1/admin/webhooks \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"tenant_id": "<tenant-id>", "url": "https://example.com/hooks/ledger", "event_types": ["transaction.created"]}'
```

or with `ledgerctl`:

```sh
ledgerctl webhook add --tenant <tenant-id> --url https://example.com/hooks/ledger --events transaction.created
```

The signing secret (to verify the `X-Ledger-Signature` header on each delivery) is shown exactly once.

**7. View the trial balance,** the double-entry proof that every currency nets to zero across the tenant. Console: Reports panel. API:

```sh
curl localhost:8080/v1/reports/trial-balance -H "Authorization: Bearer $KEY"
```

## Architecture

One place moves money (the domain services), reached identically over REST and
gRPC. Balances are never stored: they are summed from an append-only posting
history, and the zero-sum invariant is enforced both in the domain type and by a
Postgres `CHECK` trigger on the postings table, so no write path can break it.
Every post also writes a transactional outbox row in the same transaction; a
single-leader chainer drains that into a hash-chained, tamper-evident
`audit_log`, which is also the event stream webhooks are delivered from.

```
  CLIENTS                   SERVICE                          STORAGE

  REST (huma)  ┐                                             Postgres
  gRPC         ├──▶  keys, scopes,  ──▶  domain services ──▶  • RLS per tenant
  console/CLI  ┘     throttle            (internal/ledger)    • append-only postings
                                         post · convert ·     • balances = SUM(amount)
                                         reverse · approve    • CHECK trigger: sum = 0
                                         · report
                                         (SERIALIZABLE + retry)
                                              │
                                              │ each post also writes
                                              │ an outbox row (same tx)
                                              ▼
  audit_outbox  ──▶  chainer  ──▶  audit_log      ──▶  webhook fan-out  ──▶  subscribers
  (per event)      (1 leader)    (hash chain,          (1 leader, signed,     (HTTPS,
                                  tamper-evident)        at-least-once)         dedup by id)

  Observability:  OpenTelemetry traces · slog (JSON) · Prometheus metrics
```

## Features

- **Tenants with a status lifecycle:** active, suspended, closed, each fully isolated by Postgres row-level security so one tenant can never see another's data even through a bug.
- **API keys with scopes, expiry, and rotation:** `read`, `post`, `approve`, `admin`, an optional expiry, and a rotate flow that keeps the old key live for an overlap window.
- **Multi-currency accounts and FX conversion,** env-configured rates or an optional free live rate feed with a staleness guard, four-leg atomic conversion through per-currency clearing accounts, and USD-hub triangulation for cross pairs. See [ADR-014](docs/adr/014-multi-currency-and-fx.md), [ADR-022](docs/adr/022-usd-hub-fx-triangulation.md), and [ADR-026](docs/adr/026-security-and-correctness-hardening.md).
- **Approval workflows:** over-threshold transactions are held as pending intent (no postings, balances untouched) until an approver replays them against current balances, with an optional per-key four-eyes control. See [ADR-025](docs/adr/025-approval-workflows-and-lifecycle-events.md).
- **Account hierarchy and rollup reporting:** nested accounts with balances rolled up over each subtree. See [ADR-023](docs/adr/023-account-hierarchy-and-rollup.md).
- **Transaction reversal,** posting the exact inverse of an existing transaction, idempotent against re-reversal.
- **External reference and value date:** an optional reconciliation id (unique per tenant) and an `effective_at` distinct from post time.
- **Mandatory idempotency keys** on every posting endpoint, with a bounded, server-configured replay window. See [ADR-012](docs/adr/012-api-authentication-and-hardening.md).
- **Webhooks:** signed (HMAC over a timestamped body, so a receiver can reject replays), retried with backoff, at-least-once delivery of ledger events to a tenant's registered URLs, fanned out off the same tamper-evident event stream, with an SSRF egress guard. See [ADR-027](docs/adr/027-webhooks.md).
- **Per-tenant policy:** an optional max transaction amount, daily volume cap, and currency allowlist, enforced per currency.
- **Row-level security (RLS)** at the Postgres layer as the backstop for tenant isolation, not just application-layer checks.
- **PII crypto-shredding:** an irreversible per-tenant erasure of posting-description encryption keys, reconciling the right to erasure with an append-only ledger. See [ADR-018](docs/adr/018-pii-crypto-shredding.md).
- **Tamper-evident audit chain:** every posting is hash-chained so any retroactive edit is cryptographically detectable, safe under multiple app instances via a transactional outbox and single chainer. See [ADR-012](docs/adr/012-api-authentication-and-hardening.md) and [ADR-017](docs/adr/017-multi-instance-audit-chain.md).
- **Reporting and disputes:** a trial balance endpoint (the balance proof), account statements, CSV export, and a disputes workflow over posted transactions.

## Project structure

```
go-ledger/
├── cmd/
│   ├── server/        # main.go: wiring, config, migrations, graceful shutdown. No business logic.
│   └── ledgerctl/      # operator CLI over the admin surface: tenants, keys, rates, webhooks
├── internal/          # the heart of the project; domain and service code, unimportable from outside
│   ├── domain/         # the double-entry model: Transaction, Posting, Account, Validate()
│   ├── api/            # REST handlers (huma), OpenAPI generation, the Scalar playground
│   ├── web/             # the /console operator console (thin client, no server-side logic of its own)
│   └── postgres/       # repository layer, goose migrations, sqlc-generated queries
├── docs/
│   └── adr/            # architecture decision records: why double-entry, schema, auth, FX, RLS, PII...
├── api/
│   └── openapi.yaml    # committed OpenAPI spec, regenerated with `make openapi`
├── Makefile            # run, build, test, lint, dev, migrations
├── docker-compose.yml   # demo, local, dev, and load-test profiles
├── Dockerfile           # multi-stage build, distroless base
├── .golangci.yml        # lint config (gofumpt, gosec, and friends)
└── .air.toml            # hot reload for local dev
```

## Development

| Command | What it does |
|---|---|
| `make run` | Run the server |
| `make build` | Build binary to `bin/` |
| `make test` | Run tests with race detector |
| `make cover` | Run coverage and enforce the coverage floors |
| `make lint` | Run golangci-lint |
| `make dev` | Hot reload via [air](https://github.com/air-verse/air) |
| `make migrate-up` | Apply all Postgres migrations (needs `DATABASE_URL`) |
| `make openapi` | Regenerate the committed OpenAPI spec |

Every significant design decision gets an architecture decision record before the code that implements it; see [docs/adr](docs/adr). Read one before proposing an alternative it already considered and rejected.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for prerequisites, running the test suite (including the Postgres integration tests), migrations, and the ADR-first convention this project follows.

## License

[MIT](LICENSE)

# go-ledger

**A production-grade payment ledger service in Go, built in public over twelve weeks.**

Money never appears or disappears. It only moves. go-ledger takes that 500-year-old rule of double-entry bookkeeping and enforces it with Go's type system, Postgres constraints, and tests that throw 10,000 concurrent transactions at the ledger and expect zero violations. Every balance is the sum of an immutable, append-only history; if a transaction doesn't sum to zero, it never persists.

This is the same pattern at the core of every fintech company, built small enough to read in an afternoon and serious enough to run in production: REST and gRPC APIs, idempotency keys, an immutable audit log, OpenTelemetry tracing, and a real CI/CD deployment.

📖 **Follow the build:** every week ships with a blog post explaining the why behind the code, at [notes.sohag.pro/series/go-ledger](https://notes.sohag.pro/series/go-ledger).

## Quickstart

Requires Go 1.26+.

```sh
make run
```

Then:

```sh
curl localhost:8080/healthz
# {"status":"ok"}
```

## Project structure

```
go-ledger/
├── cmd/
│   └── server/        # main.go: wiring, config, graceful shutdown. No business logic.
├── internal/          # the heart of the project; domain and service code, unimportable from outside
├── pkg/               # public packages, only if any earn the spot
├── docs/
│   └── adr/           # architecture decision records: why double-entry, money representation, schema
├── Makefile           # run, build, test, lint, dev
├── Dockerfile         # multi-stage build, distroless base
├── .golangci.yml      # lint config (gofumpt, gosec, and friends)
└── .air.toml          # hot reload for local dev
```

Design decisions live in [docs/adr](docs/adr). Start with [ADR-001: why double-entry](docs/adr/001-why-double-entry.md).

## Development

| Command | What it does |
|---|---|
| `make run` | Run the server |
| `make build` | Build binary to `bin/` |
| `make test` | Run tests with race detector |
| `make lint` | Run golangci-lint |
| `make dev` | Hot reload via [air](https://github.com/air-verse/air) |

## Roadmap

Built over a 12-week public roadmap: domain model, Postgres schema, atomic transaction posting under SERIALIZABLE isolation, REST + gRPC APIs, idempotency keys, audit log, OpenTelemetry, load and chaos testing, then a live deployment via GitHub Actions. The [blog series](https://notes.sohag.pro/series/go-ledger) tracks each week, including what went wrong.

Deliberately out of scope for v1: multi-currency, FX, webhooks, web UI, event streaming. Shipping beats feature lists.

## License

[MIT](LICENSE)

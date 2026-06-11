# go-ledger

A production-grade payment ledger service in Go, built on double-entry
accounting. Work in progress; see [docs/adr](docs/adr) for design decisions.

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

## Development

| Command | What it does |
|---|---|
| `make run` | Run the server |
| `make build` | Build binary to `bin/` |
| `make test` | Run tests with race detector |
| `make lint` | Run golangci-lint |
| `make dev` | Hot reload via [air](https://github.com/air-verse/air) |

## License

[MIT](LICENSE)

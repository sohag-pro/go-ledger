# Contributing to go-ledger

Thanks for looking at go-ledger. This is a personal, build-in-public project, but issues, bug reports, and pull requests are welcome. This doc covers what you need locally and the conventions the codebase follows.

## Prerequisites

- **Go 1.26+.** Check `go.mod` for the exact minimum.
- **Docker (Docker Desktop or colima)** for the Postgres integration tests, which run against real containers via testcontainers-go. If you use colima instead of Docker Desktop, testcontainers-go does not auto-detect its socket, so set these before running tests:

  ```sh
  colima start
  export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
  export TESTCONTAINERS_RYUK_DISABLED=true
  ```

  Without a reachable Docker daemon, the integration tests skip rather than fail, so `make test` still runs clean on a machine with no Docker at all. You'll just be testing less.
- **golangci-lint** and **air** on `PATH` for `make lint` and `make dev`. If you installed Go tools the usual way, they land in `~/go/bin`.

## Building and running

```sh
make run    # run the server against DATABASE_URL
make build  # build to bin/go-ledger
make dev    # hot reload via air
```

See the [README](README.md) for the full set of ways to run the service, including Docker Compose profiles and the interactive first-run setup.

## Tests, coverage, and lint

```sh
make test    # go test -race -cover ./...
make cover    # coverage with enforced floors (needs Docker for full numbers)
make lint    # golangci-lint run ./...
```

A pull request should pass all three. Tests are table-driven; anything touching the balance invariant (postings summing to zero) also gets a property-style randomized test, not just fixed cases. Lint config is `.golangci.yml` (v2 format, gofumpt formatting, gosec enabled); run `make lint` before opening a PR, it should be clean.

To run a single test:

```sh
go test -race -run TestName ./internal/path/...
```

## Migrations

Schema migrations live in `internal/postgres/migrations` and are managed with [goose](https://github.com/pressly/goose). Add a new migration as a new numbered `.sql` file there, following the existing naming and format.

- `make migrate-up` applies every pending migration against `DATABASE_URL`.
- `make migrate-down` rolls back the most recent one.
- The server binary itself also embeds the same migrations and can apply them directly: `go-ledger migrate` (or `go-ledger migrate status` to check the current schema version with no changes). This is the exact step the deploy pipeline runs against a new binary before swapping it into place, and it's what the `demo` and `local` Docker Compose profiles use as their init step.

## The ADR-first convention

Every significant design decision in this codebase gets an architecture decision record in `docs/adr/NNN-*.md`, written **before** the plan and the code, not after. If you're proposing a change that involves a real design choice (a schema change, a new invariant, a new external dependency, anything with a "why this and not that"), read the existing ADRs first: the decision you're about to propose may already have been considered and rejected, with the reasoning recorded. If it's genuinely new ground, a PR that changes behavior along those lines should come with an ADR, following the format of the existing ones.

## Writing style

These rules apply to all text in this repo: code comments, commit messages, ADRs, and docs.

- **No em dashes or en dashes anywhere** (U+2014, U+2013). Use a comma, a period, parentheses, "to" for numeric ranges, or a colon instead. Regular hyphens in compound words ("double-entry", "append-only") are fine. Before submitting, check with:

  ```sh
  rg -uu '[\x{2013}\x{2014}]' -g '!.git' -g '!bin' -g '!**/scalar.js'
  ```

  That should return nothing (the vendored Scalar bundle is the one deliberate exception).
- Write plainly and directly, the way you'd explain something to a teammate. Avoid stiff filler ("delve", "moreover", "it's worth noting").

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/) style (`fix:`, `feat:`, `docs:`, `test:`, and so on), same as the existing history. Keep the subject line short and explain the "why" in the body when it isn't obvious from the diff.

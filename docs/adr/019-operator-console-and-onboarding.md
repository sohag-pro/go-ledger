# ADR-019: Operator Console, Public-Demo Admin, and One-Command Onboarding

This ADR records a deliberate reversal of an earlier scope rule and the design of
the developer-experience work that follows from it: turning the thin developer
console into an operator console with admin panels, exposing admin publicly in
demo mode, a one-command run experience, an interactive first-run setup for the
binary, and cross-platform release binaries.

## Status

Accepted: 2026-07-11

## Context

The remediation (ADR-015 through ADR-018) turned go-ledger into a genuine
multi-tenant, white-label money core: a tenant entity, API-key lifecycle, an
admin REST surface plus a `ledgerctl` CLI, webhooks, per-tenant policy, RLS,
disputes, and reporting. None of that is discoverable to a newcomer. The README
is a four-line `make run` quickstart. The `/console` is a thin developer tool that
hardcodes the public demo key and only does accounts and transactions. There is
no packaged way to run the system (it needs Postgres), and no released binaries.

The original scope discipline (in CLAUDE.md) explicitly listed "product web UI /
admin dashboard" as out of scope and said to keep the console "thin and
developer-facing." That rule made sense while the ledger was single-tenant. Now
that the system is white-label and multi-tenant, the lack of any browseable way to
onboard a tenant, issue a key, or set up a webhook is the main barrier to anyone
actually running it. The rule has outlived its purpose.

## Decision

### 1. The console becomes an operator console (reverses the "no admin dashboard" rule)

`/console` gains admin and management panels on top of the existing ledger views:
tenants (create, list, suspend/close), API keys (issue, list, rotate, revoke),
webhook subscriptions, per-tenant policy, and read-only reporting (trial balance,
transaction list, disputes), all scoped to a selected tenant. This is a
deliberate, recorded reversal of the earlier "keep the console thin, no admin
dashboard" rule: the operator console is now an in-scope product surface. It stays
a thin client over the existing public `/v1` and `/v1/admin` APIs (no server-side
business logic moves into it); it is a browseable front end for capabilities that
already exist, not a new capability.

### 2. Admin auth: public in demo, key-gated in prod

The console's admin panels unlock based on the deployment mode:

- **Demo mode** (`DEMO_MODE=true`, the public go.sohag.pro deployment): admin is
  **public, no key required**. The whole database resets on the seeder interval
  (hourly by default) and the
  demo key is rate limited, so there is nothing durable to protect and no
  privilege worth stealing. To make the console's admin calls work with the public
  demo key, the demo key is elevated to include the `admin` scope in demo mode
  only. The accepted risk is that anyone can create tenants and keys on the demo
  between resets; it is bounded by the demo key's low rate limit and the hourly
  wipe, and it never touches a real deployment.
- **Production mode** (`DEMO_MODE` unset/false): admin panels stay hidden until the
  operator enters an **admin-scoped API key** in the console (stored in the
  browser's `localStorage`). The UI gate is convenience only; the server already
  enforces scope on every `/v1/admin` route, so a non-admin key cannot perform
  admin actions regardless of what the UI shows.

The console learns which mode it is in from a small unauthenticated, read-only
endpoint `GET /console/config` returning `{demo_mode, default_tenant_id}`.

### 3. First-boot admin provisioning

So a self-hoster is not locked out of their own admin surface, the server
provisions an admin credential on first boot:

- In demo mode: the demo key is provisioned with `admin` scope (as above).
- In production mode: if no admin-scoped key exists for the default tenant, the
  server generates a random admin-scoped key, stores it (hash only), and prints
  the plaintext **once** to the logs with a clear "save this now" notice. If an
  admin key already exists, nothing is printed. `ledgerctl` remains the alternate
  path to mint admin keys.

### 4. One-command run: docker compose with two profiles

The primary newcomer experience is a one-line `docker compose` invocation. Both
newcomer stacks sit behind an explicit profile, because Compose always starts
profile-less services and the pre-existing `dev` and `load-test` stacks would
otherwise collide on ports:

- `docker compose --profile demo up`: `DEMO_MODE=true`, the seeder on, so a
  newcomer immediately sees a populated demo tenant with public admin.
- `docker compose --profile local up`: an empty, production-like ledger with a
  printed random admin key.

`local` is a named profile like any other, not a default. A bare `docker compose
up` starts only the profile-less services (Jaeger), which is not a running
ledger, so the README quotes the `--profile` form in both cases.

Both bring up Postgres and the app and apply migrations as an init step (the
`migrate` subcommand from ADR-017's pre-swap step, reused). The README documents
both, plus the from-source and release-binary paths.

### 5. Interactive first-run setup for the binary

When the server binary is run directly with no `DATABASE_URL` **and a terminal is
attached (stdin is a TTY)**, it runs an interactive setup: it prompts for the
Postgres connection (host, port, database, user, password, or a full URL, and
sslmode), tests the connection, and offers to save it to a config file for next
time, then boots. When there is no TTY (docker, systemd, CI), it keeps today's
fail-fast behavior: a missing `DATABASE_URL` is a clear fatal error, never a hang
waiting on input. This makes "download the binary and run it" work for a newcomer
without making the containerized/service path interactive.

### 6. Cross-platform release binaries via GoReleaser

Releases publish prebuilt binaries with GoReleaser, triggered by a version tag in
GitHub Actions: the **server** and **`ledgerctl`**, for darwin (amd64 and arm64),
linux (amd64 and arm64), and windows (amd64), with checksums and a generated
changelog, attached to the GitHub Release. The download-and-run instructions live
in the refreshed README.

## Consequences

- A newcomer can go from zero to a running, populated, browseable ledger with one
  command, and from a downloaded binary with a guided setup. That is the point.
- The console is no longer thin or purely developer-facing; it is an operator
  admin UI. This is a real scope expansion, recorded here so the reversal is
  explicit and not an accident. The corresponding CLAUDE.md scope note is updated.
- Demo mode now exposes admin publicly. This is safe only because of the demo's
  hourly reset and rate limits; it must never be enabled on a deployment that
  holds real data. The `DEMO_MODE`-in-production guard (ADR-015 Phase 0) refuses
  that combination at boot, but it is worth being precise about how much that
  guard actually covers, because this ADR originally leaned on it as the thing
  "that makes this safe to adopt." The guard fires only when `APP_ENV` is
  exactly `"production"`, and `APP_ENV` defaults to `"development"`. It is
  therefore **opt-in**: a self-hoster who sets `DEMO_MODE=true` and never sets
  `APP_ENV` boots with a publicly reachable, unauthenticated admin surface over
  their real data, and nothing stops them. The real control is the operator
  knowing not to enable demo mode outside a demo; the boot guard is a backstop
  for the one environment that declares itself production, not a general safety
  net.
- The console remains a thin client with no server-side logic of its own, so it
  cannot become a second, divergent implementation of any rule; it can only call
  the same authenticated, scope-checked, RLS-protected APIs everything else uses.
- Interactive setup adds a TTY-gated code path to the server's startup; it must be
  strictly gated so the non-interactive path (the one that runs in production) is
  byte-for-byte the current fail-fast behavior.
- GoReleaser and a tag-triggered workflow add a release process; a bad release is
  contained to the artifacts and does not touch the running VPS deploy (a separate
  workflow).
- The work is phased (run foundation, then console, then README and playground,
  then release binaries) so each phase is independently shippable and the README
  documents capabilities that are already real when it is written.

# ADR-013: VPS Infrastructure-as-Code with Ansible, and Docker as a Dev-Only Artifact

## Status

Accepted: 2026-07-09

Supersedes the Week 10 framing in the (gitignored) build plan, which still
describes Terraform modules and an AWS ECS/RDS deploy. That framing was
overtaken by the deployment decision recorded when the service went live.

## Context

The 14-week plan's Week 10 targets AWS: a multi-stage Dockerfile plus Terraform
modules (VPC, ECS Fargate, RDS Postgres, ALB, IAM, ECR) and a `make deploy` that
stands the ledger up on AWS in one command. Two things have made that plan
stale:

1. The locked deployment decision is a single VPS driven by GitHub Actions, not
   AWS. The service has run in production at https://go.sohag.pro since
   2026-06-12, and the CI/CD pipeline the plan scheduled for Week 11 already
   ships every push to `main`.
2. How prod actually runs is settled and deliberately un-containerized. It is a
   single small VPS running Ubuntu LTS, shared with one other low-traffic site.
   The ledger runs as a **bare prebuilt binary** under a hardened
   `go-ledger.service` systemd unit. Postgres 16 is installed on the same box,
   memory-tuned for the host's modest RAM, and backed up with pg_dump. Docker is
   not installed on the server; all building happens in CI, which ships the
   binary to a fixed install directory.

So the AWS half of the planned Week 10 is dead on arrival, and the deploy half is
already done. What genuinely remains, and fits the VPS decision, is two gaps:

- The Dockerfile is still a skeleton (its own comment says "fleshed out in Week
  10 (distroless, <20MB target)").
- The VPS was provisioned by hand as root over SSH and captured only as prose in
  the gitignored `docs/ops/server-setup.md`. There is no executable,
  version-controlled way to reproduce or audit the box.

This ADR records how Week 10 closes those gaps without relitigating the VPS
decision.

## Decision

### 1. No Terraform, no AWS

The Terraform modules and the AWS ECS/RDS/ALB/ECR deploy are dropped, not
deferred. Writing Terraform for infrastructure that will never be provisioned is
throwaway work that would also invite drift between a fictional AWS topology and
the real box. The plan file's Week 10 is treated as superseded by this ADR.

### 2. Docker is a dev-and-CI artifact, never prod

The multi-stage Dockerfile produces a distroless image under 20 MB, used for
local development and CI (build and load test). Prod is unaffected: it stays a
bare systemd binary against a co-located Postgres. We do not add a Docker daemon
to the shared box, and we do not run compose in prod.

Rationale: the box's RAM is split between the app, Postgres, and the other site
it hosts. A container runtime is pure overhead there, and the bare-binary +
systemd model already gives strong isolation (NoNewPrivileges, ProtectSystem=
strict, the full hardening block) at zero memory cost. Docker earns its keep for
reproducible local dev and for the load-test stack, so that is where it lives.

The Dockerfile is hardened to be real rather than skeletal: `go.sum` is copied
before `go mod download` so the dependency layer is pinned and cacheable;
BuildKit cache mounts speed rebuilds; `-trimpath -ldflags "-s -w"` shrink the
binary; the distroless base is pinned by digest and runs non-root. A
`make image-size` target asserts the image is under 20 MB. The target passes on
merit: the binary is ~9 to 10 MB (it embeds the Scalar playground bundle) and
distroless-static adds ~2 MB.

### 3. The local dev stack is compose

`docker compose --profile dev up` brings up the full local stack: app, Postgres,
Jaeger (traces), and Prometheus (scraping the app's existing `/metrics`). It
carries a `dev` profile, kept separate from the `load-test` profile (tuned for
the multi-tenant audit-chain k6 run), because Docker Compose always starts
profile-less services: without its own profile the dev stack would boot during a
`--profile load-test` run too and collide on ports. This is local-only; nothing
here reaches prod.

### 4. VPS provisioning is codified as an idempotent Ansible playbook

The prose in `server-setup.md` becomes an executable, re-runnable Ansible
playbook under `infra/ansible/`, split into roles that mirror the runbook: base
(ufw, fail2ban, unattended upgrades), postgres (Postgres 16, role and
database, small-host memory tuning, pg_dump backup cron), app (the install
directory, root-owned env file, deploy user, restricted sudoers), systemd (the
hardened unit), nginx (the service vhost with gzip), and tls (certbot).

Ansible over the alternatives:

- **Over a plain bash provision script**: Ansible is idempotent and re-runnable
  by construction, and its roles document the box's desired state more clearly
  than an accretion of `if not exists` shell. The box was built by hand once;
  the value now is a state description that stays true, not a replay of the
  original keystrokes.
- **Over Terraform**: Terraform provisions cloud resources. There are none. The
  work here is configuring an existing host, which is configuration management,
  Ansible's job, not infrastructure provisioning.

### 5. The public repo leaks nothing about the live box

The repo is public. The roles and playbook are tracked and generic: no IP, no
secrets, no portfolio-specific detail beyond what is already public. The real
inventory (the VPS IP), the vault (the Postgres password and the deploy private
key), and any host-specific vars are gitignored, exactly as `server-setup.md`
is. Committed `*.example` files show the shape without the secrets. The public
repo gets reproducible IaC; the live box's address and credentials stay out of
it.

The nginx role runs on a shared host, so a bad config or a failed reload is not
contained to this service. The role therefore validates with `nginx -t`, reloads
rather than restarts, and asserts that the other site on the box still returns
200 after any change. Re-running the playbook against the live box is deliberate,
not casual: the README documents a check-mode-first workflow.

## Consequences

- Week 10 produces a real container image and a reproducible, auditable
  description of the production host, both consistent with the VPS decision.
- The plan file's Week 10 and its AWS blog title are formally void; the actual
  blog documents Docker plus Ansible IaC on a VPS.
- Prod remains un-containerized. If the service ever outgrows one box, the
  bare-binary model is what would be revisited, and the Ansible roles are the
  starting point for provisioning additional hosts.
- The Ansible playbook is a control-machine dependency (Ansible must be
  installed to run it) and is validated in check mode and by idempotent re-runs;
  a full from-scratch provision is exercised manually, never against a throwaway
  host in CI.
- Anyone with the repo can read exactly how the box is built, but not where it
  is or how to log in.

# go-ledger VPS provisioning

Idempotent Ansible playbook that reproduces the production VPS from
`docs/ops/server-setup.md`. Docker is not part of prod: the server runs a bare
`go-ledger` binary under systemd against a co-located Postgres 16. See ADR-013.

## What is tracked and what is not

Tracked (public, generic): `playbook.yml`, `roles/`, `ansible.cfg`, the
`*.example` files. Never tracked (gitignored): `inventory` (the VPS address),
`group_vars/all.yml` and any vault (the Postgres password, the deploy key).

## Setup

    cp inventory.example inventory                 # fill in the real host
    cp group_vars/all.example.yml group_vars/all.yml   # fill in real values
    pip install ansible ansible-lint               # or brew install ansible

## Run

Always dry-run first. This host is shared with the portfolio at sohag.pro,
which uses HSTS preload, so a broken nginx or cert takes the portfolio down too.

    ansible-playbook playbook.yml --check --diff   # dry run, no changes
    ansible-playbook playbook.yml                  # apply

The nginx role validates with `nginx -t`, reloads (never restarts) nginx, and
asserts `https://sohag.pro/` still returns 200. Re-running the whole playbook is
safe: it is idempotent and a second run reports no changes.

## Verify

    ansible-playbook playbook.yml --syntax-check
    ansible-lint

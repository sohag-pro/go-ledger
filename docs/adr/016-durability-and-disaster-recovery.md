# ADR-016: Durability and disaster recovery (WAL archiving, offsite encrypted backups, PITR)

Status: Accepted
Date: 2026-07-10
Supersedes the backup portion of the Week 10 VPS setup (the same-disk daily
`pg_dump`). Referenced by ADR-015 (audit remediation, Phase 1) and closes audit
finding A4.1 (Blocker), with A8.1 (restore drill).

## Context

The ledger is the system of record for money. The Week 10 deployment backs it up
with a single mechanism: a `cron.daily` job that runs `pg_dump | gzip` into
`/var/backups/go-ledger` on the same VPS disk as the database, keeping 14 days.

For a hobby service that is fine. For a production, white-label money core it is
disqualifying, and the audit flagged it as a Blocker:

- **Same disk.** If the disk or the VPS is lost, the database and every backup
  are lost together. There is no offsite copy.
- **No point-in-time recovery.** A logical dump is a snapshot at dump time. Any
  write between the nightly dump and a failure is gone: up to ~24 hours of
  posted transactions. For a ledger that is unbounded data loss.
- **Not encrypted.** The dumps are plaintext money data at rest.
- **Never restored.** A backup that has never been restored is a hope, not a
  recovery plan. There is no proof the dumps are usable.

A ledger's durability bar is specific: no committed transaction may ever be
silently lost, and recovery must be a rehearsed, bounded procedure, not an
improvisation during an outage.

## Decision

### 1. Tool: pgBackRest

Adopt **pgBackRest** as the backup and recovery system, replacing the same-disk
`pg_dump` as the durability mechanism. pgBackRest gives us, in one tool:
continuous WAL archiving, full plus differential physical base backups,
point-in-time recovery, repository-level AES-256 encryption, retention
management, integrity checksums, and a single-command restore.

The alternative considered was WAL-G. Both are solid. pgBackRest wins here for
its simpler configuration, first-class differential backups, built-in encryption
and retention, and a restore path that is one command rather than a scripted
sequence. Neither choice is load-bearing on the rest of the system; the ledger
does not know how it is backed up.

### 2. Topology: co-located archiving to encrypted offsite object storage

Now: PostgreSQL on the VPS ships WAL and base backups to an S3-compatible object
store in a **different failure domain** (a separate provider or region from the
VPS), with pgBackRest repository encryption on, so the offsite copy is ciphertext
at rest. WAL archiving is asynchronous (`archive-async`) so archiving never
stalls a commit.

The growth path, explicitly out of scope for this ADR, is a managed Postgres
service (the provider owns WAL archiving, PITR, and replicas) when the service
outgrows a single VPS. ADR-015's Phase 3 (multi-instance) is the trigger to
revisit; a managed primary with a read replica also addresses availability
(A4.2), which this ADR does not.

### 3. Recovery objectives

- **RPO (recovery point objective): <= 5 minutes.** Continuous WAL archiving with
  `archive_timeout = 60s` bounds the worst-case loss to roughly the last minute
  of WAL plus archive latency. In practice a busy ledger fills and ships WAL
  segments continuously, so the real RPO is seconds.
- **RTO (recovery time objective): <= 1 hour.** The database is small (a 1 GB
  shared box). A restore of the latest base backup plus WAL replay to the target
  time, followed by the invariant verification (objective 5), completes well
  inside an hour. The runbook step sequence is written down, not improvised.

### 4. Schedule and retention

- **Full backup weekly**, **differential backup daily**, **WAL archived
  continuously** via `archive_command = pgbackrest ... archive-push`.
- **Retention:** keep 2 full backups (a two-week PITR window) with their
  differentials and the WAL needed to recover to any point within that window;
  pgBackRest expires older backups and their WAL automatically.
- Schedule via systemd timers (or cron); the timers run `pgbackrest backup`
  with `--type=full` weekly and `--type=diff` daily.

A local logical `pg_dump` is retained as a convenience for quick, small,
same-box restores and inspection, but it is explicitly **not** the disaster
recovery path and is documented as such. The offsite encrypted pgBackRest
repository is the system of record for recovery.

### 5. Automated restore-and-verify

A backup is not trusted until a restore has been proven, so a scheduled job
**restores the latest offsite backup into a throwaway PostgreSQL instance and
verifies the ledger's own invariants against the restored data**:

- every account's balance derived from its postings is internally consistent and
  every transaction still sums to zero per currency (the core invariant), and
- every tenant's tamper-evident audit hash chain verifies end to end
  (`audit/verify`).

The job runs **weekly in CI** (GitHub Actions), pulling from the offsite
repository with a **read-only** credential, so it exercises the real offsite copy
from a different machine than the one that wrote it, and it fails loudly (red
build, alert) on any mismatch or any inability to restore. Running it off-box is
deliberate: it proves the copy that would survive losing the VPS is actually
restorable, which is the only thing that matters in a disaster.

The RPO and RTO, the restore runbook, and the verify job are documented in
`docs/ops/server-setup.md`.

## Consequences

- The single worst production risk, unbounded data loss on disk failure, is
  removed. Recovery becomes a bounded, rehearsed, off-box procedure.
- **Operational prerequisites the code cannot provide** (owner action):
  provision an S3-compatible bucket in a different failure domain from the VPS;
  set the pgBackRest repository credentials and the AES-256 encryption
  passphrase as secrets on the box (not in git); set a read-only repository
  credential as a CI secret for the restore-verify job. Until the bucket and
  secrets exist, the Ansible role and scripts are inert configuration. This is
  called out in the Phase 1 plan and the runbook.
- Cost: a few dollars a month of object storage and egress, and the CI minutes
  for the weekly restore-verify. Negligible against the risk removed.
- WAL archiving adds a small, continuous write and network load on the box; the
  asynchronous archiver keeps it off the commit path.
- The encryption passphrase becomes a recovery dependency: losing it makes the
  offsite repository unrecoverable, so it is stored in the operator's secret
  manager alongside the object-store credentials, documented in the runbook.
- Availability (a hot standby, automated failover) is still out of scope; this
  ADR is about not losing data, not about staying up. A4.2 is deferred to the
  managed-Postgres growth path.

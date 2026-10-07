# BackupProof

**Backups you can prove.** BackupProof is a self-hosted backup system for servers, applications and databases. Every backup produces signed evidence. Every restore drill produces signed evidence that the data was *actually restored and verified*. All of it is chained into a tamper-evident ledger that an auditor can verify offline, without trusting BackupProof.

> A green "backup succeeded" tells you a job ran. It doesn't tell you the data comes back.
> BackupProof shows a source as **Proven** only after it has been restored from storage into an isolated sandbox and checked.

This is a ground-up rewrite (in Go) of the original TypeScript BackupProof v13. See [What changed from v13](#what-changed-from-v13).

---

## What it does

| | |
|---|---|
| **Backs up** | File trees, PostgreSQL, MySQL/MariaDB, MongoDB, SQLite, or any command's output. Pre/post hooks let you quiesce applications. |
| **Stores** | In its own deduplicating, encrypted, content-addressed repository format on local disk, any S3-compatible service (AWS, B2, R2, Wasabi, Garage, SeaweedFS…) with optional **S3 Object Lock**, or SFTP with a pinned host key. |
| **Proves** | Scheduled **restore drills** restore the latest snapshot from storage into an empty directory or a `--network none` database container. Checks per data type:<br>• All data: the restored bytes must reproduce the snapshot's Merkle root.<br>• PostgreSQL: `amcheck` with `heapallindexed`.<br>• MySQL/MariaDB: `CHECK TABLE`.<br>• MongoDB: `validate(full)`.<br>• SQLite: `integrity_check`.<br>• Databases: row counts reconciled against counts captured at backup time.<br>• Your own SQL assertions and commands. |
| **Attests** | Each backup and drill (pass *or fail*) becomes an [in-toto](https://in-toto.io) statement in a DSSE envelope, signed with the agent's Ed25519 key. It is optionally timestamped by an RFC 3161 TSA, and appended to a hash-chained ledger with signed checkpoints. |
| **Watches** | A dead-man's-switch watchdog alerts on what *didn't* happen: overdue backups, stale proofs and silent agents. Alerts go to a webhook (Slack, Discord, Mattermost) or email, and the server can ping an external heartbeat URL. |
| **Exports** | Per-proof bundles and a period **evidence pack** mapped to SOC 2 A1.2/A1.3, ISO 27001 A.8.13, NIST CSF, DORA Art. 12, NIS2 Art. 21 and HIPAA. A printable report is included. |

It ships as one static binary: `backupproof server`, `backupproof agent`, and a standalone CLI.

**New here? Read [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md):** a plain-language guide with no backup jargon.

## Easy by default

- **Nothing to install on the dashboard machine.** The server includes an agent ("This server") that protects its own machine.
- **One-line install for other computers.** The dashboard shows a copy-paste command for Linux, macOS or Windows. It downloads the agent, connects it with a one-time code, and keeps it running as a service.
- **Point and click.** A wizard asks *what* (folders, websites, databases), *where* (disk, Backblaze B2, Amazon S3, Cloudflare R2, Wasabi, SFTP) and *how often*.
  - Each computer reports what it found: WordPress sites and their databases, and databases running in Docker. Logins are read on the computer itself.
  - Storage has a **Test connection** button. The encryption password is generated for you, with a downloadable recovery kit.
- **Bring your old backups.** Existing backups in S3, B2, a disk or SFTP are fetched, decrypted and converted into restore-tested copies that keep their original dates:
  - **GPG, OpenSSL and age** encrypted files, opening `.zip` and `.tar.gz` archives;
  - **restic** repositories, reusing the existing `/etc/restic/env` and password file;
  - **Kopia** and **BorgBackup** repositories;
  - files on **Google Drive, Dropbox, OneDrive** and 70+ other services via rclone (including rclone-encrypted folders).

  PostgreSQL dumps found inside are loaded into a test database. See [docs/GUIDE-LUXVPS.md](docs/GUIDE-LUXVPS.md) for a worked example.

## How the proof works

```
backup ──► snapshot ─► Merkle root R (BLAKE3 over path,type,size,content-hash)
              │
              ├─► attestation{backup, subject=R, source meta, storage, lock}  ─┐  signed by the agent
              │                                                                │  (Ed25519, DSSE)
drill  ──► restore from storage ─► recompute root R' from the bytes on disk    │
              ├─► R' == R ?  integrity checks · row reconciliation · asserts   │
              └─► attestation{drill, subject=R, checks, RTO, sandbox image}  ──┤
                                                                               ▼
                         ledger:  hash_n = SHA256(seq,time,kind,subject,envelope_digest,hash_{n-1})
                                  (append-only; DB triggers reject UPDATE/DELETE)
                                  signed checkpoint over the head  ·  optional RFC 3161 token
```

- **The content root is unkeyed.** Blob names are keyed MACs, so storage observers learn nothing about the contents. The Merkle root, by contrast, can be recomputed by *anyone* who holds the restored data. An auditor can match "the data we restored" to "the data the signed statement talks about" without the repository password.
- **Failures are recorded too.** A failed drill is signed and ledgered like a passing one. Deleting it breaks the hash chain.
- **Separation of duties.** A source can name a separate verifier agent. That agent restores using only the repository, on a different machine with its own signing key.
- **Unbiased sampling.** Repository read-data checks pick their sample from the current ledger head, so an operator can't steer the check away from data they know is bad.
- **Keep-last-verified retention.** GFS retention, with restic-compatible semantics, never forgets the newest snapshots that passed a drill.

Verify offline:

```bash
backupproof proof verify drill.bundle.json --key bpkey1:server@backup:… --key bpkey1:agent:web-01:… --require-timestamp
backupproof proof verify-pack evidence.json --key …
```

## Quick start

### Standalone (one machine, no server)

```bash
export BP_PASSWORD='a long repository password'   # store it offline too
backupproof init   --repo /mnt/backup/vault
backupproof backup --repo /mnt/backup/vault --name website /var/www
backupproof backup --repo /mnt/backup/vault --spec examples/postgres.json     # BP_SOURCE_PASSWORD for the DB
backupproof drill  --repo /mnt/backup/vault --spec examples/postgres.json --timestamp
backupproof proof list   --repo /mnt/backup/vault
backupproof proof export --repo /mnt/backup/vault <digest> -o drill.bundle.json
```

S3 with Object Lock: `--repo 's3://bucket/vault?endpoint=https://s3.eu-central-003.backblazeb2.com&lock=COMPLIANCE&lockDays=30'`. Credentials come from `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`.

### Fleet (control plane + agents)

```bash
# control plane (behind a TLS reverse proxy)
backupproof server --data /var/lib/backupproof-server --listen 127.0.0.1:8420 --public-url https://backup.example.com
# or: docker compose up -d
```

1. Open the dashboard and create the admin account. There is no default password.
2. **Repositories → Add**: choose local, S3 or SFTP, and set a repository password you also store offline.
3. **Agents → Enroll**: copy the single-use command (it expires in 1 h) and run it on each server:
   ```bash
   backupproof agent enroll --server https://backup.example.com --token pve_…
   backupproof agent run          # or install deploy/backupproof-agent.service
   ```
4. **Sources → Add**: choose the kind, agent, repository, schedules, retention and drill checks. Optionally choose a **verifier** agent on another host (it needs Docker for database drills).
5. Watch each source move from **Unproven** to **Proven**.

Agents connect **outbound only** (HTTPS long-poll). Backup data goes straight from the agent to storage and never passes through the control plane.

## Repository format (v1)

| Object | Contents |
|---|---|
| `config` | Format, version, repository ID, chunker parameters (plaintext, no secrets). |
| `keys/<id>` | Password slots. Argon2id (t=3, 64 MiB, p=4) derives a KEK, which wraps random master keys with XChaCha20-Poly1305. Add a break-glass slot with `key-add`. |
| `data/<id[:2]>/<id>` | Chunks from FastCDC (512 KiB / 1 MiB / 8 MiB, gear table derived from a secret seed). Each chunk is zstd-compressed and sealed with XChaCha20-Poly1305 (random 192-bit nonce, AAD = object name). The ID is BLAKE3 keyed by a MAC key over the plaintext and is re-checked on every read. |
| `snapshots/<id>` | Encrypted snapshot record: source, host, time, Merkle root, stats, database metadata, and the IDs of the chunked manifest. |
| `proofs/<digest>.json` | Signed attestations (public; they contain hashes and counts, never file names or data). |

Deduplication works across files and snapshots. Unchanged files (same size and mtime) reuse their chunk lists. Writes are lock-free. Prune deletes only unreferenced blobs older than a grace period, and refuses to run if any snapshot is unreadable.

## Security model

- **Secrets at rest.** Repository passwords, storage and database credentials are encrypted in the control-plane database with a 256-bit key (`secret.key` or `BP_SECRET_KEY`). They are sent only to the agent holding the job lease.
- **Agents.** Each agent has its own Ed25519 attestation key and a bearer token, stored hashed on the server. The server checks every attestation's signature against the enrolled key and checks that it describes the leased job's source before ledgering it. A compromised agent cannot forge evidence for sources it isn't assigned.
- **Accounts.** Argon2id password hashes. Sessions are HttpOnly, `SameSite=Strict` cookies with CSRF tokens. Roles are admin, operator and auditor (read-only plus evidence). Anything that could give control of a server is admin-only: commands and hooks, custom restore-test commands, moving an item to another server, connecting servers, on-the-fly rclone remotes and previewing old backups. Every operator action is written to the same ledger as the proofs.
- **Built-in agent.** It never backs up, imports from, or stores into the server's own data folder (keys, database), even when asked to protect `/` or `C:\`.
- **Restores.** All writes go through an `os.Root` confined to the restore folder, and symlinks are created last, so a crafted snapshot can't write outside it. Restore tests only restore the snapshot named in the backup proof the server verified from that item's own server, and check its content root first.
- **Web.** Strict CSP (`default-src 'self'`), `X-Frame-Options: DENY`, no third-party assets.
- **Drill sandboxes.** `--network none`, memory, CPU and PID limits, `no-new-privileges`, data on tmpfs, restored files mounted read-only, and the image recorded by digest.
- **Ransomware.** Use S3 Object Lock in COMPLIANCE mode, and storage credentials that cannot delete. Prune skips locked objects. Run verifiers on a different machine.

## Building

```bash
make test        # go test ./...
make build       # bin/backupproof (static, CGO_ENABLED=0)
make dist        # linux/darwin/windows × amd64/arm64 + SHA256SUMS
docker build --target server -t backupproof-server .
docker build --target agent  -t backupproof-agent  .
```

Requires Go 1.27+ (see `go.mod`). The binary is pure Go: SQLite via `modernc.org/sqlite`, no cgo.

## What changed from v13

The original BackupProof (TypeScript, v13) proved the product idea: a backup only counts once recovery is shown to work. This version keeps that idea and rebuilds the parts that couldn't carry it.

| BackupProof v13 (TypeScript) | BackupProof (Go rewrite) |
|---|---|
| The database "proof check" passed whenever a target string was set; it never restored the DB | Databases are restored into version-matched sandbox containers, with engine integrity checks, row-count reconciliation and SQL assertions |
| Proof reports were plain JSON files anyone could edit | Signed in-toto/DSSE attestations, a hash-chained append-only ledger, RFC 3161 timestamps and offline-verifiable bundles |
| Whole files read into memory (5 GB guard), so dedup only covered identical files | Streaming FastCDC chunking with constant memory, dedup within and across files |
| Fixed scrypt salt, scrypt run on every chunk, chunk names were the plaintext SHA-256 (leaks content), manifests stored in plaintext | Argon2id with random salt, keyed BLAKE3 IDs, XChaCha20-Poly1305, encrypted manifests |
| Prune deleted manifests but never chunks, so storage grew forever | Reference-counted prune with grace period and object-lock awareness |
| Checksum proof compared restored data against the *live* source (false failures) | Restored bytes are compared with the snapshot's own Merkle root |
| Default `admin`/`admin` | First-run setup, Argon2id, CSRF, roles |
| Single host; agent was a heartbeat stub | Outbound-only agents with single-use enrollment, job leases, and separate verifier hosts |

## Roadmap

- Pack files (batch small chunks) and a local index cache for very large repositories
- PostgreSQL physical/PITR (pg_basebackup + WAL, `pg_verifybackup`) and MySQL physical (XtraBackup/mariabackup `--prepare`)
- MSSQL (`RESTORE` + `DBCC CHECKDB`) and Redis (`redis-check-rdb` + key digests)
- mTLS agent identities with short-lived certificates; transparency-log (C2SP tlog-tiles) checkpoints with witness co-signing
- Object Lock retention extension during maintenance; repository-to-repository copy for 3-2-1
- Docker volume and Kubernetes PVC sources; Windows VSS snapshots

## License

MIT

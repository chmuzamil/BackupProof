# Contributing to BackupProof

Thanks for helping make backups provable.

## Prerequisites

- Go 1.27 or later
- Optional, to run the import tests that use real tools: `restic`, `kopia`, `rclone`, `gpg`, `openssl` and `age` on your `PATH`. Tests that need a missing tool are skipped.
- Optional: Docker, to try database restore tests (PostgreSQL, MySQL/MariaDB, MongoDB) by hand.
- Optional: [golangci-lint](https://golangci-lint.run/) v2

## Quick start

```bash
git clone https://github.com/chmuzamil/BackupProof.git
cd BackupProof
go build -o bin/backupproof ./cmd/backupproof
go test ./...
```

Run the dashboard locally:

```bash
./bin/backupproof server --data ./backupproof-data --listen 127.0.0.1:8420
```

Open http://127.0.0.1:8420 and create the admin account. The built-in agent ("This server") connects automatically.

## Project layout

| Path | What lives there |
|---|---|
| `cmd/backupproof` | The single binary: `server`, `agent` and the standalone CLI |
| `internal/repo`, `internal/engine`, `internal/chunker`, `internal/crypto`, `internal/snapshot` | Repository format, backup/restore/prune, FastCDC, encryption, Merkle tree |
| `internal/source` | What gets backed up: files, databases, commands |
| `internal/drill` | Restore tests and their sandboxes |
| `internal/proof` | Attestations (in-toto/DSSE), ledger, RFC 3161 timestamps, bundles |
| `internal/importer` | Converting existing backups (files, restic, Kopia, Borg, rclone) |
| `internal/server` | Dashboard API, scheduler, watchdog, evidence; `web/` holds the dashboard UI (vanilla JS, no build step) |
| `internal/agent`, `internal/protocol` | Outbound-only agent and its wire protocol |
| `internal/e2e` | End-to-end tests of the full server + agent flow |

## Before you open a pull request

```bash
gofmt -l .              # must print nothing
go vet ./...
go test -race ./...     # -race needs cgo on Windows; use plain go test there
golangci-lint run ./...
node --check internal/server/web/app.js   # if you changed the dashboard
```

CI runs the same checks on Linux and Windows, plus `govulncheck`.

## Guidelines

- **Security first.** Anything that touches restore paths, agent authentication, roles or proofs needs a test that tries the attack and shows it fails. Report vulnerabilities privately to the maintainer, not in public issues.
- **Plain language in the dashboard.** Use the existing vocabulary ("Restore test", "Backup storage", "Servers") and keep technical details behind "Show technical details".
- **Keep the dashboard CSP-clean.** No inline scripts or styles, no third-party assets, and never put server data into `innerHTML`.
- **Don't change wire or storage formats** (repository objects, attestations, ledger hashing) without a version bump and a migration note in the changelog.

## Commit messages

Write the subject in the imperative ("Add Kopia import", "Fix restore through symlinks") and explain *why* in the body when it isn't obvious.

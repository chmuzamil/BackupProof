# Security audit scope

This is for a firm or person reviewing BackupProof's security. It says what
to look at, in what order, what you get, and what to send back. Read
[THREAT-MODEL.md](THREAT-MODEL.md) first: it states what each part is meant to
guarantee and the risks already known.

## What BackupProof is

BackupProof is open-source (MIT), self-hosted backup software written in Go:

- a dashboard (control plane);
- agents on each server;
- encrypted storage on S3, SFTP, a folder or rclone;
- regular restore tests, each recorded as a signed proof in a hash-chained
  ledger that is published as a transparency log.

Source: <https://github.com/chmuzamil/BackupProof>. Releases are signed and
reproducible.

## Priorities

In order. The line counts are non-test Go, as of 0.6.

### 1. Encryption and key handling

`internal/crypto` (240 lines), plus the sealing in `internal/repo/repo.go`.

- Argon2id parameters and their bounds.
- XChaCha20-Poly1305 use: nonces, and the associated data binding each object
  to its kind and name.
- Keyed BLAKE3 chunk naming, and what it reveals.
- Key wrapping in key files.
- Storage credentials sealed under `secret.key` on the dashboard.
- Token and password hashing.

**Question:** can anyone without the storage password read or undetectably
change backup data?

### 2. Storage format and its readers

`internal/repo` (1,200 lines), `internal/backend` (1,000), `internal/snapshot`
(360), `internal/engine` (2,550).

- Parsing of every object read from storage: config, key files, chunks, pack
  trailers and headers, index files, snapshots and file lists.
- Restore path handling.
- Prune (mark, then delete after a grace time) and the v1 to v2 upgrade.
- Range reads in each backend.

**Question:** can a hostile storage crash, exhaust or mislead a server that
opens it? Can it make a restore write outside its target, or make a check or
restore test pass on altered data?

### 3. Proof chain

`internal/proof` (780 lines), `internal/transparency` (820),
`internal/server/agentapi.go` and `internal/server/tlog*.go`, and the
verifiers in `cmd/backupproof` and `cmd/verify-wasm`.

- DSSE envelopes and in-toto statements.
- What the dashboard checks before accepting a statement.
- Ledger hashing and checkpoints.
- RFC 3161 timestamp verification.
- C2SP signed notes, cosignatures, tile serving, and inclusion and consistency
  proofs.
- `verify`, `verify-pack` and `verify-log`.

**Question:** can a proof, evidence pack, checkpoint or log verify when it
shouldn't? Can a dashboard rewrite or fork its history without a witness or a
saved checkpoint catching it?

### 4. Agent API and sessions

`internal/server/agentapi.go`, `agentsession.go`, `internal/agent` (890 lines),
`internal/protocol`.

- Enrollment tokens, long-lived tokens and 15-minute sessions.
- Lease contents and binding.
- Whether one agent can get another's jobs, secrets or signing identity.

### 5. Witness

`internal/transparency/witness.go` and `cmd/backupproof/transparency.go`.

- Request parsing.
- The size check, the consistency check and state updates.
- Key handling.

**Question:** can a witness be made to cosign a checkpoint that doesn't extend
the last one it cosigned?

### 6. Dashboard web app

`internal/server` (9,700 lines in all), `internal/server/web/app.js` (4,500).

- Sign-in, sessions, CSRF, two-factor sign-in and roles (viewer, auditor,
  operator, admin).
- API tokens.
- The first-admin setup code.
- Output encoding in the single-page app.
- Restore and app-test settings an operator can set: what they can make Docker
  do (`internal/source/apptest.go`, `internal/drill`).

### 7. Release and install

`.github/workflows/release.yml`, `scripts/install.sh`, `scripts/install.ps1`,
`scripts/verify-build.sh`.

- minisign verification in the installers.
- Reproducibility.
- The SBOM and provenance attestation.

## Out of scope

- Attacks that need root on the dashboard's or an agent's server. Exception:
  where a lower-privileged local user can reach BackupProof's files or
  sockets, which is in scope.
- An administrator denying service, or deleting what they're allowed to
  delete.
- Vulnerabilities inside the Docker images or apps a user picks for app tests.
  How BackupProof sandboxes them is in scope.
- The website backupproof.dev and the public demo, except
  backupproof.dev/verify/, which is in scope (item 3).
- Third-party services: S3 providers, timestamp authorities, GitHub.
- The risks [THREAT-MODEL.md](THREAT-MODEL.md) lists as residual, unless you
  find them worse than described there.

## What you get

- The source at a tagged release, and the matching signed binaries. Check them
  with [docs/SIGNING.md](SIGNING.md) and `sh scripts/verify-build.sh <tag>`.
- Test suites:
  - `go test ./...`;
  - fuzz targets (`go test -fuzz`; the CI fuzz job lists them in
    `.github/workflows/test.yml`);
  - compatibility fixtures from every release since 0.2.0 under
    `internal/*/testdata/compat`;
  - the browser verifier check, `scripts/verify-wasm-test.mjs`;
  - the WordPress app-test check, `BP_WORDPRESS_TEST=1`.
- A test setup:

  ```bash
  backupproof server --data ./d --listen 127.0.0.1:8420
  ```

  This runs a dashboard with its built-in agent. Agents on other machines
  connect with the one-line command under Servers → Connect a server.
  Restore tests and app tests need Docker. S3 can be tested against any
  S3-compatible server; CI uses SeaweedFS.
- A maintainer who answers questions within 3 working days.

## What to send back

- Each finding with:
  - affected versions and files;
  - a severity, and how you rated it;
  - steps to reproduce, or a proof of concept;
  - a suggested fix if you have one.
- Report findings privately: [Report a vulnerability](https://github.com/chmuzamil/BackupProof/security/advisories/new),
  or as agreed. Don't open public issues.
- A summary fit to publish once fixes are released, naming what was reviewed
  and at which commit.

[SECURITY.md](../SECURITY.md) sets out disclosure: a fix for confirmed issues
within about 14 days, and publication after the fix or 90 days. Release notes
credit reviewers unless they'd rather not be named.

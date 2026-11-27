# Threat model

What BackupProof protects, from whom, how, and what it doesn't protect
against. It covers five areas: encryption, the storage format, the proof
chain, the agent API and witnesses. [AUDIT-SCOPE.md](AUDIT-SCOPE.md) turns
this into questions for a security review. Report a problem privately as
[SECURITY.md](../SECURITY.md) describes.

Version described: 0.6. File paths are in this repository.

## The parts

| Part | Runs on | Holds |
|---|---|---|
| **Dashboard** (`backupproof server`) | One server, behind an HTTPS reverse proxy | Accounts, items, schedules, the proof ledger and transparency log, storage passwords and credentials (encrypted with `secret.key`), its signing key (`server.key`) |
| **Agent** (`backupproof agent run`) | Each server it backs up, and the dashboard's own built-in agent | Its Ed25519 signing key and long-lived token, in its state folder |
| **Storage** | S3-compatible, SFTP, a folder, or anything rclone reaches | Encrypted chunks, packs, indexes, snapshots, key files, signed statements |
| **Witness** (`backupproof witness`) | A machine apart from the dashboard | Its Ed25519 key and the last checkpoint it cosigned per log |
| **Verifiers** | Anyone's computer: `backupproof proof verify`, `verify-pack`, `verify-log`, and backupproof.dev/verify/ (the same code as WebAssembly) | The public keys they're given |

Data flows: agents connect out to the dashboard over HTTPS and never listen
on a port. Backup data goes from the agent straight to storage; the dashboard
never sees file contents. The dashboard hands an agent a job (a *lease*),
including the storage password and credentials it needs, and the agent sends
back a signed statement of what it did.

## Who we defend against

| Adversary | Can | Should not be able to |
|---|---|---|
| **The storage provider**, or anyone with read or write access to the storage | Read, change, delete, replay or roll back any object | Read backup contents or file names; change a backup without a check or restore noticing; make a restore test pass on data that isn't the backup; crash a server that opens the storage |
| **Someone on the network** between an agent and the dashboard or storage | Watch and change traffic, if TLS fails | Anything, while TLS holds: every connection is HTTPS (or SSH for SFTP) |
| **A stolen agent token** | Use it from anywhere | From an agent's first session (0.5 on), do anything without that agent's signing key |
| **A compromised agent** | Everything that agent can: its own leases, so its items' storage passwords and data | Get another agent's jobs or secrets; sign proofs as another agent; make the dashboard trust a snapshot it planted in shared storage |
| **A restored backup**, or the app started from it in an app test | Contain anything, including hostile files and code | Write outside the restore folder; reach the network or the server it's tested on; run with privileges |
| **A dashboard account** | What its role allows (viewer, auditor, operator, admin) | More than its role; a viewer or auditor changing anything |
| **The dashboard itself**, compromised or dishonest | Rewrite its database, sign new checkpoints | Rewrite or fork its proof history without every witness refusing and `verify-log` failing for anyone who kept an earlier checkpoint; forge an agent's signature |
| **A forged or altered release** | Be offered as a download | Pass the installer's signature check, or `verify-build.sh` |

Out of scope: an attacker with root on the dashboard's server *and* the
storage passwords, or root on an agent's server (they have what that server
has); denial of service by an administrator; vulnerabilities in a Docker image
or app you choose for an app test.

## 1. Encryption

**Design** (`internal/crypto/crypto.go`):

- Each storage has random master keys: an encryption key and a MAC key.
- Each password slot is a key file in `keys/`. The password goes through
  Argon2id (default 3 rounds, 64 MiB, 4 threads, a random salt) to make a
  key-encryption key. That key wraps the master keys with XChaCha20-Poly1305.
- The key derivation settings come from the key file, so they are bounded: 1 to
  16 rounds, 8 MiB to 1 GiB and 1 to 16 threads (0.4). A planted key file
  can't crash or exhaust a server that opens the storage.
- Every object is compressed with zstd, then sealed with XChaCha20-Poly1305
  under a random 192-bit nonce. Its associated data is the object's kind and
  name, so an object can't be passed off as another (`internal/repo/repo.go`).
- Chunks are named by BLAKE3 keyed with the MAC key, so the storage can't
  confirm a guess of what a chunk holds. Every read checks the name against the
  decrypted contents.
- Storage passwords and credentials kept by the dashboard are sealed with
  XChaCha20-Poly1305 under `secret.key`. Bearer tokens are stored only as
  hashes. Account passwords use Argon2id (`HashPassword`).

**What the storage still learns:** how many objects there are, their sizes,
when they're written, and how much each backup adds. Deduplication works only
within one storage, so two storages can't be compared.

**Residual risks**

- Anyone with the storage password can decrypt everything in that storage. The
  dashboard holds every storage password in order to hand it to agents, so a
  compromised dashboard plus access to the storage reads the backups.
- A weak storage password is open to offline guessing by whoever holds the key
  files. Argon2id slows guessing down; it doesn't make a weak password strong.
- Key files can be added (`key-add`) but not rotated: there is no re-encryption
  under new master keys.

## 2. Storage format

**Design** (`internal/repo`, `internal/engine`, `docs/COMPATIBILITY.md`):

- `config` (format version, ID, chunker settings), `keys/`, `snapshots/`
  (encrypted backup records), `proofs/` (public signed statements).
- v1: `data/`, one chunk per object.
- v2 (0.4 on): `packs/` of about 16 MB, each ending with its own sealed list
  of contents, plus `index/` files listing where chunks are. Index files are
  cached locally (`BP_CACHE_DIR`).
- Prune marks first and deletes later, with a grace time, because storage has
  no locks.
- `backupproof upgrade` writes the index first and the format version last. A
  re-run deletes leftover index files that are cut off or stale (0.5.1).
- Every file restored is checked against the backup's **content root**: an
  RFC 6962-style Merkle tree of BLAKE3 file hashes
  (`internal/snapshot/merkle.go`).

**Attacks and what stops them**

| Attack | Defence |
|---|---|
| Change a chunk, pack or index | AEAD fails, or the keyed name doesn't match: the read fails and checks report it |
| Swap two objects | Associated data binds each object to its kind and name |
| Delete chunks | `check` and the weekly health check report missing chunks; a restore test fails |
| Roll the storage back, or drop recent backups | Restore tests ask for snapshot IDs the dashboard took from signed backup statements, never "latest"; a missing one fails. Object Lock stops deletion for the lock period |
| Plant a snapshot in shared storage | Restore tests only test snapshots attested by the item's own agent, and check the restored content root against the attested one |
| Hostile objects meant to crash or exhaust the reader | Fuzz tests on blobs, key files, file lists, proofs and evidence packs; bounded key derivation settings |
| A restore writing outside its folder | A file list with a `..`, `.` or empty path segment is refused when read (`internal/snapshot`), and restore won't write through links; tests plant links outside the target |

**Residual risks**

- With no repository locks, safety rests on the grace time: a backup that runs
  longer than the mark delay (24 hours by default) could lose chunks that a
  concurrent prune deleted.
- Someone who can write to storage can still destroy it. Only Object Lock or a
  second copy elsewhere protects against that.

## 3. Proof chain

**Design** (`internal/proof`, `internal/server/agentapi.go`, `internal/transparency`):

1. **Statements.** After a backup, restore test or copy, the agent signs an
   in-toto v1 statement in a DSSE envelope with its Ed25519 key. A statement
   holds hashes, counts, times and check results, never file names or
   contents.
2. **Acceptance.** The dashboard accepts a statement only if all of these hold:
   - it's signed by the enrolled key of the agent the job was leased to;
   - the job is running on that agent;
   - the statement names that job's item and snapshot;
   - for a restore test or copy, the content root matches the one in that
     item's signed backup statement.
3. **Ledger.** Each accepted statement, and each operator change, is appended
   to a hash chain. Each entry's hash is SHA-256 over the entry with its
   sequence, time, kind, subject, envelope digest and the previous hash. The
   database refuses updates and deletes (triggers).
4. **Checkpoints.** The dashboard signs the ledger head with its key.
   Optionally, an RFC 3161 timestamp authority stamps a statement or
   checkpoint, so it can't be backdated.
5. **Transparency log** (0.5). Ledger entry hashes also form a C2SP
   tlog-tiles log at `/log/`, with signed-note checkpoints. Witnesses cosign
   them (section 5).
6. **Verification.** `proof verify` checks one bundle: the signature, the
   ledger entry, the chain to a signed checkpoint, and the timestamp.
   `verify-pack` checks an evidence pack. `verify-log` checks the log, its
   cosignatures, consistency with a saved checkpoint, and inclusion. The
   browser page runs the same Go code as WebAssembly, and CI requires it to
   agree with the command-line tool on every fixture.

**Residual risks**

- **Keys are the anchor.** A proof shows which key signed it. Verifiers must
  get public keys out of band. Without keys, a verifier only shows a bundle is
  internally consistent, and says so.
- **The dashboard can rewrite history it alone keeps.** It can rebuild a
  ledger and sign new checkpoints. This is detected only by witnesses, by
  auditors who kept an earlier checkpoint (`verify-log --since`), or by
  timestamps. With no witness configured, nothing outside the dashboard
  checks it.
- **Times come from the signing server** unless an RFC 3161 timestamp is
  present. The browser verifier doesn't pin timestamp authority roots.
- **A compromised agent can sign false statements** about its own items, such
  as a restore test that "passed". Restore tests can run on a separate
  verifier server (`verifier_id`) so the server backed up doesn't grade its
  own backup.
- **A restore test proves the backup restores and matches what was backed
  up**, not that the right things were backed up.

## 4. Agent API

**Design** (`internal/server/agentapi.go`, `agentsession.go`, `internal/agent`):

- **Enrollment.** A single-use code, valid for an hour, created by an admin.
  The agent sends its new Ed25519 public key and receives a long-lived token,
  which the dashboard stores hashed.
- **Sessions** (0.5; 15 minutes from 0.5.1).
  - The agent signs `backupproof-agent-session/v1\n<id>\n<unix time>\n` with
    its key. The dashboard accepts a time within 5 minutes and returns a
    random session token, stored hashed.
  - After an agent's first session, its long-lived token is accepted only to
    start a session.
  - Revoking an agent ends its sessions.
- **Routes:** `enroll`, `session`, `poll`, `jobs/{id}/log`,
  `jobs/{id}/attest`, `jobs/{id}/finish`. Each one after `session` checks that
  the job is leased to the calling agent.
- **Leases** carry the item's source settings, the storage password and
  credentials, the snapshot to test and its expected root, a sampling seed
  taken from the ledger head (so a health-check sample can't be chosen in
  advance), and limits.
- **Restore tests and app tests** run in containers:
  - no network and no published ports;
  - restored data read-only, unless a throwaway copy is asked for;
  - no extra privileges, and memory and CPU limits;
  - secrets come from the agent's environment and are scrubbed from results
    (`internal/drill`).

**Residual risks**

- A lease contains storage secrets in plaintext, protected by TLS. An agent
  that has been compromised receives them for every job it runs.
- An agent older than 0.5 has no sessions: its long-lived token alone
  authenticates it.
- Session requests are bound to a time window, not a one-use challenge. A
  captured signed request can be replayed within 5 minutes, but only together
  with the long-lived token, and it only gets a session for that same agent.

## 5. Witnesses

**Design** (`internal/transparency/witness.go`, `cmd/backupproof/transparency.go`):

- A witness trusts the logs given with `--log` (verifier keys), and keeps the
  last checkpoint it cosigned for each, in `witnessed.json`.
- `POST /add-checkpoint` carries the old size, a consistency proof and the new
  checkpoint. The witness checks three things: the log's signature, that the
  old size is the one it saw (409 with its size if not), and that the proof
  shows the new tree extends the old one (422 on a fork). Then it returns a
  timestamped Ed25519 cosignature (C2SP cosignature/v1).
- Its private key is `witness.key` (mode 0600) in its data folder.
- The dashboard asks every configured witness at most once a minute while the
  log has grown. It raises `witness-failed` after 15 minutes of failures and
  serves `/healthz/witnesses` for monitors (0.5.1).

**Residual risks**

- **Trust on first use.** A new witness accepts the first checkpoint it sees.
  History rewritten *before* a witness was added isn't detected by that
  witness.
- **One witness is one party.** If its key or host is compromised, it could
  cosign a fork. Run witnesses on machines, and with operators, independent of
  the dashboard, and use more than one.
- **Losing `witnessed.json` resets the witness to trust on first use.** Back up
  the data folder, and keep the key offline.
- **A witness that can't be reached stops cosigning**, which raises the alert.
  It can't forge a cosignature for a fork, but an attacker who blocks it
  delays detection until someone checks.

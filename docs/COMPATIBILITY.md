# Compatibility

Every proof made by BackupProof 0.2.0 or later keeps verifying with newer
versions, and every storage written by 0.2.0 or later keeps restoring. CI
checks both on every change against proofs and storage made by each release.
If a proof format has to change, the new one gets a new version and the old one
stays supported. If the storage format has to change, newer versions keep
reading the old one, and moving to the new one is a separate step you run.

## Storage

The storage format is versioned: v1 from 0.2.0, v2 (packs and an index) from
0.4.0; see the README. Every version reads both, and `backupproof upgrade`
moves v1 storage to v2 in place. Versions before 0.4 can't read v2, so the
dashboard creates v2 storage, and offers the upgrade, only when every server
using it runs 0.4 or later. Storage written by any release from 0.2.0 on must,
with the current version:

- open with its password;
- restore every backup in it byte for byte, with the restored files matching
  the backup's Merkle root;
- pass a full check that reads every chunk;
- take new backups, which restore too.

A version that can't read a storage's format says so and stops; it never
writes to storage it doesn't understand.

## What this covers

A proof is a signed in-toto statement with its ledger entries and checkpoint,
exported as a bundle and checked with `backupproof proof verify` and the
signer's public key. The fixtures cover, for every 0.2.x release where the
feature existed:

| Proof | Made by | From |
| --- | --- | --- |
| Backups and file restore tests | command-line tool | 0.2.0 |
| Backups and restore tests (files, SQLite, PostgreSQL) | dashboard and its agent | 0.2.0 |
| Second-copy proofs | dashboard and its agent | 0.2.0 |
| Second-copy restore tests | dashboard and its agent | 0.2.14 |
| Evidence packs | dashboard | 0.2.0 |
| RFC 3161 timestamps | dashboard | 0.2.0 |

Failed restore tests are proofs too: a signed record that the test failed must
verify just like a passing one.

## The rules

1. A proof's type is a versioned URI, such as
   `https://backupproof.dev/attestation/restore-drill/v1`, and bundles say
   `backupproof-bundle/v1`. A version is never changed in place.
2. Within a version, changes only add optional fields. Verifiers ignore fields
   they don't know.
3. A change that would stop an older proof verifying needs a new version. The
   verifier keeps the code for every older version.
4. The checks a valid proof passes (signature, ledger chain, checkpoint,
   timestamp, evidence-pack rows and summary) never get weaker to make an old
   proof pass.

## How it's tested

`internal/proof/testdata/compat/<tag>/` holds proofs made by that release's own
command-line tool, with `key.txt`, the public key that signed them.
`TestProofsFromEarlierReleasesStillVerify` checks each one with the current
code on every change.

`internal/server/testdata/compat/<tag>/` holds proofs made by that release's
dashboard and built-in agent. `tools/compatgen` drives the dashboard over its
HTTP API: it protects a folder with a second copy, an SQLite file and a
PostgreSQL database, backs each up, runs a restore test of each and of the
second copy, and saves the bundles, the evidence pack, the signing keys
(`keys.json`) and `manifest.json`, which lists what that release produced.
`TestDashboardProofsFromEarlierReleasesStillVerify` checks every bundle, its
timestamp and the evidence pack.

`internal/repo/testdata/compat/<tag>/` holds a storage written by that
release's own command-line tool: two backups of a small folder, the second
after one file was added, one edited and one left alone, so it has reused
chunks too. `TestStorageFromEarlierReleasesStillRestores` restores both,
checks every chunk and adds a backup with the current code, on a copy.

The fixtures are made in CI, not on a developer's machine. The `compat-fixtures`
workflow renames its runner to `compat-fixtures` and builds each tag with its
real version string. After each release, run the workflow for the new tag and
commit its three artifacts: `compat-fixtures` under
`internal/proof/testdata/compat/`, `compat-dashboard` under
`internal/server/testdata/compat/` and `compat-repos` under
`internal/repo/testdata/compat/`.

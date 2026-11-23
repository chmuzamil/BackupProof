# Proof compatibility

Every proof made by BackupProof 0.2.0 or later keeps verifying with newer
versions. CI checks this on every change against proofs made by each release.
If a proof format has to change, the new one gets a new version and the old one
stays supported.

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

The fixtures are made in CI, not on a developer's machine. The `compat-fixtures`
workflow renames its runner to `compat-fixtures` and builds each tag with its
real version string. After each release, run the workflow for the new tag and
commit its two artifacts: `compat-fixtures` under
`internal/proof/testdata/compat/` and `compat-dashboard` under
`internal/server/testdata/compat/`.

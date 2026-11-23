# Proof compatibility

Every proof made by BackupProof's command-line tool since 0.2.0 keeps verifying
with newer versions. CI checks this on every change against proofs made by each
release. If a proof format has to change, the new one gets a new version and the
old one stays supported.

## What this covers

A proof here is a bundle from `backupproof proof export`: the signed in-toto
statement, its ledger entries and checkpoint, checked with
`backupproof proof verify` and the signer's public key. Today the fixtures cover
the command-line tool's backup and file restore-test proofs. Proofs made by the
dashboard and its agents, database restore tests, second-copy proofs, evidence
packs and RFC 3161 timestamps aren't covered yet; this page will say so when
they are.

## The rules

1. A proof's type is a versioned URI, such as
   `https://backupproof.dev/attestation/restore-drill/v1`, and bundles say
   `backupproof-bundle/v1`. A version is never changed in place.
2. Within a version, changes only add optional fields. Verifiers ignore fields
   they don't know.
3. A change that would stop an older proof verifying needs a new version. The
   verifier keeps the code for every older version.
4. The checks a valid proof passes (signature, ledger chain, checkpoint) never
   get weaker to make an old proof pass.

## How it's tested

Dashboard proofs are tested the same way: `tools/compatgen` drives each
release's dashboard and built-in agent over its HTTP API (a folder with a second
copy, an SQLite file and a PostgreSQL database), and
`TestDashboardProofsFromEarlierReleasesStillVerify` checks every bundle, its
timestamp and the evidence pack from `internal/server/testdata/compat/<tag>/`.
Each fixture's `manifest.json` lists what that release produced.


`internal/proof/testdata/compat/<tag>/` holds proofs made by that release's own
command-line tool, with `key.txt`, the public key that signed them.
`TestProofsFromEarlierReleasesStillVerify` checks each one with the current code
on every change.

The fixtures are made in CI, not on a developer's machine. The `compat-fixtures`
workflow renames its runner to `compat-fixtures`, builds each tag with its real
version string and runs `scripts/compat-fixtures.sh`. After each release, run the
workflow for the new tag and commit its artifact under
`internal/proof/testdata/compat/`.

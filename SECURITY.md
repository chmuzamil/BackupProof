# Security policy

BackupProof holds the keys to people's backups, so security reports get
priority over everything else.

## Reporting a vulnerability

Report it privately on GitHub: **[Report a vulnerability](https://github.com/chmuzamil/BackupProof/security/advisories/new)**
(the Security tab of this repository). Please don't open a public issue.

Include what you found, the version (`backupproof version`), and how to
reproduce it. A proof of concept helps but isn't required.

What happens next:

- You get a reply within 3 working days.
- A fix for a confirmed issue is released as soon as it's ready, usually within
  14 days, sooner for anything that exposes backup data or keys.
- The release notes credit you, unless you'd rather not be named.
- Please give us until the fix is released, or 90 days, before publishing.

## What counts

Anything that breaks what BackupProof promises, for example:

- reading or changing backup data without the storage password;
- a proof, evidence pack, timestamp or transparency-log checkpoint that verifies when it shouldn't, or a witness that cosigns a checkpoint that doesn't extend the last one it saw;
- a restore test that passes when the backup can't be restored;
- a restore writing outside its target folder;
- a restored backup, or an app test, reaching the server it runs on;
- getting into the dashboard, or an account doing more than its role allows;
- an agent getting jobs or data meant for another;
- a release download that isn't what the signed checksums say.

Not in scope: attacks that need root on the server running BackupProof, or
the storage password and the server's keys together; denial of service by an
administrator; and problems in a Docker image or app you chose for an app test.

## Supported versions

Fixes go into the newest release. Upgrade to it; there are no backports.

## Checking what you run

- Releases are signed: see [docs/SIGNING.md](docs/SIGNING.md).
- Releases are reproducible: `sh scripts/verify-build.sh <tag>` rebuilds a
  release from its source and checks it matches, byte for byte.
- Each release has a software bill of materials (`sbom.spdx.json`) and GitHub
  build provenance: `gh attestation verify <file> -R chmuzamil/BackupProof`.

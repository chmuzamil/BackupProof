# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- README banner, badges, demo, install and configuration sections; `CONTRIBUTING.md` and this changelog.
- CI on GitHub Actions: tests on Linux (with the race detector) and Windows, `gofmt`, `go vet`, cross-platform builds, `golangci-lint` and `govulncheck`. Dependabot for Go modules and Actions.

### Security

- Restores write through an `os.Root` confined to the restore folder and create symlinks last, so a crafted snapshot can't write files outside it.
- Restore tests only restore the snapshot named in the backup proof the server verified from that item's own server, and check its content root first.
- Operator accounts can no longer gain control of a server. Commands and hooks, custom restore-test commands, moving items between servers, connecting servers, on-the-fly rclone remotes and the old-backup preview are admin-only.
- The built-in agent never backs up, imports from or stores into the server's own data folder.

### Changed

- Dashboard accessibility and design pass: keyboard and screen-reader support, focus handling in dialogs, inline form errors, an unsaved-changes guard, locale-aware dates and numbers, and wizard steps in the URL.

## [0.1.0] - 2026-10-07

First release.

### Added

- Encrypted, deduplicating repository format (FastCDC, keyed BLAKE3, XChaCha20-Poly1305, Argon2id) on local disk, S3-compatible storage with Object Lock, or SFTP.
- Sources: files, PostgreSQL, MySQL/MariaDB, MongoDB, SQLite and commands, with logins read from Docker containers and WordPress settings.
- Restore tests in isolated sandboxes with Merkle-root comparison, database integrity checks, row-count reconciliation, SQL assertions and checks of PostgreSQL dumps found inside backups.
- Signed in-toto/DSSE proofs, a hash-chained append-only ledger, RFC 3161 timestamps, offline-verifiable bundles and evidence packs mapped to SOC 2, ISO 27001, NIST CSF, DORA, NIS2 and HIPAA.
- Dashboard with plain-language wizards, a built-in agent, one-line installs for other servers and discovery of what to protect.
- Import of existing backups: GPG/OpenSSL/age-encrypted files, restic, Kopia, BorgBackup, and cloud drives via rclone.

[Unreleased]: https://github.com/chmuzamil/BackupProof/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/chmuzamil/BackupProof/releases/tag/v0.1.0

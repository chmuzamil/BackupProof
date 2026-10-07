# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.4] - 2026-10-07

### Fixed

- Removing an item left its alerts open, with an "Open item" link to a page that no longer exists. Removing an item now resolves its alerts and unlinks them, like its proofs, so a new item that gets the same ID doesn't inherit them. Alerts left behind by older versions are cleaned up when the server starts.
- Opening a removed item showed "source N not found". It now says the item was removed, with links to Proof history and Protected.

## [0.1.3] - 2026-10-07

### Added

- **Import recognises restic, Kopia and Borg backups.** Choosing "Backup files in a bucket or folder" for a location that holds a restic or Kopia repository (also inside a folder, such as `server1/`) now switches the import wizard to the right tool, points it at the repository folder and checks again with the same password. Borg repositories get a hint to use the BorgBackup import.

### Fixed

- Converting a restic, Kopia or Borg repository as "backup files" failed with "cannot decrypt (wrong password or key)" for every file. It now stops with a plain explanation of what the backups are and which import to choose.

## [0.1.2] - 2026-10-07

### Added

- **Installer: `--domain NAME`** sets up HTTPS on Linux. It checks that the name points at the server, then configures nginx with a Let's Encrypt certificate from certbot, or Caddy (installing Caddy if no web server is present), keeps the dashboard listening only on 127.0.0.1 and sets its public address. `--email` adds an address for certificate expiry notices. If HTTPS can't be set up, the install still completes and explains what to do.
- Install docs: step-by-step HTTPS setup, `--public-url` behind your own proxy, and uninstall commands.

### Fixed

- Re-running the Linux/macOS installer to upgrade kept the data but reset `--listen` to all interfaces and dropped `--public-url`, which could expose a dashboard that had been limited to 127.0.0.1. Re-runs now keep the existing settings unless new ones are given.

## [0.1.1] - 2026-10-07

### Security

- Windows: the built-in agent's protection of the server's own data folder could be bypassed with an 8.3 short path (such as `C:\PROGRA~3\...`) to a folder that doesn't exist yet, for example when choosing a storage location. Paths are now resolved through their deepest existing folder before comparing.

## [0.1.0] - 2026-10-07

First release.

### Added

- **Install:** one-line installers for the dashboard (`install.sh` for Linux/macOS, `install.ps1` for Windows) that verify the download, install a service and print the address and a one-time setup code. Re-running upgrades in place; `--uninstall` removes it. Docker images `ghcr.io/chmuzamil/backupproof` and `ghcr.io/chmuzamil/backupproof-agent`.
- **Backups:** an encrypted, deduplicating repository format (FastCDC, keyed BLAKE3, XChaCha20-Poly1305, Argon2id) on a local disk, S3-compatible storage with Object Lock, or SFTP. Sources: files, PostgreSQL, MySQL/MariaDB, MongoDB, SQLite and commands, with logins read from Docker containers and WordPress settings.
- **Restore tests** in isolated sandboxes: Merkle-root comparison of the restored files, database integrity checks, row-count reconciliation, SQL assertions, and loading PostgreSQL dumps found inside backups into a test database.
- **Proofs:** signed in-toto/DSSE attestations, a hash-chained append-only ledger, RFC 3161 timestamps, offline-verifiable bundles and evidence packs mapped to SOC 2, ISO 27001, NIST CSF, DORA, NIS2 and HIPAA.
- **Dashboard:** plain-language wizards, a built-in agent ("This server"), one-line installs for other servers, discovery of what to protect, dark mode, keyboard and screen-reader support.
- **Import** of existing backups: GPG/OpenSSL/age-encrypted files in buckets or folders, restic, Kopia, BorgBackup, and cloud drives via rclone.
- **Release pipeline:** every `v*` tag publishes binaries for Linux, macOS and Windows (amd64/arm64), `SHA256SUMS`, the installers and Docker images. CI runs tests (Linux with the race detector, and Windows), `gofmt`, `go vet`, `golangci-lint` and `govulncheck`.

### Security

- A one-time setup code protects creating the first admin account from other machines on the network.
- Restores write through an `os.Root` confined to the restore folder and create symlinks last, so a crafted snapshot can't write files outside it.
- Restore tests only restore the snapshot named in the backup proof the server verified from that item's own server, and check its content root first.
- Anything that could give control of a server is admin-only: commands and hooks, custom restore-test commands, moving items between servers, connecting servers, on-the-fly rclone remotes and the old-backup preview.
- The built-in agent never backs up, imports from or stores into the server's own data folder.

[Unreleased]: https://github.com/chmuzamil/BackupProof/compare/v0.1.4...HEAD
[0.1.4]: https://github.com/chmuzamil/BackupProof/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/chmuzamil/BackupProof/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/chmuzamil/BackupProof/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/chmuzamil/BackupProof/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/chmuzamil/BackupProof/releases/tag/v0.1.0

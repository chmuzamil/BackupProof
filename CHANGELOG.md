# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.2] - 2026-10-08

The first published 0.2 release: no binaries were published for 0.2.0 or 0.2.1, whose release builds stopped at a failing test. It includes everything listed under them.

### Fixed

- Docker volumes backed up or restored through the helper container (Docker Desktop, or an agent that isn't root) keep their files' real owners. Before, they took the owner of the BackupProof agent.

## [0.2.1] - 2026-10-08

### Fixed

- Storage used only for second copies now gets its weekly health check and size chart too (before, Check now said no item uses it).
- A scheduled health check that couldn't be queued is tried again at the next tick instead of a week later.

## [0.2.0] - 2026-10-08

### Added

- **Restore from the dashboard.** Pick a signed backup, browse its files, download a selection as a zip, or restore on a server: back where it came from (owners and permissions kept), into a new folder, or onto another server. Database items restore into a new database (or SQLite file) or replace the original after confirmation. Every file is checked against its content hash.
- **Docker apps and volumes.** Back up a Compose app (all its volumes, container settings and Compose files) or single volumes, optionally stopping the containers while copying, and put volumes back exactly. Volumes are read directly, or through a small helper container on Docker Desktop.
- **Second copy (3-2-1).** Every backup can also go to a second storage, re-encrypted with its keys, sending only what changed. Each copy has a signed proof whose content root must equal the original's.
- **Storage health checks** run weekly (or on demand), re-read a sample of the data, and chart each storage's size.
- **Alerts** to Microsoft Teams, Telegram, ntfy, Gotify, Pushover and PagerDuty (incidents resolve when fixed), "resolved" messages that name the item, and a **weekly summary email**.
- **Two-factor sign-in** with an authenticator app and one-time recovery codes; administrators can reset someone's two-factor.
- **API tokens** for scripts (role-limited, can't manage people or tokens) and an **Activity** page listing every recorded action, including sign-ins.
- **Speed limits and time windows** per server for scheduled jobs.
- **Light, dark or automatic theme**, a top bar with the account menu, and `backupproof admin` commands to recover a locked-out account.

### Changed

- Changing your password needs your current password.
- Backups record file owners on Unix (outside the content root) so in-place restores are exact.
- The Home status box no longer has decorative lines.
- **Only administrators add storage** and choose an item's second copy. An operator could otherwise send backups to storage whose password they know and read files they aren't allowed to see.

### Security

- Restoring to the original location opens each folder on the way one level at a time and refuses any that is a symbolic link on disk (except root's own, such as `/var/run`), so a link planted after the backup can't redirect the restore into another folder such as `/etc`.
- A saved email password or ntfy/Gotify token is dropped when its server address changes, so it can't be sent to a server someone else picked.
- Saving an item no longer changes or clears its second copy.

## [0.1.5] - 2026-10-07

### Changed

- **Redesigned dashboard** in the same ink-and-paper look as backupproof.dev, with the website's logo and colours: a navy sidebar, a Home page that answers in one sentence whether everything is restore-tested, and a **proof tape** on every item showing the last 14 days of restore tests, backups and failures.
- The logo links to Home, and a footer links to the website and GitHub.

### Added

- **Remove storage** (administrators): storage that no item uses can be removed from the dashboard. The backups in it are not deleted; keep its recovery kit to add it again. Each storage shows which items use it.
- **Remove** on every row of the Protected page, not only on the item's own page.

### Fixed

- PostgreSQL restore tests failed on databases with partitioned tables (for example Supabase's `realtime.messages`): the index check passed the partitioned table's parent index to `amcheck`, which rejects it. Only real indexes are checked now, which still covers every partition. A failed check no longer shows leftover command output in its message.

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

[Unreleased]: https://github.com/chmuzamil/BackupProof/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/chmuzamil/BackupProof/compare/v0.1.5...v0.2.0
[0.1.5]: https://github.com/chmuzamil/BackupProof/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/chmuzamil/BackupProof/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/chmuzamil/BackupProof/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/chmuzamil/BackupProof/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/chmuzamil/BackupProof/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/chmuzamil/BackupProof/releases/tag/v0.1.0

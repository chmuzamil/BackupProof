# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **App tests can follow redirects, including to HTTPS.** Turn on **Follow redirects** and each page asked for follows up to 5 redirects. The test has no network or certificates, so a redirect to `https://name/page` asks the app for `/page` again with `Host: name` and `X-Forwarded-Proto: https`, as a site behind an HTTPS proxy is asked; the redirects followed are recorded in the check. A server older than 0.6 can't follow them, so the dashboard won't save that setting for an item it tests.
- **WordPress template, version 3,** with options on the form: the sign-in page (for a site whose security plugin moved it; the admin-area check is then left out), whether to check the site's API (for a site that turns its REST API off), and following redirects to HTTPS (on by default). A new CI job builds real WordPress sites in Docker in four setups (plain, sign-in moved by WPS Hide Login, REST API off, HTTPS forced) and checks the template passes each with the right option and fails each without it, rather than passing a site it can't really check.
- **[docs/THREAT-MODEL.md](docs/THREAT-MODEL.md)** and **[docs/AUDIT-SCOPE.md](docs/AUDIT-SCOPE.md):** what encryption, the storage format, the proof chain, the agent API and witnesses defend against and the risks that remain, and a scope for an outside security review.
- **Check a proof in the browser** at backupproof.dev/verify/. The proof verifier, compiled to WebAssembly (`backupproof-verify.wasm`, a new release file listed in the signed `SHA256SUMS`), checks a bundle on the visitor's own computer; the page can't send it anywhere. CI runs it on every proof any release has made and requires the same report as `backupproof proof verify`, and rejects tampered copies. `sh scripts/verify-build.sh <tag> js/wasm` rebuilds it byte for byte.

## [0.5.1] - 2026-11-27

### Fixed

- **An interrupted storage upgrade is safe to run again.** The upgrade writes the new index first and the format version last, so a crash or timeout leaves the storage on v1 and fully usable. Running it again now first checks index files an interrupted run left behind and deletes any that are cut off or list chunks deleted since, for example by a prune in between. Before, a re-run kept them, and new backups could then skip uploading a chunk the storage no longer had. Running the upgrade on storage that's already v2 does nothing.
- **Nothing else uses a storage while the dashboard upgrades it.** Jobs that back up to it or copy to it wait in the queue until the upgrade ends, and the upgrade doesn't start while one is running or the dashboard is backing itself up there.

### Changed

- **Agent sessions last 15 minutes instead of an hour,** so a stolen session token is useful for at most 15 minutes. Agents renew a session 5 minutes before it ends, counting from their own clock (the dashboard now also says how long a session lasts), so a clock that differs from the dashboard's by a few minutes doesn't matter; a session that ends early anyway is renewed on the spot. 0.5.0 agents keep working: they renew sooner than they need to.

### Added

- **An alert when a witness stops cosigning.** A witness that has failed for 15 minutes raises the `witness-failed` alert, sent like any other, and it resolves itself on the next cosignature.
- **`GET /healthz/witnesses`** for an uptime monitor: 200 while every witness cosigns, 503 once one has failed for 15 minutes. It reveals nothing about the log, so it answers without a sign-in even when the log is private; limit it to your monitor in the reverse proxy if you like.

## [0.5.0] - 2026-11-27

### Added

- **The proof ledger is a transparency log** in the C2SP format (tlog-tiles, signed-note checkpoints): `/log/checkpoint` and `/log/tile/...` on the dashboard, for signed-in auditors and API tokens, or anyone if you choose. It publishes only hashes of ledger entries. Existing ledgers are added on first start.
- **Witnesses.** `backupproof witness` runs a witness on another machine. It cosigns the dashboard's checkpoints (C2SP tlog-witness, timestamped Ed25519 cosignatures), but only checkpoints that extend the last one it saw, checked with a consistency proof. Add witnesses under **Settings → Transparency log**; the dashboard asks each one to cosign every new checkpoint.
- **Short-lived agent sessions.** An agent proves it holds its own signing key (the one its proofs are signed with) to get a session that lasts an hour, and renews it before it ends. From an agent's first session, its long-lived token is only accepted to start a session, so a copy of that token is useless without the agent's key. Revoking an agent ends its sessions. Agents older than 0.5 keep using their long-lived token.
- **`backupproof proof verify-log`** checks the log's signature and every witness's cosignature, that the log still extends a checkpoint you saved before (`--since`), and that a proof bundle's ledger entry is in the log (`--bundle`), from tiles each checked against the signed root. A dashboard that rewrote or forked its history fails all three.

## [0.4.0] - 2026-11-27

### Security

- **A damaged or planted key file in storage can no longer crash or exhaust a server that opens it.** The key derivation settings come from the storage's key files; zero rounds made every agent opening that storage crash, and huge memory or round counts could exhaust it. They are now limited (1 to 16 rounds, 8 MiB to 1 GiB, 1 to 16 threads), which every storage BackupProof made fits. Found by the new fuzz tests. Exploiting it needed write access to the storage.

### Added

- **Storage format v2: packs and an index.** New chunks are packed about 16 MB to an object, and each backup writes a small index of where its chunks are. A backup now starts by reading the index instead of listing every chunk: at a million chunks (about 1 TB) that is 1 list request plus 10 downloads the first time, then 1 list request, instead of 1,000 list requests every time, with the same memory (about 80 MB). Each chunk inside a pack is sealed and checked exactly as before, and each pack ends with a sealed list of its contents.
- **Local index cache.** Index files are cached on the server that reads them (`BP_CACHE_DIR`, or the user's cache folder), so only new ones are downloaded.
- **Prune for packed storage** deletes packs no backup needs, rewrites packs that are mostly unneeded, clears packs left by backups that didn't finish, and folds the index files into one, with the same mark-first, delete-later delay as before. The new index is written before anything is deleted.
- **`backupproof upgrade`** and **Storage → Upgrade to v2** move existing storage to v2 without copying anything: its chunks stay where they are and are listed once into the index. The dashboard offers it only when every server using that storage runs 0.4 or later.
- **New storage is v2** when every server that uses it runs 0.4 or later, and v1 otherwise, so older agents keep working. `backupproof init --format 1` makes v1 storage.

### Changed

- Storage backends read byte ranges (S3 `Range`, SFTP and local `ReadAt`, `rclone cat --offset`), to read one chunk out of a pack.

## [0.3.0] - 2026-11-27

### Fixed

- **Database restore tests no longer fail on larger databases for lack of memory.** PostgreSQL 18, MySQL, MariaDB and MongoDB restore tests kept the database in memory inside a container limited to 2 GB, so one bigger than about 1.5 GB failed even when the backup was good. Databases up to about 1 GB loaded are still tested in memory (now with a 4 GB limit); larger ones go on a throwaway disk volume. PostgreSQL up to 17 already used disk, because the image's own volume sat on top of the memory folder; it now goes where the size says. Restore-test containers no longer use swap.
- **A restore test that runs out of memory or disk says so.** The check explains that the server running the test ran out of room, which says nothing about the backup, and the alert says nothing was found wrong instead of "the backup may not be usable".

### Added

- **The dashboard backs itself up.** Under **Settings → Dashboard backup**, choose a storage that isn't on the dashboard's server. Once a day the dashboard backs up its accounts, items, schedules, proof history and keys there, with a signed proof like any other backup, and keeps the newest 14. A failure raises an alert.
- **`backupproof server restore-dashboard`** rebuilds a lost dashboard from that backup into an empty data folder, using the storage's recovery kit, and checks the proof ledger it brought back. The rebuilt dashboard keeps its signing key, so its earlier proofs still match, and connected servers reconnect once its address points at the new server.
- **App tests can check more pages.** Up to 10 more addresses, each with its own expected status and text, such as a sign-in page, an API or an uploaded file you know exists. Each is its own check in the proof; a failing one fails only the app test.
- **WordPress template, version 2,** also checks that the admin area asks for a sign-in and that the site's API answers from the restored database.
- **n8n template** for n8n in Docker with its built-in database: passes when n8n has opened the restored database and its editor loads. Offered automatically when an n8n container is found.
- **What restore tests download.** Each item's page shows how much a restore test downloads from storage and about how much that is a month at its schedule. Where the provider charges for downloads (Amazon S3, Google Cloud Storage), it says so and how to pay less.
- **`SECURITY.md`:** how to report a vulnerability privately, what counts, and how fast it's handled.
- **Reproducible builds, checkable.** `sh scripts/verify-build.sh <tag>` rebuilds a release from its tag and checks it matches the published binary byte for byte. 0.2.2, 0.2.10, 0.2.20 and 0.2.23 were checked.
- **Software bill of materials and build provenance** with every release: `sbom.spdx.json`, covered by the signed `SHA256SUMS`, and GitHub build provenance for each file (`gh attestation verify FILE -R chmuzamil/BackupProof`).
- **Free-space check before every restore test.** The test checks the server has room for the restored backup plus 1 GB before it starts, instead of failing partway.
- **Database test memory setting** under the item's restore test settings, for a database that needs more (or less) than the automatic choice.
- **Storage reliability tests.** Prune stopped after each of its first deletes, two machines backing up into one storage at once, a backup resumed after a failed upload, and prune during a storage outage: in each, every backup still restores byte for byte and a full check finds nothing missing.
- **Fuzz tests** for the code that reads chunks, storage settings and keys, backup file paths, proofs and evidence packs, run on every change.
- **A scale test** (`BP_SCALE=1000000 go test -run TestIndexScale ./internal/repo/`) that measures what a backup does first on large storage: list every chunk. At a million chunks that is 1,000 list requests before each backup.
- **Storage compatibility tests.** A small storage written by each release's own command-line tool is kept in the repository, and CI checks the current version still opens it, restores both of its backups byte for byte, passes a full check and adds new backups to it.

## [0.2.23] - 2026-11-24

### Fixed

- **Stopping the agent is a clean exit.** `systemctl stop` or a restart made the agent exit with an error, so systemd logged it as failed on every upgrade and reboot.

## [0.2.22] - 2026-11-24

### Changed

- **The dashboard has the new BackupProof logo,** the receipt mark from backupproof.dev, on the sign-in page, in the sidebar and as the browser-tab icon. The name now reads plain "BackupProof".

## [0.2.21] - 2026-11-24

### Security

- **`proof verify-pack` now checks the readable parts of an evidence pack against the signed statements.** Before, each proof's pass or fail, its item and kind, and the per-item summary were listed beside the signed statements but not checked, so an edited pack could show a failed restore test as passed and still verify. Timestamps in a pack are now checked too.

### Fixed

- **A PostgreSQL restore test no longer fails when `pg_dump` is newer than the database server.** The test ran on the server's PostgreSQL version, whose `pg_restore` can't read a dump from a newer `pg_dump` ("unsupported version in file header"), so a good backup was reported as failed. Backups now record `pg_dump`'s version and the test uses the newer of the two; for earlier backups it reads the version from the dump itself.

### Added

- **Compatibility policy and tests.** Every proof made by BackupProof 0.2.0 or later keeps verifying with newer versions: backups and restore tests from the command-line tool and the dashboard, second-copy proofs, evidence packs and RFC 3161 timestamps. CI checks this on every change against proofs made by each release. See `docs/COMPATIBILITY.md`.

## [0.2.20] - 2026-11-23

### Security

- **Releases are signed.** `SHA256SUMS`, which lists every binary and both installers, is signed with a minisign key held offline and published in `scripts/minisign.pub`, on the website and with each release. `install.sh` checks the signature itself with minisign or OpenSSL and stops if it's missing or wrong; `--allow-unsigned` installs a release from before signing began. `install.ps1` checks it when minisign is installed. See `docs/SIGNING.md`. The release key's ID is `1FA0D06091C6CB65`.
- **Built with Go 1.27.4,** which fixes a vulnerability in Go's HTTP server (GO-2026-6859). Earlier releases were built with Go 1.27.3.

### Changed

- **The evidence summary from `backupproof proof verify-pack` says "statements"**, the word used everywhere else, instead of "attestations".

## [0.2.19] - 2026-11-10

### Fixed

- **A restore test of a second copy no longer fails because a copy job removed the backup it was about to test.** The two now take turns for each item, and the test picks the newest copy when it starts. Before, this could raise a false "That copy may not be usable" alert that stayed open until the next test, up to 30 days later.

## [0.2.18] - 2026-11-10

### Added

- **Download the recovery kit for existing storage.** Administrators can get it from the **Storage** page after entering their password and, if it's on, a two-factor code. Wrong attempts count towards the sign-in limit, API tokens can't use it, and every download or refusal is recorded in Activity. Downloads are also sent to your alert channels.

### Fixed

- **The recovery kit can now be downloaded when you choose your own encryption password.** It's only made once the password is long enough and both fields match.
- **"I saved the password" now unticks when the password changes,** including when you switch between a generated password and your own.
- **Recovery kit commands now work when the password contains an apostrophe.**
- **A storage check counts a damaged or missing file list once,** instead of reporting one problem as two.

## [0.2.17] - 2026-11-10

### Fixed

- **MariaDB without the `mysql` commands now backs up and restores.** Newer MariaDB only ships `mariadb` and `mariadb-dump`, so these backups failed. BackupProof now uses whichever is installed, on the server and in containers.
- **A missing program is now named as the problem,** instead of a bare "exit status 127".
- **App tests now work for apps that don't run as root.** The restored copy keeps its backed-up owners and folder permissions, so the app can read its own data.

## [0.2.16] - 2026-11-09

### Security

- **Operators can no longer get around the edit limits** by sending a change as a new item. They also can't choose the restore-test image or reduce how many backups are kept.
- **Restore tests no longer receive the item's database password or import credentials,** which they never used. A separate verifier server now really only has access to the storage.
- **Sign-ins are limited:** 10 failures from one address in 15 minutes, and 4 password checks at a time. The storage test is for administrators only.
- **Docker put-back refuses more places** that control startup, such as `/etc/logrotate.d`, `/etc/rc.local` and `~/.config/systemd`.

### Fixed

- **Stopping or upgrading a service during a job now cleans up.** Containers stopped for a backup start again, and restore-test containers and decrypted copies are removed. Anything left by a killed process is cleared when the agent next starts.
- **Cleaning old backups can no longer break a running backup in shared storage.** Unused data is now marked and deleted on a later run at least a day afterwards, so freed space appears later than before. Backups never reuse marked data.
- **Docker volume put-back** now restores first and replaces the volume only if that succeeds, so a failed download leaves it as it was. It briefly needs room for both copies.
- **An item no longer shows as restore-tested when its last restore test couldn't finish.** It shows as at risk, with the reason.
- **A real failure is no longer hidden behind an open "nothing was found damaged" alert.** The alert is reworded and you're notified.
- **A blown data-loss limit no longer closes itself** after a long run of failed tests, and it now applies to items that have never passed a restore test.
- **Weekday-only schedules no longer raise a false overdue alert** at weekends.
- **Paused and manual items close their overdue alerts.**
- **A busy database no longer fails a proof or cancels a running job,** and a job with a number like 409 is no longer cancelled by a passing network error.
- **An app test no longer counts towards restore time,** can wait as long as the item's own response limit, and stops if the server is running out of disk (`app-disk-space`).
- **Recovery plan:** usable steps for items not yet tested, and the SFTP settings the command line needs. The recovery kit has them too.
- **A restore that failed no longer claims every file was verified** in its proof, and an expired restore no longer raises an unrelated alert.

## [0.2.15] - 2026-11-09

### Added

- **App restore test.** Optionally start the restored app after a restore test passes and check that it responds: container running, HTTP status, expected text and response time, recorded as `app-*` checks in the signed proof. A failed app test raises its own alert and the backup still counts as restore-tested. You name the image; nothing from the backup is run, including restored Compose files. Restored data is read-only (writable paths get a throwaway copy), there's no network or published port, secrets are referenced from the server running the test and never stored, and containers are always removed. Set it under **Edit (advanced)**, administrators only. Older agents ignore the setting.
- **App test templates** for WordPress in Docker and any web app. They fill in the form for you to review, can't give an app more access or hold secrets, and are recorded by name and version in the proof. Docker apps found on a server suggest a template.
- **Recovery plan** for each server, for administrators to print or download ahead of time: what to restore in what order, the newest tested backup, the storage and recovery kit needed, dashboard and command-line steps, what a person must confirm, known gaps, and what to do if the dashboard is lost. It holds no secrets.

### Fixed

- **Edit (advanced) on a Docker item** showed it as folders and files and wouldn't save it without a folder.
- **Docker apps no longer vanish from a server's list** when a container is removed at the moment the list is read, for example as a restore test finishes.

## [0.2.14] - 2026-11-09

### Security

- **Docker put-back checks where files go.** The paths in a backup are now checked when it's restored, as well as when it's made. A tampered backup can no longer put files in system folders, or in places that control logins, scheduled tasks or services, such as `/etc/cron.d` or `.ssh`. Those files can still be restored to a folder.
- **Restore-test containers run with only the capabilities the database images need,** instead of Docker's default set.

### Fixed

- **Database restores from the dashboard for WordPress and container items.** Items whose database name is worked out at backup time failed to restore, because the restore looked for a dump with no name. The name now comes from the backup, and the login is read on the server as during a backup. The confirmation no longer asks you to type "undefined".

### Added

- **Restore tests of second copies.** Each second copy is restore-tested every 30 days, or on demand with **Test this copy**, and gets its own signed proof and alert. Agents don't need upgrading. The first test runs soon after upgrading, and each test downloads the whole backup from the second storage.

## [0.2.13] - 2026-11-09

### Added

- **Recovery goals.** Optional per-item limits for data loss (RPO) and restore time (RTO), compared with the newest backup that passed a restore test and the last passed restore test's duration. The item's page shows both with a chart of its last 30 restore tests; passing a limit raises an alert. Also at `GET /api/sources/{id}/recovery`.
- **Prometheus metrics** at `GET /metrics`, for any token or account that can read the dashboard.

### Fixed

- The storage size chart drew its bars as thin dashes; they now show the real sizes.

## [0.2.12] - 2026-11-08

### Fixed

- **Restore tests wait for storage that refuses a chunk.** After a chunk's 6 attempts, a restore (and a copy to a second storage) reads it again after 30, 60 and 90 seconds while the other downloads carry on, instead of stopping at once. Found on 8 November, when Backblaze B2 returned HTTP 500 and 503 for one chunk for about 25 seconds and the weekly restore test of rasikh-recovery failed.
- **A restore test stopped only by storage errors says so.** Its alert reads "couldn't read part of the backup from storage … Nothing was found damaged; it runs again within the hour", and the restore test runs again within the hour. Any other failure still warns that the backup may not be usable.

## [0.2.11] - 2026-11-08

### Security

- Updated `github.com/klauspost/compress` to 1.20.2 for GO-2026-6865 (a crafted two-block zstd frame could crash the process).

## [0.2.10] - 2026-11-08

### Added

- **Optional error reporting.** Set `BP_SENTRY_DSN` to send crash reports from the server, agents and dashboard to your own Sentry or GlitchTip project. It is off by default and BackupProof ships with no DSN. Reports are scrubbed before they leave the process: no request data, users, host names, addresses, file paths or anything that looks like a password, key or token, and no tracing or replay. Dashboard errors go through the server, so the page's CSP is unchanged.

## [0.2.9] - 2026-10-31

### Changed

- **Backups process data about 5× faster.** Finding chunk boundaries read the data one byte at a time; it now scans it in large blocks, about 10× faster on its own (2.9 GB/s against 280 MB/s), and each chunk is fingerprinted once instead of twice. An unchanged 512 MB stream now takes 0.4 s instead of 2 s. Chunk boundaries are exactly the same as before, so existing repositories keep deduplicating: a test compares every boundary with the old chunker, and a backup by this version of data stored by 0.2.8 uploads nothing new.
- The timing line is left out of backups that finish within a second.

## [0.2.8] - 2026-10-30

### Fixed

- **Storage requests retry patiently.** The six attempts are now spaced between half and all of 0.5 s, 1 s, 2 s, 4 s and 8 s (and longer when the service sends Retry-After), instead of possibly all within a second, so a busy storage node has time to recover. Each retry is written to the job log.
- **A blob storage couldn't hand over is no longer reported as damaged.** Only a failed authentication or content-hash check counts as damage. A storage check reads unreadable blobs (and snapshot file lists) again after a pause; if they still can't be read, the check says so, reports nothing as damaged, and runs again within the hour. Found on 29 October, when a single HTTP 503 from Backblaze B2 failed a weekly check as "1 corrupt blob".
- **Alerts close only when the same kind of job succeeds.** A failed storage check, copy or restore test now has its own alert, which a later successful backup no longer closes.

## [0.2.7] - 2026-10-30

### Fixed

- A dashboard address that isn't valid UTF-8 (such as `/%c0`, sent by scanners) now gets "400 Bad Request" instead of a server error (500).

## [0.2.6] - 2026-10-23

### Security

- Built with Go 1.27.3, which fixes a certificate-verification vulnerability in the Go standard library (GO-2026-6809). BackupProof uses it when checking timestamp-authority certificates.

### Added

- **Where the time went**, at the end of every backup and restore (including restore tests) that takes more than a second. For a backup: the total time, time spent reading the source (for a command, waiting for its output), time spent waiting for uploads, and how much was uploaded, how fast, with how many requests at once, then which part was the slowest. For a restore: time spent waiting for storage, and how much was downloaded, how fast, with how many requests at once.

### Measured

- Two weeks of scheduled jobs on rasikh-recovery (one 4.1 GB file on Backblaze B2) since 0.2.4:
  - **restore test:** 5 min 17 s and 7 min 27 s (was 30 min 15 s), about 4–6× faster;
  - **nightly backup:** 5 min 37 s – 8 min 5 s (was 11 min 18 s), about twice as fast. The new timing lines will show how much of that is the backup command producing its output.

## [0.2.5] - 2026-10-09

### Changed

- Item cards no longer repeat the last restore test ("Restore tested 26 hours ago" above "Last restore test passed 26 hours ago"). When the newest backup hasn't been restore-tested yet, the card says so and when its test runs: "Newest backup not restore-tested yet. Next test: Sunday 06:00."
- The explanation of the status labels on the Protected page is folded away under **What the labels mean**, with small coloured dots instead of full labels.

### Security

- Built with Go 1.27.2, which fixes HTTP/2 and net/http vulnerabilities in the Go standard library (GO-2026-6612, GO-2026-6613, GO-2026-6617).

### Measured

- rasikh-recovery backup with 0.2.4: 5 min 52 s (was 11 min 18 s). A 452 MB restore test from Backblaze B2 on the dashboard server: 6.4 s.

## [0.2.4] - 2026-10-08

### Changed

- **Much faster restores, restore tests, second copies, zip downloads, health checks and backups.** Chunks used to move one at a time, so every 1 MB piece waited a full round trip to the storage service. Now one shared transfer layer runs requests in parallel: 8 at first, adapting between 4 and 32 (more while throughput rises, half when the service throttles or latency doubles), with downloaded data waiting in memory capped at 64 MiB and released in order. Decrypting and checking run on separate workers alongside the downloads, chunks repeated close together are downloaded once, and compression for uploads now uses every core.
  - Benchmark (64 MiB in 4 MiB chunks, 80 ms round trip): 1.39 s one at a time, 0.30 s adaptive (4.6×).
  - rasikh-recovery (one 4.1 GB file on Backblaze B2), before: restore test 30 min 15 s, backup 11 min 18 s.
- Failed storage requests are retried up to 6 times with jittered exponential backoff, honouring `Retry-After`; the S3 connection pool is shared per endpoint and sized for the highest concurrency.
- Per-server **Advanced** settings for transfers at once and memory, and `--concurrency` / `--max-inflight` on `backup`, `restore`, `drill` and `check`. `--concurrency 1` reads exactly as before.
- Speed limits still apply to all of a server's transfers together.

### Added

- **Remove disconnected servers** from the Servers page (administrators). Items that run or are tested on the server must be moved or removed first. Its key is kept, so the proofs it signed stay verifiable.

### Compatibility

- No change to the repository format (v1): older agents read repositories written by this version and the other way round. Upgrade the dashboard first (its built-in agent runs the restore tests), then the agents on your other servers. Older agents ignore the new transfer settings.

## [0.2.3] - 2026-10-08

### Added

- **Docker containers started on their own** (`docker run`, without Compose) are listed and can be protected like a Compose app.
- **Mounted folders**: a Docker app's backup also includes the folders and files on the server mounted into its containers (bind mounts such as `./data`), and **Put the app's data back** returns them to where they came from. System paths such as `/proc`, `/tmp` and the Docker socket are left out.

### Changed

- Clearer messages when a server has no Docker apps or volumes.

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

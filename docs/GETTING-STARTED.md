# Getting started with BackupProof

This guide is for people who want their data safe and don't want to become backup experts.

## The idea in one minute

- **BackupProof copies your important things** (folders, websites and databases) to a safe place, on a schedule.
- **It then tests that the copies actually work.** It restores them in a safe place and checks that everything is there. Only then does it show **Restore tested ✓**.
- **It keeps proof.** Every backup and every restore test is recorded in a way that can't be secretly changed. You can download that record for an auditor, a customer or yourself.

There are three things to know:

| On the dashboard | What it means |
|---|---|
| **Protected** | The things you're backing up, like "Documents" or "Shop website". |
| **Storage** | Where the backup copies are kept: a disk, Backblaze B2, Amazon S3, and so on. |
| **Servers** | The machines whose data you protect. The machine running the dashboard is already connected as **This server**. |

## Step 1: Start the dashboard

**On Windows**, open PowerShell in the BackupProof folder:

```bash
.\backupproof.exe server
```

**On Linux**, Docker is the easiest way:

```bash
docker compose up -d
```

Open **http://localhost:8420** in your browser and create your admin account.

The computer running the dashboard is protected straight away. You don't need to install anything else on it.

## Step 2: Protect something

Click **Protect something** and answer four questions:

1. **Which computer?** Usually "This server".
2. **What?** Folders, a website, or a database. BackupProof shows what it found on the computer, so you can just tick boxes:
   - For a **WordPress** site, it finds the database and its login on its own.
   - For a **database running in Docker**, it reads the login from the container, so you don't type any passwords.
3. **Where?** Pick a storage, or add one:
   - **A disk:** an external USB drive, or a second disk. Choose it from the list.
   - **Backblaze B2:** paste the *Key ID*, *Application Key*, *bucket name* and *endpoint* from your B2 account.
   - **Amazon S3, Cloudflare R2, Wasabi:** similar. Paste your keys and bucket name.

   Click **Test connection** to make sure it works.

   BackupProof creates an **encryption password** for this storage. **Download the recovery kit and keep it safe** (a password manager, or printed in a drawer). Without it, nobody, including you, can open the backups.
4. **How often?** "Every night" and "test a restore every week" are good choices for most people.

Click **Protect now**. BackupProof makes the first backup, then immediately tests that it restores. After a minute or so you should see **Restore tested ✓**.

## Step 3: Add your other servers (optional)

Go to **Servers → Connect a server**, choose Linux, Windows or Mac, and copy the one line shown. On the other computer:

- **Linux / Mac:** open a terminal, paste the line, press Enter.
- **Windows:** open PowerShell **as Administrator**, paste the line, press Enter.

The computer appears in the dashboard within a minute. The line works once and expires after an hour.

If the dashboard warns that other computers can't reach it, enter the address they should use, for example `https://backup.mycompany.com`. Your server must be reachable at that address.

## Bring in your old backups

If you already have backups on Backblaze B2, Amazon S3, a disk or another server, go to **Import**. BackupProof fetches them, decrypts them, stores them in its own format, and then restore-tests them. Your old backups are only read, never changed or deleted.

| Your old backups are… | Choose | What you need |
|---|---|---|
| Files made by a script or tool (database dumps, `.zip` / `.tar.gz` archives), possibly encrypted with **GPG** (`.gpg`), **OpenSSL** (`.enc`) or **age** (`.age`) | **Backup files in a bucket or folder** | The storage keys, plus the password or key file used to encrypt them. The encryption type is detected automatically. |
| A **restic** repository | **restic** | Easiest: choose the server where restic already runs and point to its settings and password files (for example `/etc/restic/env` and `/etc/restic/password`), so nothing secret is typed into the dashboard. |
| A **Kopia** repository | **Kopia** | Easiest: choose the server where Kopia runs and point to its settings file. Or connect to the bucket, folder or SFTP server, with the repository password. |
| A **BorgBackup** repository (another server over SSH, BorgBase, Hetzner Storage Box…) | **BorgBackup** | The repository address and passphrase (or a passphrase file), on a server with borg installed. |
| Backup files on **Google Drive, Dropbox, OneDrive** and 70+ other services | **Google Drive, Dropbox, OneDrive…** | Set up the drive once with `rclone config` on the converting server, then enter its name and folder (e.g. `gdrive:Backups`). Same decryption and unpacking options as backup files. |

Good to know:

- **Dates are kept.** Files with dates in their names (`shop-2026-09-01.sql.gz.gpg`, folders like `2026-09-01/`) become one backup copy per day, each with its original date.
- **Archives can be opened,** so the restore test checks every file inside, not just the archive.
- **Database dumps inside are tested.** PostgreSQL dumps found in the backups are loaded into a temporary test database (this needs Docker on the computer doing the restore tests).
- **You can keep your current backup job.** Set the import to run every night after it; each new backup is converted and restore-tested automatically.

## What the colours mean

| Status | Meaning | What to do |
|---|---|---|
| 🟢 **Restore tested ✓** | The latest restore test passed recently. | Nothing. |
| ⚪ **Not tested yet** | Backed up, but no restore test has passed yet. | Click **Test restore**. |
| 🟠 **Needs attention** | The last successful test is too old, or a backup is late. | Check the computer is on and click **Back up now**. |
| 🔴 **Problem** | The last backup or restore test failed. | Open the item to read what went wrong. |

You can get these alerts by email, Slack or Discord under **Settings → Notifications**.

## Golden rules

1. **Save the recovery kit** for every storage. No password means no restore.
2. **Keep at least one copy away from the computer itself:** the cloud or another building.
3. **Trust the green tick, not a "backup finished" message.** BackupProof only shows it after a real restore test.

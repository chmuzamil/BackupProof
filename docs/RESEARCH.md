# Research notes (October 2026)

This document records the market and technical research behind BackupProof's design, so later decisions can be checked against it. Facts were taken from vendor and project documentation at the URLs listed. Items marked *(unverified)* came from secondary sources.

## 1. The gap

| Tier | What it proves | Evidence format |
|---|---|---|
| OSS engines (restic, Kopia, Borg, Duplicacy, Bareos) | Bit-level integrity: stored bytes match their hashes (`check --read-data`, `verify-data`, `Verify Level=Data`) | Console text or JSON |
| Commercial VM suites (Veeam SureBackup, Datto, Acronis, Unitrends, Rubrik, Zerto) | "It boots": heartbeat, ping, screenshot, port probe, custom script in an isolated lab | Vendor console, HTML/PDF reports, screenshots |
| AWS Backup restore testing | Scheduled restore plus validation through EventBridge → Lambda → `PutRestoreValidationResult` | Restore job records |
| SMB/SaaS DB backup (SimpleBackups, SnapShooter, Ottomatik, backupninja) | "The job succeeded" | Dashboards |

**Nobody emits signed, timestamped, independently verifiable recovery evidence.** Other things are also missing:

- reconciling source data against the restored copy (DORA Art. 12 asks for "checks and reconciliations");
- proof of completeness (that no failed drill was quietly removed);
- unbiased sampling;
- separation of duties between the host that writes backups and the host that verifies them;
- RTO recorded as signed evidence.

Direct, small competitors in this niche:

- **FireDrill** (Apache-2.0, July 2026): pg/mysql drills with ed25519-signed JSON. It has no engine or fleet.
- **pgctl verify**: Postgres only.
- **Constat**: archived. Its "eleven ways a backup that works fails to restore" taxonomy is a good checklist: https://forum.restic.net/t/eleven-ways-a-backup-that-works-fails-to-restore-a-taxonomy-plus-a-tool-built-against-it/10953

## 2. User pain points that shaped requirements

1. **"Green job ≠ restorable backup."** Users call it Schrödinger's backup. In the GitLab 2017 outage, pg_dump 9.2 ran against PostgreSQL 9.6 and failed, the failure emails were dropped by DMARC, and nobody owned restore testing. https://about.gitlab.com/blog/2017/02/10/postmortem-of-database-outage-of-january-31/
   - What BackupProof does: dumps run inside the database's own container (`container`), drills pick sandbox images from the recorded server version, a dump that produces no data fails the job, and a watchdog catches what did not happen.
2. **Silent non-execution.** What BackupProof does: the dead-man's-switch watchdog, plus a heartbeat URL so the backup server itself is monitored.
3. **No fleet view for restic, Borg or Kopia** (Backrest is per-host). What BackupProof does: a control plane with outbound-only agents.
4. **Fear of repository corruption** (Kopia blob loss, Duplicati database rebuilds). What BackupProof does: self-verifying blobs, a prune that refuses to run when any snapshot is unreadable, and seeded read-data sampling.
5. **Database backups are a separate silo.** What BackupProof does: files and databases are sources in the same system with the same proofs.
6. **Commercial cost.** Veeam raised prices in Jan 2025 and again in Jan 2026: https://www.eon.io/blog/veeam-pricing
7. **Storage vendor churn.** The MinIO community edition was archived in April 2026. What BackupProof does: uses the AWS SDK against any S3-compatible service, plus SFTP and local storage.

## 3. Cryptographic choices

| Concern | Choice | Reason / prior art |
|---|---|---|
| Chunking | FastCDC, normalized level 2, keyed gear table | FastCDC paper; Borg uses a secret chunker seed against fingerprinting |
| Blob ID | BLAKE3 keyed with a MAC key | Borg's keyed IDs; avoids the confirmation-of-file attack that plaintext SHA-256 allows |
| AEAD | XChaCha20-Poly1305 with random 24-byte nonce | Random nonces are safe at this size; restic still uses CTR+Poly1305-AES rather than an AEAD |
| KDF | Argon2id t=3, 64 MiB, p=4, random salt per key slot | Borg 2 default; restic and Kopia use scrypt |
| Content commitment | Unkeyed BLAKE3 Merkle tree with RFC 6962/9162 structure and inclusion proofs | Third parties can recompute it from restored data without the repo key |
| Attestation | in-toto Statement v1 in a DSSE envelope, Ed25519 | https://github.com/in-toto/attestation/blob/main/spec/README.md · https://github.com/secure-systems-lab/dsse/blob/master/protocol.md |
| Trusted time | RFC 3161 (FreeTSA, DigiCert), `github.com/digitorus/timestamp` | Rekor v2 dropped SETs, so TSA tokens are the standard source of trusted time: https://blog.sigstore.dev/rekor-v2-ga/ |
| Completeness | Hash-chained ledger, append-only triggers, signed checkpoints | Next step: C2SP tlog-tiles (Trillian-Tessera) with witness co-signing |

## 4. What a strong restore test looks like, by engine

| Engine | Strong test | Implemented |
|---|---|---|
| PostgreSQL | Restore into an isolated instance; `pg_amcheck`/`bt_index_check(heapallindexed)`; exact row counts against counts from the source at the same snapshot; app queries. `pg_verifybackup` docs say it does not replace a test restore. | Logical: yes. Physical/PITR: roadmap |
| MySQL/MariaDB | Import or `--prepare`; `CHECK TABLE` (mysqlcheck); counts or `CHECKSUM TABLE` against the source | Logical: yes |
| MongoDB | `mongorestore --oplogReplay`; `validate({full:true})`; counts | Yes |
| SQLite | `VACUUM INTO` for a consistent copy; `PRAGMA integrity_check`; exact counts | Yes, with exact reconciliation |
| MSSQL | `RESTORE` plus `DBCC CHECKDB` (`RESTORE VERIFYONLY` does not check data structure) | Roadmap |
| Redis | `redis-check-rdb`; load into a server; `DBSIZE` and digests | Roadmap |

## 5. Compliance language (what auditors test)

- **SOC 2 A1.3:** "tests recovery plan procedures"; point of focus: backup integrity and completeness tested periodically.
- **ISO/IEC 27001:2022 A.8.13:** backups "maintained and regularly tested".
- **DORA Art. 12:** periodic restore testing, segregated backup systems, integrity checks and reconciliations. https://www.digital-operational-resilience-act.com/Article_12.html
- **NIS2 Art. 21(2)(c)** and Implementing Regulation 2024/2690 §4.2: backup plan, integrity checks, documented recovery tests.
- **HIPAA 164.308(a)(7)(ii):** "retrievable exact copies"; testing is addressable *(the 2025 NPRM tightening this was not final as of mid-2026)*.

None of these frameworks require cryptographic proof. Signed, timestamped evidence reduces audit sampling and makes evidence portable, which makes it a product differentiator rather than a checkbox.

## 6. Architecture references

- **Outbound-only agents with long-poll:** Elastic Fleet (`checkin_long_poll`), Netdata ACLK, Bareos client-initiated connections.
- **Single-use enrollment tokens:** Teleport join tokens, Tailscale pre-auth keys.
- **Retention semantics:** restic `forget`, https://restic.readthedocs.io/en/stable/060_forget.html. BackupProof matches restic's "keep oldest if buckets unfilled" behavior and adds keep-last-verified.
- **Lock-free prune:** Kopia's and Duplicacy's time-based safety windows. BackupProof uses a grace period on blob modification time.

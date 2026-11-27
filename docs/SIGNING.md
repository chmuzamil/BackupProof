# Release signing

Every release from 0.2.20 on has a signed checksum list. `SHA256SUMS` lists the
SHA-256 of every binary and of both installers, and `SHA256SUMS.minisig` is its
[minisign](https://jedisct1.github.io/minisign/) signature. Checking the
signature, then the checksums, proves a file came from a BackupProof release.

Releases up to 0.2.19 are not signed. They have checksums only.

## The release key

The public key is in [`scripts/minisign.pub`](../scripts/minisign.pub), on
[backupproof.dev/security](https://backupproof.dev/security/#signing) and
attached to every release as `minisign.pub`. Check that at least two of these
agree before you trust it.

## Verify a download

Linux or macOS:

```bash
minisign -Vm SHA256SUMS -P <key from scripts/minisign.pub>
sha256sum --ignore-missing -c SHA256SUMS      # on macOS: shasum -a 256 --ignore-missing -c SHA256SUMS
```

Windows, with minisign installed (`winget install jedisct1.minisign`):

```powershell
minisign -Vm SHA256SUMS -P <key>
(Get-FileHash backupproof-windows-amd64.exe).Hash   # compare with the line in SHA256SUMS
```

`install.sh` does both checks itself, using minisign or, if it isn't
installed, OpenSSL. It stops if the signature is missing or wrong. To install a
release from before signing began, pass `--allow-unsigned`; the checksums are
still checked. `install.ps1` checks the signature when minisign is installed,
and otherwise warns that it checked the checksums only, because Windows has no
built-in Ed25519 check.

## Rebuild a release yourself

Releases are reproducible: building a tag's source with the same Go version and
command gives the same bytes. `scripts/verify-build.sh` does it and compares:

```bash
sh scripts/verify-build.sh v0.4.0                  # linux/amd64
sh scripts/verify-build.sh v0.4.0 windows/amd64
```

It prints `MATCH` when the published binary is exactly what the source
builds. It needs git, curl and Go; the exact Go version of the release is
downloaded automatically. Releases with downloads start at 0.2.2; 0.2.2,
0.2.10, 0.2.20 and 0.2.23 were checked and match.

## Software bill of materials and provenance

From 0.3.0, each release also has:

- `sbom.spdx.json`, an SPDX list of every Go module built into the program,
  covered by `SHA256SUMS` and so by its signature;
- GitHub build provenance for every file, which shows the commit and the
  workflow run that built it:
  `gh attestation verify backupproof-linux-amd64 -R chmuzamil/BackupProof`.

## How releases are signed

The `release` workflow signs `SHA256SUMS` with the secret key from the GitHub
environment `release`, then checks the signature against
`scripts/minisign.pub` before publishing. A release fails if the key is
missing, is still the placeholder, or doesn't match.

The secret key exists in two places only: an offline copy, encrypted with its
password, and the `release` environment's secrets.

## Create the key (maintainers, once)

On a computer you trust, ideally offline:

```bash
minisign -G -p minisign.pub -s backupproof-release.key
```

Choose a strong password. Then:

1. Keep `backupproof-release.key` and its password offline, apart from each
   other (for example an encrypted USB drive and a password manager).
2. In GitHub, open **Settings → Environments → New environment**, name it
   `release`, limit it to tags matching `v*`, and add two secrets:
   `MINISIGN_SECRET_KEY` (the whole contents of `backupproof-release.key`) and
   `MINISIGN_PASSWORD`. Adding yourself as a required reviewer means every
   release waits for your approval.
3. Put the second line of `minisign.pub` into `scripts/minisign.pub`, the
   `PUBKEY` line of `scripts/install.sh` and the `$PubKey` line of
   `scripts/install.ps1`, and publish it on the website. CI checks that the
   three copies match.

## Replace the key

Planned replacement, with the old key still safe:

1. Create a new key as above.
2. Sign the new public key with the old one, so people who trust the old key
   can trust the new one:
   `minisign -S -s old.key -m minisign.pub -t "BackupProof key replaced by <new key ID> on <date>"`.
   Commit `minisign.pub` and `minisign.pub.minisig` under `docs/keys/<date>/`.
3. Update the three copies of the public key and the website, and replace the
   two secrets in the `release` environment.
4. Say so in the next release's notes. List the old key on the security page as
   retired, with its dates, so older releases can still be checked.

If the secret key may have leaked:

1. Remove the secrets from the `release` environment at once, so nothing more
   is signed with it.
2. Publish a security advisory naming the key ID and the time it may have
   leaked from.
3. Create a new key and publish it as above. Don't sign it with the leaked key.
4. Check the published files of recent releases against a fresh build of the
   same tag, re-sign their `SHA256SUMS` with the new key, and replace their
   `.minisig` files. Mark anything you can't check as untrusted.

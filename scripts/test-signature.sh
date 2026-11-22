#!/bin/sh
# Tests the release signature check in install.sh against real minisign
# signatures: valid ones pass, with minisign or with OpenSSL alone, and
# tampered files, comments or keys fail. Needs minisign and OpenSSL.
#   sh scripts/test-signature.sh
set -eu
cd "$(dirname "$0")/.."

# The key in install.sh must be the published one.
want=$(sed -n 2p scripts/minisign.pub)
have=$(sed -n 's/^PUBKEY="\(.*\)"$/\1/p' scripts/install.sh)
[ "$want" = "$have" ] || { echo "FAIL: install.sh has key $have, scripts/minisign.pub has $want"; exit 1; }
have=$(sed -n 's/^\$PubKey = "\(.*\)"$/\1/p' scripts/install.ps1)
[ "$want" = "$have" ] || { echo "FAIL: install.ps1 has key $have, scripts/minisign.pub has $want"; exit 1; }

eval "$(sed -n '/^# --- signature check/,/^# --- end signature check/p' scripts/install.sh)"

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
pw=test-password
printf '%s\n%s\n' "$pw" "$pw" | minisign -G -f -p "$T/key.pub" -s "$T/key.sec" >/dev/null
printf 'abc123  backupproof-linux-amd64\n' > "$T/SUMS"
printf '%s\n' "$pw" | minisign -S -s "$T/key.sec" -m "$T/SUMS" -t "BackupProof test SHA256SUMS" >/dev/null
printf '%s\n' "$pw" | minisign -S -l -s "$T/key.sec" -m "$T/SUMS" -x "$T/SUMS.legacy" -t "BackupProof test SHA256SUMS" >/dev/null
KEY=$(sed -n 2p "$T/key.pub")
printf '%s\n%s\n' "$pw" "$pw" | minisign -G -f -p "$T/other.pub" -s "$T/other.sec" >/dev/null
OTHER=$(sed -n 2p "$T/other.pub")

# A PATH with OpenSSL and the basic tools, but no minisign.
mkdir "$T/bin" "$T/bare"
for t in openssl sed tr wc head tail od cat cp mktemp rm; do
  ln -s "$(command -v "$t")" "$T/bin/$t"
  [ "$t" = openssl ] || ln -s "$(command -v "$t")" "$T/bare/$t"
done

fails=0
check() { # name want-rc path file sig key
  rc=0
  (PATH=$3; verify_signature "$4" "$5" "$6") || rc=$?
  if [ "$rc" = "$2" ]; then echo "ok    $1"; else echo "FAIL  $1: got $rc, want $2"; fails=$((fails + 1)); fi
}
check "prehashed, minisign" 0 "$PATH" "$T/SUMS" "$T/SUMS.minisig" "$KEY"
check "prehashed, OpenSSL" 0 "$T/bin" "$T/SUMS" "$T/SUMS.minisig" "$KEY"
check "legacy, OpenSSL" 0 "$T/bin" "$T/SUMS" "$T/SUMS.legacy" "$KEY"
check "wrong key, minisign" 1 "$PATH" "$T/SUMS" "$T/SUMS.minisig" "$OTHER"
check "wrong key, OpenSSL" 1 "$T/bin" "$T/SUMS" "$T/SUMS.minisig" "$OTHER"
check "no minisign or OpenSSL" 2 "$T/bare" "$T/SUMS" "$T/SUMS.minisig" "$KEY"

cp "$T/SUMS" "$T/SUMS.bad"; printf 'def456  install.sh\n' >> "$T/SUMS.bad"
check "changed file, minisign" 1 "$PATH" "$T/SUMS.bad" "$T/SUMS.minisig" "$KEY"
check "changed file, OpenSSL" 1 "$T/bin" "$T/SUMS.bad" "$T/SUMS.minisig" "$KEY"
check "changed file, legacy, OpenSSL" 1 "$T/bin" "$T/SUMS.bad" "$T/SUMS.legacy" "$KEY"

sed '3s/.*/trusted comment: BackupProof v9.9.9 SHA256SUMS/' "$T/SUMS.minisig" > "$T/SUMS.comment"
check "changed trusted comment, minisign" 1 "$PATH" "$T/SUMS" "$T/SUMS.comment" "$KEY"
check "changed trusted comment, OpenSSL" 1 "$T/bin" "$T/SUMS" "$T/SUMS.comment" "$KEY"
printf 'not a signature\n' > "$T/SUMS.junk"
check "not a signature, OpenSSL" 1 "$T/bin" "$T/SUMS" "$T/SUMS.junk" "$KEY"

[ "$fails" = 0 ] || { echo "$fails failed"; exit 1; }
echo "all signature checks passed"

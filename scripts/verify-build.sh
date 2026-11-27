#!/bin/sh
# Rebuilds a BackupProof release from its tag's source and checks the result
# is byte for byte the binary in that release. Needs git, Go (any recent
# version: the exact one the release used is downloaded) and curl.
#   sh scripts/verify-build.sh v0.3.0 [linux/amd64]
# Platforms: linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
# This proves the binary matches the source. To prove the release itself is
# genuine, also check SHA256SUMS's signature (docs/SIGNING.md).
set -eu
TAG=${1:?usage: sh scripts/verify-build.sh TAG [OS/ARCH]}
PLAT=${2:-linux/amd64}
os=${PLAT%/*}
arch=${PLAT#*/}
ext=
[ "$os" = windows ] && ext=.exe
name=backupproof-$os-$arch$ext
REPO=https://github.com/chmuzamil/BackupProof

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

git clone -q --depth 1 --branch "$TAG" "$REPO" "$WORK/src" 2>/dev/null
gover=$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' "$WORK/src/go.mod")
echo "building $name from $TAG with Go $gover…"
# The same command as the release workflow (.github/workflows/release.yml).
(cd "$WORK/src" && GOTOOLCHAIN=go$gover CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
  -ldflags "-s -w -X github.com/chmuzamil/backupproof/internal/engine.Version=backupproof/${TAG#v}" \
  -o "$WORK/$name" ./cmd/backupproof)

curl -fsSL "$REPO/releases/download/$TAG/SHA256SUMS" -o "$WORK/SHA256SUMS"
want=$(awk -v n="$name" '$2 == n { print $1 }' "$WORK/SHA256SUMS")
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$WORK/$name" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$WORK/$name" | cut -d' ' -f1)
fi
echo "published: $want"
echo "rebuilt:   $got"
if [ -n "$want" ] && [ "$want" = "$got" ]; then
  echo "MATCH: $name in $TAG is built from that tag's source."
else
  echo "MISMATCH: the rebuilt binary differs from the published one." >&2
  exit 1
fi

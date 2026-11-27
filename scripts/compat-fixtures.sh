#!/bin/sh
# Makes compatibility fixtures: for each release tag matching PATTERN, builds
# that release's own command-line tool, backs up a small folder, runs a restore
# test and exports both signed proofs with the signing key's public half.
# With REPODIR it also keeps the storage itself, after a second backup of the
# changed folder, so newer versions can be checked to still restore it.
#   sh scripts/compat-fixtures.sh 'v0.2.*' OUTDIR [REPODIR]
# Run by the compat-fixtures workflow on a host named compat-fixtures; the
# results go in internal/proof/testdata/compat/ (see docs/COMPATIBILITY.md).
set -eu
PATTERN=$1
OUT=$(mkdir -p "$2" && cd "$2" && pwd)
REPOS=
if [ -n "${3:-}" ]; then REPOS=$(mkdir -p "$3" && cd "$3" && pwd); fi
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"; git worktree prune' EXIT

mkdir -p "$WORK/src/docs"
printf 'BackupProof compatibility fixture\n' > "$WORK/src/readme.txt"
printf 'a nested file\n' > "$WORK/src/docs/notes.txt"

for tag in $(git tag -l "$PATTERN" | sort -V); do
  v=${tag#v}
  git worktree add -q --detach "$WORK/tree" "$tag"
  (cd "$WORK/tree" && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/chmuzamil/backupproof/internal/engine.Version=backupproof/$v" \
    -o "$WORK/backupproof" ./cmd/backupproof)
  git worktree remove --force "$WORK/tree"

  dir="$OUT/$tag"
  mkdir -p "$dir"
  export BP_STATE="$WORK/state-$tag" BP_PASSWORD=compat-fixture-password
  repo="$WORK/repo-$tag"
  (cd "$WORK/src" && "$WORK/backupproof" init --repo "$repo" >/dev/null &&
    "$WORK/backupproof" backup --repo "$repo" --name files . >/dev/null &&
    "$WORK/backupproof" drill --repo "$repo" --name files >/dev/null)
  "$WORK/backupproof" proof key > "$dir/key.txt"
  # proof list: DIGEST, CREATED (date and time), KIND, ...
  "$WORK/backupproof" proof list --repo "$repo" | awk 'NR > 1 { print $1, $4 }' | while read -r digest kind; do
    "$WORK/backupproof" proof export --repo "$repo" "$digest" -o "$dir/$kind.bundle.json"
  done
  if [ -n "$REPOS" ]; then
    # A second backup after a change: one new file, one edited, one unchanged,
    # so the kept storage also has reused chunks and two snapshots.
    printf 'added after the first backup\n' > "$WORK/src/docs/added.txt"
    printf 'BackupProof compatibility fixture, edited\n' > "$WORK/src/readme.txt"
    (cd "$WORK/src" && "$WORK/backupproof" backup --repo "$repo" --name files . >/dev/null)
    rm -f "$WORK/src/docs/added.txt"
    printf 'BackupProof compatibility fixture\n' > "$WORK/src/readme.txt"
    cp -R "$repo" "$REPOS/$tag"
  fi
  rm -f "$WORK/backupproof"
  echo "$tag: $(ls "$dir" | tr '\n' ' ')"
done

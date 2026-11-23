#!/bin/sh
# Makes dashboard compatibility fixtures: for each release tag matching
# PATTERN, builds that release, starts its dashboard (with the built-in agent)
# and runs tools/compatgen against it.
#   sh scripts/compat-dashboard.sh 'v0.2.*' OUTDIR [host:port:user:password:db]
# Run by the compat-fixtures workflow; see docs/COMPATIBILITY.md.
set -eu
PATTERN=$1
OUT=$(mkdir -p "$2" && cd "$2" && pwd)
PG=${3:-}
WORK=$(mktemp -d)
trap 'kill "$SERVER" 2>/dev/null || true; rm -rf "$WORK"; git worktree prune' EXIT
SERVER=

go build -o "$WORK/compatgen" ./tools/compatgen
for tag in $(git tag -l "$PATTERN" | sort -V); do
  v=${tag#v}
  git worktree add -q --detach "$WORK/tree" "$tag"
  (cd "$WORK/tree" && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/chmuzamil/backupproof/internal/engine.Version=backupproof/$v" \
    -o "$WORK/backupproof" ./cmd/backupproof)
  git worktree remove --force "$WORK/tree"

  data="$WORK/data-$tag"
  "$WORK/backupproof" server --data "$data" --listen 127.0.0.1:8420 > "$WORK/server-$tag.log" 2>&1 &
  SERVER=$!
  if ! "$WORK/compatgen" -url http://127.0.0.1:8420 -work "$WORK/work-$tag" -out "$OUT/$tag" ${PG:+-postgres "$PG"}; then
    echo "$tag: generator failed"; tail -20 "$WORK/server-$tag.log"
  fi
  kill "$SERVER"; wait "$SERVER" 2>/dev/null || true
  SERVER=
  rm -f "$WORK/backupproof"
done

#!/bin/sh
# BackupProof dashboard installer for Linux and macOS.
#
#   curl -fsSL https://github.com/chmuzamil/BackupProof/releases/latest/download/install.sh | sudo sh
#
# Options (pass after `sh -s --`):
#   --version vX.Y.Z    install a specific release (default: latest)
#   --listen ADDR       address to listen on (default: 0.0.0.0:8420)
#   --public-url URL    address other servers use to reach the dashboard
#   --uninstall         stop and remove the service and binary (data is kept)
#
# Running it again upgrades BackupProof in place; your data is kept.
set -eu

REPO="chmuzamil/BackupProof"
VERSION="latest"
LISTEN="0.0.0.0:8420"
PUBLIC_URL=""
UNINSTALL=0
BIN=/usr/local/bin/backupproof

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --listen) LISTEN="$2"; shift 2 ;;
    --public-url) PUBLIC_URL="$2"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help) sed -n '2,13p' "$0" 2>/dev/null || true; exit 0 ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done

say() { printf '\033[1m%s\033[0m\n' "$*"; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || fail "please run with sudo (the installer creates a system service)."

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in linux|darwin) ;; *) fail "unsupported system: $OS (use install.ps1 on Windows)";; esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "unsupported CPU: $(uname -m)" ;;
esac

if [ "$OS" = linux ]; then
  DATA=/var/lib/backupproof
  UNIT=/etc/systemd/system/backupproof.service
else
  DATA="/Library/Application Support/BackupProof"
  UNIT=/Library/LaunchDaemons/dev.backupproof.server.plist
fi

if [ "$UNINSTALL" = 1 ]; then
  say "Removing BackupProof (your data in $DATA is kept)…"
  if [ "$OS" = linux ]; then
    systemctl disable --now backupproof 2>/dev/null || true
  else
    launchctl unload -w "$UNIT" 2>/dev/null || true
  fi
  rm -f "$UNIT" "$BIN"
  [ "$OS" = linux ] && systemctl daemon-reload || true
  say "Done."
  exit 0
fi

if [ "$VERSION" = latest ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fetch() { curl -fsSL --retry 3 -o "$2" "$1" || fail "could not download $1 (is the release public?)"; }

say "Downloading BackupProof ($VERSION) for $OS/$ARCH…"
NAME="backupproof-$OS-$ARCH"
fetch "$BASE/$NAME" "$TMP/$NAME"
fetch "$BASE/SHA256SUMS" "$TMP/SHA256SUMS"

say "Checking the download…"
# sha256sum writes "<hash>  name" or "<hash> *name" (binary mode)
EXPECTED=$(grep -E "[ *]$NAME\$" "$TMP/SHA256SUMS" | awk '{print $1}')
[ -n "$EXPECTED" ] || fail "no checksum for $NAME in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL=$(sha256sum "$TMP/$NAME" | awk '{print $1}')
else
  ACTUAL=$(shasum -a 256 "$TMP/$NAME" | awk '{print $1}')
fi
[ "$EXPECTED" = "$ACTUAL" ] || fail "checksum mismatch for $NAME; the download is damaged or tampered with"

FRESH=1
[ -f "$DATA/backupproof.db" ] && FRESH=0
install -m 0755 "$TMP/$NAME" "$BIN"
mkdir -p "$DATA"
chmod 700 "$DATA"


if [ "$OS" = linux ]; then
  command -v systemctl >/dev/null 2>&1 || fail "systemd not found; start it yourself with: $BIN server --data $DATA --listen $LISTEN"
  cat > "$UNIT" <<UNITEOF
[Unit]
Description=BackupProof dashboard and built-in agent
After=network-online.target docker.service
Wants=network-online.target

[Service]
# Runs as root so the built-in agent can read every folder you choose to protect.
ExecStart=$BIN server --data $DATA --listen $LISTEN${PUBLIC_URL:+ --public-url $PUBLIC_URL}
Restart=on-failure
RestartSec=5
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
UNITEOF
  systemctl daemon-reload
  systemctl enable backupproof >/dev/null 2>&1
  systemctl restart backupproof
else
  PLIST_URL=""
  [ -n "$PUBLIC_URL" ] && PLIST_URL="<string>--public-url</string><string>$PUBLIC_URL</string>"
  cat > "$UNIT" <<PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.backupproof.server</string>
  <key>ProgramArguments</key><array>
    <string>$BIN</string><string>server</string>
    <string>--data</string><string>$DATA</string>
    <string>--listen</string><string>$LISTEN</string>$PLIST_URL
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>$DATA/server.log</string>
</dict></plist>
PLISTEOF
  launchctl unload "$UNIT" 2>/dev/null || true
  launchctl load -w "$UNIT"
fi

# Wait for the first start, which creates the one-time setup code.
if [ "$FRESH" = 1 ]; then
  i=0
  while [ $i -lt 30 ] && [ ! -f "$DATA/setup-code.txt" ]; do sleep 1; i=$((i+1)); done
fi

PORT=${LISTEN##*:}
HOST=$(hostname -I 2>/dev/null | awk '{print $1}')
[ -n "$HOST" ] || HOST=$(ipconfig getifaddr en0 2>/dev/null || echo localhost)
URL=${PUBLIC_URL:-http://$HOST:$PORT}

echo
say "BackupProof is running."
echo "  Open:        $URL"
if [ -f "$DATA/setup-code.txt" ]; then
  echo "  Setup code:  $(cat "$DATA/setup-code.txt")   (needed once, to create the admin account)"
fi
echo "  Data:        $DATA"
echo
echo "For use over the internet, put it behind HTTPS (for example Caddy:"
echo "  backup.example.com { reverse_proxy 127.0.0.1:$PORT } )"
echo "and re-run this installer with --public-url https://backup.example.com"

#!/bin/sh
# BackupProof dashboard installer for Linux and macOS.
#
#   curl -fsSL https://github.com/chmuzamil/BackupProof/releases/latest/download/install.sh | sudo sh
#
# Options (pass after `sh -s --`):
#   --domain NAME       serve the dashboard at https://NAME: sets up nginx (with
#                       certbot) or Caddy and a free certificate (Linux). Point
#                       NAME's DNS at this server first.
#   --email ADDR        email for certificate expiry notices (with --domain)
#   --version vX.Y.Z    install a specific release (default: latest)
#   --listen ADDR       address to listen on (default: 0.0.0.0:8420,
#                       or 127.0.0.1:8420 with --domain)
#   --public-url URL    address other servers use to reach the dashboard
#   --uninstall         stop and remove the service and binary (data is kept)
#
# Running it again upgrades BackupProof in place and keeps your data and the
# --listen and --public-url settings of the existing installation.
set -eu

REPO="chmuzamil/BackupProof"
VERSION="latest"
LISTEN=""
PUBLIC_URL=""
DOMAIN=""
EMAIL=""
UNINSTALL=0
BIN=/usr/local/bin/backupproof

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --listen) LISTEN="$2"; shift 2 ;;
    --public-url) PUBLIC_URL="$2"; shift 2 ;;
    --domain) DOMAIN="$2"; shift 2 ;;
    --email) EMAIL="$2"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help) sed -n '2,20p' "$0" 2>/dev/null || true; exit 0 ;;
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

# Re-running keeps the existing installation's settings unless new ones are given.
if [ -f "$UNIT" ]; then
  if [ "$OS" = linux ]; then
    OLD_LISTEN=$(sed -n 's/^ExecStart=.* --listen \([^ ]*\).*/\1/p' "$UNIT")
    OLD_PUBLIC=$(sed -n 's/^ExecStart=.* --public-url \([^ ]*\).*/\1/p' "$UNIT")
  else
    OLD_LISTEN=$(sed -n 's/.*<string>--listen<\/string><string>\([^<]*\)<\/string>.*/\1/p' "$UNIT")
    OLD_PUBLIC=$(sed -n 's/.*<string>--public-url<\/string><string>\([^<]*\)<\/string>.*/\1/p' "$UNIT")
  fi
  [ -n "$LISTEN" ] || LISTEN="$OLD_LISTEN"
  [ -n "$PUBLIC_URL" ] || [ -n "$DOMAIN" ] || PUBLIC_URL="$OLD_PUBLIC"
fi
if [ -n "$DOMAIN" ]; then
  DOMAIN=$(printf '%s' "$DOMAIN" | sed -e 's#^https\{0,1\}://##' -e 's#/.*$##' | tr '[:upper:]' '[:lower:]')
  printf '%s' "$DOMAIN" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$' || fail "--domain needs a name like backup.example.com"
  PUBLIC_URL="https://$DOMAIN"
  # Behind the HTTPS proxy the dashboard only needs to be reachable locally.
  [ -n "$LISTEN" ] || LISTEN="127.0.0.1:8420"
fi
[ -n "$LISTEN" ] || LISTEN="0.0.0.0:8420"

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


# ------------------------------------------------------------------ HTTPS
# Sets up a reverse proxy with a free Let's Encrypt certificate for --domain.
# Never fails the install: on any problem it explains what to do instead.
https_manual() {
  echo
  echo "Set up HTTPS yourself: point $DOMAIN at this server and proxy it to 127.0.0.1:$PORT,"
  echo "for example with Caddy:  $DOMAIN { reverse_proxy 127.0.0.1:$PORT }"
  HTTPS_OK=0
}

pkg_install() {
  if command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$@" >/dev/null 2>&1 ||
      { apt-get update -q >/dev/null 2>&1 && DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$@" >/dev/null 2>&1; }
  elif command -v dnf >/dev/null 2>&1; then dnf install -y -q "$@" >/dev/null 2>&1
  elif command -v yum >/dev/null 2>&1; then yum install -y -q "$@" >/dev/null 2>&1
  else return 1; fi
}

port_busy() { ss -ltn 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]$1\$"; }

dns_check() {
  ADDRS=$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk '{print $1}' | sort -u)
  if [ -z "$ADDRS" ]; then
    echo "  $DOMAIN doesn't resolve yet. Add a DNS A record pointing it at this server,"
    echo "  wait a few minutes, then run this installer again with the same options."
    return 1
  fi
  MINE="$(hostname -I 2>/dev/null) $(curl -fsS -m 5 https://api.ipify.org 2>/dev/null || true)"
  for a in $ADDRS; do
    for m in $MINE; do [ "$a" = "$m" ] && return 0; done
  done
  echo "  $DOMAIN points to $(printf '%s' "$ADDRS" | tr -s '\n' ' '), but this server is $(printf '%s' "$MINE" | tr -s ' \n' ' ' | sed 's/^ //; s/ $//')."
  echo "  Fix the DNS record, wait a few minutes, then run this installer again with the same options."
  return 1
}

https_nginx() {
  if [ -d /etc/nginx/sites-available ]; then
    SITE=/etc/nginx/sites-available/backupproof-$DOMAIN.conf
    LINK=/etc/nginx/sites-enabled/backupproof-$DOMAIN.conf
  else
    SITE=/etc/nginx/conf.d/backupproof-$DOMAIN.conf
    LINK=""
  fi
  # Written once; on later runs certbot's HTTPS additions are kept.
  if [ ! -f "$SITE" ]; then
    cat > "$SITE" <<NGINXEOF
# BackupProof dashboard (written by the BackupProof installer)
server {
    listen 80;
    listen [::]:80;
    server_name $DOMAIN;
    client_max_body_size 10m;

    location / {
        proxy_pass http://127.0.0.1:$PORT;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$remote_addr;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_read_timeout 120s;
    }
}
NGINXEOF
    [ -z "$LINK" ] || ln -sf "$SITE" "$LINK"
  fi
  if ! nginx -t >/dev/null 2>&1; then
    echo "  nginx's configuration has an error (see: nginx -t), so it wasn't reloaded."
    https_manual; return
  fi
  systemctl reload nginx || { echo "  Couldn't reload nginx."; https_manual; return; }
  if ! command -v certbot >/dev/null 2>&1; then
    say "Installing certbot…"
    pkg_install certbot python3-certbot-nginx || { echo "  Couldn't install certbot."; https_manual; return; }
  fi
  say "Getting a certificate for $DOMAIN…"
  if [ -n "$EMAIL" ]; then set -- -m "$EMAIL"; else set -- --register-unsafely-without-email; fi
  if certbot --nginx -d "$DOMAIN" --non-interactive --agree-tos --redirect --keep-until-expiring "$@" >"$TMP/certbot.log" 2>&1; then
    HTTPS_OK=1
  else
    echo "  certbot couldn't get a certificate:"; tail -5 "$TMP/certbot.log" | sed 's/^/    /'
    https_manual
  fi
}

https_caddy() {
  CF=/etc/caddy/Caddyfile
  mkdir -p /etc/caddy
  # Replace the package's placeholder site (":80 { root … file_server }"); otherwise add ours once.
  if [ ! -s "$CF" ] || ! grep -Ev '^[[:space:]]*(#|$)' "$CF" | grep -Evq '^[[:space:]]*(:80[[:space:]]*\{|root[[:space:]].*|file_server.*|\})[[:space:]]*$'; then
    : > "$CF"
  fi
  if ! grep -q "^$DOMAIN " "$CF"; then
    printf '\n# BackupProof dashboard (written by the BackupProof installer)\n%s {\n\treverse_proxy 127.0.0.1:%s\n}\n' "$DOMAIN" "$PORT" >> "$CF"
  fi
  if ! caddy validate --config "$CF" --adapter caddyfile >/dev/null 2>&1; then
    echo "  The Caddyfile has an error (see: caddy validate --config $CF)."
    https_manual; return
  fi
  systemctl enable caddy >/dev/null 2>&1 || true
  systemctl reload caddy 2>/dev/null || systemctl restart caddy
  # Caddy gets the certificate in the background; give it a moment.
  i=0
  while [ $i -lt 30 ]; do
    if curl -fsS -o /dev/null -m 5 "https://$DOMAIN/api/status" 2>/dev/null; then HTTPS_OK=1; return; fi
    sleep 2; i=$((i+1))
  done
  echo "  Caddy is still getting the certificate; it can take a minute. Check with: journalctl -u caddy"
  HTTPS_OK=1
}

setup_https() {
  HTTPS_OK=0
  if [ "$OS" != linux ]; then echo "Automatic HTTPS is only available on Linux."; https_manual; return; fi
  say "Setting up HTTPS for $DOMAIN…"
  dns_check || { https_manual; return; }
  if command -v nginx >/dev/null 2>&1 && systemctl is-active --quiet nginx; then
    https_nginx
  elif command -v caddy >/dev/null 2>&1; then
    https_caddy
  elif port_busy 80 || port_busy 443; then
    echo "  Another web server is using port 80 or 443, and it isn't nginx or Caddy."
    https_manual
  else
    say "Installing Caddy…"
    if pkg_install caddy && command -v caddy >/dev/null 2>&1; then
      https_caddy
    else
      echo "  Couldn't install Caddy from your system's packages; see https://caddyserver.com/docs/install"
      https_manual
    fi
  fi
}

HTTPS_OK=0
[ -z "$DOMAIN" ] || setup_https

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
if [ -n "$DOMAIN" ] && [ "$HTTPS_OK" = 0 ]; then
  echo
  echo "HTTPS isn't set up yet (see above), so $PUBLIC_URL won't open until it is."
elif [ -z "$DOMAIN" ]; then
  case "$PUBLIC_URL" in
    https://*) ;;
    *)
      echo
      echo "To use it over the internet, point a domain name at this server and run this"
      echo "installer again with --domain, which sets up HTTPS for you:"
      echo "  curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | sudo sh -s -- --domain backup.example.com"
      ;;
  esac
fi

#!/usr/bin/env bash
# Install or upgrade event-center on cloud-server.
#
# Run this from your workstation (it builds for linux/amd64, uploads the
# artifact and drives systemd over SSH). It is idempotent: re-running it
# upgrades the binary and restarts the service without touching the database
# or the secrets file.
#
#   deploy/cloud-server/install.sh                      # defaults: cloud-server, 127.0.0.1:9099
#   PORT=9099 BIND=0.0.0.0 deploy/cloud-server/install.sh
#   EVENTD_GITHUB_SECRET=xxx deploy/cloud-server/install.sh   # seed the GitHub source
#
# Secrets: EVENTD_ADMIN_TOKEN is generated on the host the first time (never
# transmitted, never committed) and stored in
# /opt/event-center/event-center.env, mode 0600. Rotate it by editing that file
# and restarting the unit.
set -euo pipefail

HOST="${HOST:-cloud-server}"
PORT="${PORT:-9099}"
BIND="${BIND:-127.0.0.1}"
APP_DIR="${APP_DIR:-/opt/event-center}"
UNIT="event-center.service"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=15 "$HOST")
SCP=(scp -q -o BatchMode=yes)

echo "==> building linux/amd64 binary"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)" \
  -o /tmp/eventd.new "$ROOT/cmd/eventd"

echo "==> uploading to $HOST"
"${SCP[@]}" /tmp/eventd.new "$HOST:/tmp/eventd.new"
"${SCP[@]}" "$ROOT/deploy/cloud-server/$UNIT" "$HOST:/tmp/$UNIT"

echo "==> installing into $APP_DIR (bind $BIND:$PORT)"
"${SSH[@]}" bash -s -- "$APP_DIR" "$BIND" "$PORT" "$UNIT" "${EVENTD_GITHUB_SECRET:-}" <<'REMOTE'
set -euo pipefail
APP_DIR="$1"; BIND="$2"; PORT="$3"; UNIT="$4"; GH_SECRET="${5:-}"

install -d -m 0755 "$APP_DIR" "$APP_DIR/data"
install -m 0755 /tmp/eventd.new "$APP_DIR/eventd"

# Secrets file: created once, never overwritten, never uploaded.
ENV_FILE="$APP_DIR/event-center.env"
if [ ! -f "$ENV_FILE" ]; then
  TOKEN="$(openssl rand -hex 32)"
  umask 077
  cat > "$ENV_FILE" <<EOF
# event-center runtime secrets — root only, never commit.
EVENTD_ADMIN_TOKEN=$TOKEN
EVENTD_GITHUB_SECRET=$GH_SECRET
EVENTD_HTTP_ADDR=$BIND:$PORT
EVENTD_DB_PATH=$APP_DIR/data/eventd.db
EOF
  echo "generated new admin token (see $ENV_FILE; shown once below)"
  echo "EVENTD_ADMIN_TOKEN=$TOKEN"
else
  echo "keeping existing $ENV_FILE"
  # Keep the listen address in sync with the requested port without clobbering secrets.
  if grep -q '^EVENTD_HTTP_ADDR=' "$ENV_FILE"; then
    sed -i "s|^EVENTD_HTTP_ADDR=.*|EVENTD_HTTP_ADDR=$BIND:$PORT|" "$ENV_FILE"
  else
    echo "EVENTD_HTTP_ADDR=$BIND:$PORT" >> "$ENV_FILE"
  fi
  chmod 600 "$ENV_FILE"
fi

install -m 0644 /tmp/$UNIT /etc/systemd/system/$UNIT
systemctl daemon-reload
systemctl enable "$UNIT" >/dev/null 2>&1 || true
systemctl restart "$UNIT"
sleep 2
systemctl is-active "$UNIT"
curl -s -m 5 "http://127.0.0.1:$PORT/healthz" && echo
rm -f /tmp/eventd.new /tmp/$UNIT
REMOTE

echo "==> done"

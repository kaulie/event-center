#!/usr/bin/env bash
# Install or upgrade event-center on cloud-server.
#
# Run this from your workstation (it builds for linux/amd64, uploads the
# artifact and drives systemd over SSH). It is idempotent: re-running it
# upgrades the binary and restarts the service without touching the database
# or the secrets file.
#
#   deploy/cloud-server/install.sh                            # cloud-server, 127.0.0.1:9099
#   EC_PORT=9099 EC_BIND=0.0.0.0 deploy/cloud-server/install.sh
#   EVENTD_GITHUB_SECRET=xxx deploy/cloud-server/install.sh   # seed the GitHub source
#
# Secrets: EVENTD_ADMIN_TOKEN is generated on the host the first time (never
# transmitted, never committed) and stored in
# /opt/event-center/event-center.env, mode 0600. Rotate it by editing that file
# and restarting the unit.
#
# NOTE: every tunable is EC_-prefixed on purpose. Generic names like HOST and
# PORT are commonly already set in CI/sandbox environments and silently
# redirecting those would target the wrong host or the wrong port.
set -euo pipefail

EC_HOST="${EC_HOST:-cloud-server}"
EC_PORT="${EC_PORT:-9099}"
EC_BIND="${EC_BIND:-127.0.0.1}"
EC_APP_DIR="${EC_APP_DIR:-/opt/event-center}"
EC_UNIT="event-center.service"

case "$EC_HOST" in
  ""|0.0.0.0|"::"|localhost|127.0.0.1)
    echo "EC_HOST must be an SSH target, got '$EC_HOST'" >&2
    exit 1
    ;;
esac
case "$EC_PORT" in
  *[!0-9]*|"") echo "EC_PORT must be a number, got '$EC_PORT'" >&2; exit 1 ;;
esac
if [ "$EC_PORT" -lt 1024 ] || [ "$EC_PORT" -gt 65535 ]; then
  echo "EC_PORT out of range: $EC_PORT" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=15 "$EC_HOST")
SCP=(scp -q -o BatchMode=yes)

echo "==> building linux/amd64 binary"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)" \
  -o /tmp/eventd.new "$ROOT/cmd/eventd"

echo "==> uploading to $EC_HOST"
"${SCP[@]}" /tmp/eventd.new "$EC_HOST:/tmp/eventd.new"
"${SCP[@]}" "$ROOT/deploy/cloud-server/$EC_UNIT" "$EC_HOST:/tmp/$EC_UNIT"
"${SCP[@]}" "$ROOT/deploy/cloud-server/logrotate-event-center" "$EC_HOST:/tmp/logrotate-event-center"

echo "==> installing into $EC_APP_DIR (bind $EC_BIND:$EC_PORT)"
"${SSH[@]}" bash -s -- "$EC_APP_DIR" "$EC_BIND" "$EC_PORT" "$EC_UNIT" "${EVENTD_GITHUB_SECRET:-}" "${EC_FORCE:-}" <<'REMOTE'
set -euo pipefail
APP_DIR="$1"; BIND="$2"; PORT="$3"; UNIT="$4"; GH_SECRET="${5:-}"; FORCE="${6:-}"

# Safety: never adopt (and therefore never overwrite) a directory that is not
# ours. Existing deployments are recognised by their own files, so upgrades keep
# working while a name collision with somebody else's data is refused.
if [ -d "$APP_DIR" ] && [ -n "$(ls -A "$APP_DIR" 2>/dev/null)" ]; then
  if [ ! -f "$APP_DIR/event-center.env" ] && [ ! -f "$APP_DIR/eventd" ]; then
    if [ "$FORCE" != "1" ]; then
      echo "refusing to install into $APP_DIR: it exists and was not created by"
      echo "this installer (no eventd/event-center.env inside)."
      echo "Set EC_FORCE=1 to override, or choose another EC_APP_DIR."
      exit 3
    fi
    echo "WARNING: EC_FORCE=1, installing into a pre-existing directory $APP_DIR"
  fi
fi

# Nothing below deletes anything. `install -d` only creates missing paths and
# the only files written are eventd, event-center.env (first run only) and the
# systemd unit.
install -d -m 0755 "$APP_DIR" "$APP_DIR/data"
install -m 0755 /tmp/eventd.new "$APP_DIR/eventd"

# Ingress audit trail lives outside the app dir so logrotate can manage it and
# so it is obvious this is evidence, not state.
LOG_DIR=/var/log/event-center
AUDIT_FILE="$LOG_DIR/ingress.jsonl"
install -d -m 0750 "$LOG_DIR"
touch "$AUDIT_FILE"
chmod 0600 "$AUDIT_FILE"
install -m 0644 /tmp/logrotate-event-center /etc/logrotate.d/event-center

ENV_FILE="$APP_DIR/event-center.env"

# ensure_env adds KEY on first sight without ever clobbering an existing value.
ensure_env() { # KEY VALUE
  if ! grep -q "^$1=" "$ENV_FILE" 2>/dev/null; then
    echo "$1=$2" >> "$ENV_FILE"
    echo "  + $1"
  fi
}

# set_env replaces a key, used only for values the operator explicitly passed.
# Built with grep+append rather than sed so arbitrary secrets (slashes, ampersands)
# cannot break the substitution.
set_env() { # KEY VALUE
  local tmp
  tmp="$(mktemp)"
  grep -v "^$1=" "$ENV_FILE" 2>/dev/null > "$tmp" || true
  printf '%s=%s\n' "$1" "$2" >> "$tmp"
  cat "$tmp" > "$ENV_FILE"
  rm -f "$tmp"
}

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
  # Keep the listen address in sync with the requested port without touching secrets.
  if grep -q '^EVENTD_HTTP_ADDR=' "$ENV_FILE"; then
    sed -i "s|^EVENTD_HTTP_ADDR=.*|EVENTD_HTTP_ADDR=$BIND:$PORT|" "$ENV_FILE"
  else
    echo "EVENTD_HTTP_ADDR=$BIND:$PORT" >> "$ENV_FILE"
  fi
  chmod 600 "$ENV_FILE"
fi

# Durable ingress audit trail: these are added on upgrade too, so an existing
# deployment gains the audit file without its secrets being touched.
ensure_env EVENTD_INGRESS_LOG_PATH "$AUDIT_FILE"
ensure_env EVENTD_INGRESS_LOG_BODY_MAX 8192

# The GitHub secret is only written when explicitly supplied on the command
# line, so a routine upgrade never clears it.
if [ -n "$GH_SECRET" ]; then
  set_env EVENTD_GITHUB_SECRET "$GH_SECRET"
  echo "  ~ EVENTD_GITHUB_SECRET updated (explicitly supplied)"
fi
chmod 600 "$ENV_FILE"

install -m 0644 /tmp/$UNIT /etc/systemd/system/$UNIT
systemctl daemon-reload
systemctl enable "$UNIT" >/dev/null 2>&1 || true
systemctl restart "$UNIT"
sleep 2
systemctl is-active "$UNIT"
curl -s -m 5 "http://127.0.0.1:$PORT/healthz" && echo
rm -f /tmp/eventd.new /tmp/$UNIT /tmp/logrotate-event-center

if [ ! -d /var/log/journal ]; then
  echo
  echo "NOTE: journald is currently VOLATILE on this host (/var/log/journal is"
  echo "missing), so 'journalctl -u event-center' only covers the current boot."
  echo "The ingress audit trail above is unaffected — it is a real file."
  echo "To also persist journald across reboots (host-wide, your call):"
  echo "  mkdir -p /var/log/journal && systemd-tmpfiles --create --prefix /var/log/journal && systemctl restart systemd-journald"
fi
REMOTE

echo "==> done"

#!/usr/bin/env bash
# Set up the public HTTPS edge for event-center on cloud-server.
#
# Adds (does not modify) two nginx server blocks and obtains a Let's Encrypt
# certificate for <name>.<ip-dashed>.sslip.io — sslip.io resolves any
# sub-domain back to the embedded IP, so no DNS work is needed.
#
#   deploy/cloud-server/setup-nginx.sh
#   EC_TLS_HOST=events.example.com deploy/cloud-server/setup-nginx.sh   # own domain
#
# Idempotent: re-running refreshes the blocks, keeps the existing certificate
# and never touches other sites' configuration.
set -euo pipefail

EC_HOST="${EC_HOST:-cloud-server}"
EC_IP="${EC_IP:-115.190.153.53}"
EC_TLS_HOST="${EC_TLS_HOST:-event-center.$(echo "$EC_IP" | tr . -).sslip.io}"
EC_ACME_WEBROOT="${EC_ACME_WEBROOT:-/var/www/acme}"
EC_LE_EMAIL="${EC_LE_EMAIL:-}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=15 "$EC_HOST")
SCP=(scp -q -o BatchMode=yes)

echo "==> edge host: $EC_TLS_HOST  (proxying to 127.0.0.1:9099)"
"${SCP[@]}" "$ROOT/deploy/cloud-server/nginx/event-center-acme.conf" "$EC_HOST:/tmp/ec-acme.conf"
"${SCP[@]}" "$ROOT/deploy/cloud-server/nginx/event-center-tls.conf" "$EC_HOST:/tmp/ec-tls.conf"

"${SSH[@]}" bash -s -- "$EC_TLS_HOST" "$EC_ACME_WEBROOT" "$EC_LE_EMAIL" <<'REMOTE'
set -euo pipefail
# "${3:-}" style defaults: ssh joins its arguments with spaces before the remote
# shell parses them, so a trailing empty argument simply disappears.
HOST_NAME="${1:?tls host required}"; WEBROOT="${2:?webroot required}"; EMAIL="${3:-}"

command -v nginx >/dev/null || { echo "nginx is not installed" >&2; exit 1; }
command -v certbot >/dev/null || { echo "certbot is not installed" >&2; exit 1; }

# 1. ACME webroot + HTTP block, so the certificate can be issued and renewed.
install -d -m 0755 "$WEBROOT/.well-known/acme-challenge"
sed "s/__TLS_HOST__/$HOST_NAME/g" /tmp/ec-acme.conf > /etc/nginx/conf.d/event-center-acme.conf
nginx -t
systemctl reload nginx
echo "  acme block installed and nginx reloaded"

# 2. Certificate (reuse the existing ACME account when there is one).
if [ ! -d "/etc/letsencrypt/live/$HOST_NAME" ]; then
  echo "  requesting certificate for $HOST_NAME"
  ARGS=(certonly --webroot -w "$WEBROOT" -d "$HOST_NAME" --non-interactive --agree-tos --keep-until-expiring)
  [ -n "$EMAIL" ] && ARGS+=(-m "$EMAIL")
  certbot "${ARGS[@]}"
else
  echo "  certificate already present, leaving it alone"
fi

# 3. HTTPS block.
sed "s/__TLS_HOST__/$HOST_NAME/g" /tmp/ec-tls.conf > /etc/nginx/conf.d/event-center.conf
nginx -t
systemctl reload nginx
echo "  tls block installed and nginx reloaded"

rm -f /tmp/ec-acme.conf /tmp/ec-tls.conf
echo "  done. public entry: https://$HOST_NAME"
REMOTE

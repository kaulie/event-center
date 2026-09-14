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
# 应用的（回环）端口 —— 必须与 install.sh 使用的 EC_PORT 一致，nginx 会代理到这里。
EC_APP_PORT="${EC_APP_PORT:-9099}"
# 可选：额外的对外监听端口。留空则只监听 443（默认拓扑，已验证可用）。
# 需要绕过 443 时用，例如：EC_EDGE_PORT=9099 EC_APP_PORT=9095 ./setup-nginx.sh
EC_EDGE_PORT="${EC_EDGE_PORT:-}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=15 "$EC_HOST")
SCP=(scp -q -o BatchMode=yes)

echo "==> edge: https://$EC_IP:$EC_EDGE_PORT  (port $EC_EDGE_PORT → app 127.0.0.1:$EC_APP_PORT)"
"${SCP[@]}" "$ROOT/deploy/cloud-server/nginx/event-center-acme.conf" "$EC_HOST:/tmp/ec-acme.conf"
"${SCP[@]}" "$ROOT/deploy/cloud-server/nginx/event-center-tls.conf" "$EC_HOST:/tmp/ec-tls.conf"

"${SSH[@]}" bash -s -- "$EC_TLS_HOST" "$EC_ACME_WEBROOT" "$EC_IP" "$EC_APP_PORT" "${EC_EDGE_PORT:--}" "${EC_LE_EMAIL:--}" <<'REMOTE'
set -euo pipefail
# 参数顺序固定，可选值用 "-" 占位：ssh 会把参数用空格拼接后再交给远端 shell
# 解析，**空参数会被吞掉**，若直接传空串，后面的参数会整体前移（这里踩过）。
HOST_NAME="${1:?tls host required}"; WEBROOT="${2:?webroot required}"; IP_ADDR="${3:?ip required}"
APP_PORT="${4:?app port required}"
EDGE_PORT="${5:--}"; [ "${EDGE_PORT}" = "-" ] && EDGE_PORT=""
EMAIL="${6:--}";     [ "${EMAIL}" = "-" ] && EMAIL=""

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

# 3. HTTPS block（服务域名与字面 IP）。额外监听端口按需注入。
# 通过 ENVIRON 传值而不是 awk -v：-v 的赋值里不能含换行（会直接报错），
# 而这里的 listen 需要两行。
if [ -n "${EDGE_PORT}" ]; then
  EDGE_LISTEN="listen ${EDGE_PORT} ssl;
    listen [::]:${EDGE_PORT} ssl;"
else
  EDGE_LISTEN=""
fi
export EDGE_LISTEN
sed -e "s/__TLS_HOST__/$HOST_NAME/g" -e "s/__TLS_IP__/$IP_ADDR/g" \
    -e "s/__APP_PORT__/$APP_PORT/g" /tmp/ec-tls.conf \
  | awk '$0 ~ /^[[:space:]]*__EDGE_LISTEN__[[:space:]]*$/ { if (ENVIRON["EDGE_LISTEN"] != "") print ENVIRON["EDGE_LISTEN"]; next } { print }' \
  > /etc/nginx/conf.d/event-center.conf
nginx -t
systemctl reload nginx
if [ -n "${EDGE_PORT}" ]; then
  echo "  tls block installed (443 + :$EDGE_PORT → app 127.0.0.1:$APP_PORT) and nginx reloaded"
else
  echo "  tls block installed (443 → app 127.0.0.1:$APP_PORT) and nginx reloaded"
fi

rm -f /tmp/ec-acme.conf /tmp/ec-tls.conf
echo "  done. public entry: https://$HOST_NAME"
REMOTE

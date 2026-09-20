#!/usr/bin/env bash
#
# 启动 event-center —— 遵循部署系统规范的 runtime 脚本。
#
# 由控制面以 `restartCmd` 调用：cwd = runtimeDir，且注入
#   SERVICE_PORT = 服务契约里的服务端口（正式字段名；旧脚本兼容名 PORT 同值）
#   RUNTIME_DIR  = runtimeDir
#   APP_VERSION  = 本次部署的 8 位短 hash
#
# runtime 布局（backend/ 下的内容由平台在部署时保留，不会被 --delete 清掉）：
#   bin/eventd              可执行文件（来自发版包）
#   scripts/*.sh            本目录（来自发版包）
#   backend/.env            密钥与可选覆盖项（首次启动自动生成，权限 600）
#   backend/data/           SQLite 事件库
#   backend/ingress.jsonl   入口审计日志
#   backend/runtime.pid     进程号
#   backend/server.log      标准输出/错误
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNTIME_DIR="${RUNTIME_DIR:-$(cd "${DIR}/.." && pwd)}"
PORT="${SERVICE_PORT:-${PORT:-9099}}"   # SERVICE_PORT 是契约的正式字段名，优先它：
                                         # PORT 是通用名，CI/沙箱里常已被占用（本机预设
                                         # PORT=4211），取错值会把服务绑到别人的端口上。
APP_VERSION="${APP_VERSION:-dev}"

BIN="${RUNTIME_DIR}/bin/eventd"
BACKEND="${RUNTIME_DIR}/backend"
ENV_FILE="${BACKEND}/.env"
DATA_DIR="${BACKEND}/data"
PID_FILE="${BACKEND}/runtime.pid"
LOG_FILE="${BACKEND}/server.log"

log() { echo "[start] $*"; }
die() { echo "[start][错误] $*" >&2; exit 1; }

[ -x "${BIN}" ] || die "缺少可执行文件 ${BIN}（发版包内容不完整？）"

mkdir -p "${DATA_DIR}"

# 首次启动生成 backend/.env：只放密钥与可选覆盖项，权限 600，绝不入 git。
# 监听地址/数据库路径等由本脚本按端口与 RUNTIME_DIR 推导，避免契约换端口后
# .env 里的旧值把服务卡在旧端口上。
if [ ! -f "${ENV_FILE}" ]; then
  umask 077
  TOKEN="$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  cat > "${ENV_FILE}" <<EOF
# event-center 运行期配置（首次启动自动生成，权限 600，请勿提交到 git）
# 管理接口 token；为空则该接口不鉴权，生产环境必须保留一个强随机值。
EVENTD_ADMIN_TOKEN=${TOKEN}
# GitHub webhook 的 HMAC 密钥；留空则 github 来源不会被播种。
EVENTD_GITHUB_SECRET=
# 入口审计 JSONL 落盘路径（重启不丢、独立于 journald/日志轮转）
EVENTD_INGRESS_LOG_PATH=${BACKEND}/ingress.jsonl
# 事件保留天数，0 = 永久保留
# EVENTD_RETENTION_DAYS=0
EOF
  chmod 600 "${ENV_FILE}"
  log "已生成 ${ENV_FILE}（含新的 EVENTD_ADMIN_TOKEN）"
fi

# shellcheck disable=SC1090
set -a; . "${ENV_FILE}"; set +a

# 平台注入的值优先：端口永远跟随服务契约的 healthUrl。
export EVENTD_HTTP_ADDR="${EVENTD_BIND:-127.0.0.1}:${PORT}"
export EVENTD_DB_PATH="${EVENTD_DB_PATH:-${DATA_DIR}/eventd.db}"
: "${EVENTD_INGRESS_LOG_PATH:=${BACKEND}/ingress.jsonl}"
export EVENTD_INGRESS_LOG_PATH

# 已在运行则不重复拉起（平台重启前都会先 stop，这里是防御性检查）。
if [ -f "${PID_FILE}" ]; then
  old="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
  if [ -n "${old}" ] && kill -0 "${old}" 2>/dev/null; then
    log "已在运行 pid=${old}"
    exit 0
  fi
  rm -f "${PID_FILE}"
fi

log "启动 部署版本=${APP_VERSION} 监听=${EVENTD_HTTP_ADDR} 库=${EVENTD_DB_PATH}"
nohup "${BIN}" >> "${LOG_FILE}" 2>&1 &
echo $! > "${PID_FILE}"
pid="$(cat "${PID_FILE}")"

# 探活：/health 是平台对每个服务统一探的路径。
for _ in $(seq 1 40); do
  if ! kill -0 "${pid}" 2>/dev/null; then
    rm -f "${PID_FILE}"
    echo "[start][错误] 进程已退出，最近日志：" >&2
    tail -20 "${LOG_FILE}" >&2 || true
    exit 1
  fi
  if curl -fsS -m 2 "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    log "启动成功 pid=${pid} log=${LOG_FILE}"
    exit 0
  fi
  sleep 0.5
done

echo "[start][错误] 20s 内 /health 未就绪，最近日志：" >&2
tail -20 "${LOG_FILE}" >&2 || true
kill "${pid}" 2>/dev/null || true
rm -f "${PID_FILE}"
exit 1

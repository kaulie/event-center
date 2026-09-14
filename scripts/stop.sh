#!/usr/bin/env bash
#
# 停止 event-center —— 遵循部署系统规范的 runtime 脚本。
# 先 TERM 优雅退出（进程会关闭审计文件），超时才 KILL。
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNTIME_DIR="${RUNTIME_DIR:-$(cd "${DIR}/.." && pwd)}"
PID_FILE="${RUNTIME_DIR}/backend/runtime.pid"

log() { echo "[stop] $*"; }

if [ ! -f "${PID_FILE}" ]; then
  log "没有 pid 文件，视为未运行"
  exit 0
fi

pid="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
if [ -z "${pid}" ] || ! kill -0 "${pid}" 2>/dev/null; then
  log "pid=${pid:-<空>} 不存在，清理 pid 文件"
  rm -f "${PID_FILE}"
  exit 0
fi

log "TERM pid=${pid}"
kill -TERM "${pid}" 2>/dev/null || true
for _ in $(seq 1 30); do
  if ! kill -0 "${pid}" 2>/dev/null; then
    rm -f "${PID_FILE}"
    log "已退出"
    exit 0
  fi
  sleep 0.5
done

log "15s 未退出，KILL pid=${pid}"
kill -9 "${pid}" 2>/dev/null || true
rm -f "${PID_FILE}"
log "已强杀"

#!/usr/bin/env bash
#
# 重启 event-center —— 遵循部署系统规范的 runtime 脚本。
# 控制面默认用它作为 restartCmd（契约未显式给 restartCmd 时按此路径调用）。
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNTIME_DIR="${RUNTIME_DIR:-$(cd "${DIR}/.." && pwd)}"
export RUNTIME_DIR

"${DIR}/stop.sh" || true
"${DIR}/start.sh"

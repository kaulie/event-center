#!/usr/bin/env bash
#
# 在部署系统（agent-control-plane-deployment, :4220）里登记 event-center 的服务契约。
#
# 契约即平台运行一个服务的全部约定：runtime 目录、健康检查 URL、启停命令。
# 幂等：重复执行即更新；不改动其它服务，也不触发部署。
#
# 用法：
#   deploy/platform/register-service.sh
#   EC_SERVICE_PORT=9099 EC_RUNTIME_DIR=/Users/gaolei/runtime/event-center deploy/platform/register-service.sh
#
# 登记后即可在面板 http://localhost:4220/panel/ 里打包 / 部署，
# 或用 API：
#   curl -sS -X POST http://127.0.0.1:4220/api/deploy-notify \
#        -H 'content-type: application/json' -d '{"serviceId":"event-center"}'
#
# 注意：变量统一用 EC_ 前缀。HOST / PORT 这类通用名字在 CI/沙箱里常已被占用
# （本机就预设了 PORT=4211），直接读取会把服务登记到错误的端口上。
set -euo pipefail

EC_CONTROL_PLANE="${EC_CONTROL_PLANE:-http://127.0.0.1:4220}"
EC_SERVICE_ID="${EC_SERVICE_ID:-event-center}"
EC_RUNTIME_DIR="${EC_RUNTIME_DIR:-/Users/gaolei/runtime/${EC_SERVICE_ID}}"
EC_SERVICE_PORT="${EC_SERVICE_PORT:-9099}"
EC_GIT_REPO_URL="${EC_GIT_REPO_URL:-https://github.com/kaulie/event-center}"
EC_DEFAULT_BRANCH="${EC_DEFAULT_BRANCH:-main}"
EC_SERVICE_NAME="${EC_SERVICE_NAME:-Event Center (统一事件中心)}"

case "${EC_SERVICE_PORT}" in
  *[!0-9]*|"") echo "EC_SERVICE_PORT 必须是数字，当前：${EC_SERVICE_PORT}" >&2; exit 1 ;;
esac

echo "==> 登记服务契约 serviceId=${EC_SERVICE_ID} port=${EC_SERVICE_PORT} runtime=${EC_RUNTIME_DIR}"

curl -sS -X PUT "${EC_CONTROL_PLANE}/api/services/${EC_SERVICE_ID}" \
  -H 'content-type: application/json' \
  -d @- <<JSON
{
  "name": "${EC_SERVICE_NAME}",
  "runtimeDir": "${EC_RUNTIME_DIR}",
  "healthUrl": "http://127.0.0.1:${EC_SERVICE_PORT}/health",
  "startCmd": "bash \"${EC_RUNTIME_DIR}/scripts/start.sh\"",
  "stopCmd": "bash \"${EC_RUNTIME_DIR}/scripts/stop.sh\"",
  "restartCmd": "bash \"${EC_RUNTIME_DIR}/scripts/restart.sh\"",
  "gitRepoUrl": "${EC_GIT_REPO_URL}",
  "defaultBranch": "${EC_DEFAULT_BRANCH}"
}
JSON
echo
echo "==> 完成。面板：${EC_CONTROL_PLANE}/panel/"

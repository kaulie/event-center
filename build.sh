#!/usr/bin/env bash
#
# 打包脚本 —— 遵循「agent-control-plane-deployment」部署系统规范。
#
# 调用方（二选一，均从仓库根执行）：
#   - 控制面流水线：POST /api/deploy-notify {serviceId:"event-center"}（内部 packageFromGit）
#   - 独立发版：/Users/gaolei/deployment/bin/release.sh event-center [ref]
#
# 约定：
#   - cwd = 仓库根；环境变量 APP_VERSION = <8 位短 hash>
#   - 必须产出 outputs/，其中必须包含 scripts/restart.sh（平台硬性要求；
#     控制面 worker 会检查这个文件，缺少则判定「包不完整」）
#   - VERSION / COMMIT / GIT_REPO_URL 由调用方写入发版包，本脚本不写
#   - 运行期可变内容一律不放进 outputs/（数据库、密钥、pid、日志都在
#     backend/ 下，由平台在部署时保留，见 scripts/start.sh）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT}"

VERSION="${APP_VERSION:-dev}"
OUT="${ROOT}/outputs"

echo "[build] event-center version=${VERSION}"
rm -rf "${OUT}"
mkdir -p "${OUT}/bin" "${OUT}/scripts"

# Go 缓存/临时目录隔离在仓库**之外**（可覆盖 EC_BUILD_CACHE）。
# 控制面调用 build.sh 时会继承 deployment-server 的环境，其 HOME 可能指向
# runtime 目录；缓存若落到那里，随后 rsync --delete 无法清理（目录只读），
# 会直接破坏这次部署。放在固定路径下既能跨次复用（构建快），又不污染仓库。
BUILD_CACHE="${EC_BUILD_CACHE:-${TMPDIR:-/tmp}/event-center-build-cache}"
export GOMODCACHE="${BUILD_CACHE}/gomodcache"
export GOCACHE="${BUILD_CACHE}/gocache"
export GOPATH="${BUILD_CACHE}/gopath"
export GOTMPDIR="${BUILD_CACHE}/gotmp"
mkdir -p "${GOMODCACHE}" "${GOCACHE}" "${GOPATH}" "${GOTMPDIR}"
# 默认用可达的模块/工具链代理（部分网络下 proxy.golang.org 走 IPv6 不可达）。
[ -n "${GOPROXY:-}" ] || export GOPROXY="https://goproxy.cn,direct"

LDFLAGS="-s -w -X main.version=${VERSION}"

# 本机二进制：平台 runtime 直接运行它（packageFromGit 在控制面所在机器构建）。
CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" \
  -o "${OUT}/bin/eventd" ./cmd/eventd

# 交叉编译 linux/amd64 只在部署到远端（cloud-server / 容器）时需要，
# 默认不打进平台制品（平台在本机跑，用不上，白白让制品翻倍）：
#   EC_BUILD_LINUX=1 ./build.sh
if [ "${EC_BUILD_LINUX:-0}" = "1" ]; then
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "${LDFLAGS}" \
    -o "${OUT}/bin/eventd-linux-amd64" ./cmd/eventd
fi

# 平台通过 contracts 的 startCmd/stopCmd/restartCmd 调用这三个脚本。
cp "${ROOT}/scripts/start.sh" "${ROOT}/scripts/stop.sh" "${ROOT}/scripts/restart.sh" \
  "${OUT}/scripts/"

chmod +x "${OUT}/bin/"* "${OUT}/scripts/"*.sh

echo "[build] outputs 就绪："
ls -1 "${OUT}" "${OUT}/bin" "${OUT}/scripts" | sed 's/^/  /'

# --- 服务契约自动登记（幂等）---------------------------------------------------
# 契约的真源是代码里的 swag 注解（cmd/eventd/main.go 的 General API Info + 各 handler
# 的 @Summary/@Tags/@Router）。这一步读注解 → swag init 生成 api/swagger.json → 上报
# 注册中心（client/ 是注册中心脚本的原样拷贝，见 client/README.md）。
#
# 幂等：规范没变化时 register.sh 会跳过 PUT，不刷 revision；实例集合按声明式对齐。
# 刻意**不参与打包成败**：注册中心默认只绑本机 127.0.0.1:4240，拿它的可达性挡发布
# 没有意义，所以失败只告警。要跳过：REGISTER_CONTRACT=0；换端口/部门/实例直接给
# INSTANCES / DEPARTMENT_ID / SERVICE_NAME 环境变量。
#
# REGISTRY_URL 必须显式给：client 脚本的默认值是 http://127.0.0.1:${SERVICE_PORT}，
# 而 SERVICE_PORT 在 CI/沙箱里常被"当前服务的端口"占用（本机就预设了 4211），
# 不写死就会把契约 PUT 到别的服务上。
if [ "${REGISTER_CONTRACT:-1}" = "1" ]; then
  echo "[build] 按注解登记服务契约（可 REGISTER_CONTRACT=0 跳过）"
  SERVICE_NAME="${SERVICE_NAME:-event-center}" \
  REGISTRY_URL="${REGISTRY_URL:-http://127.0.0.1:4240}" \
  SWAG_MAIN="${SWAG_MAIN:-cmd/eventd/main.go}" \
  SWAG_OUT="${SWAG_OUT:-api}" \
  SWAG_ARGS="${SWAG_ARGS:---parseInternal --outputTypes json}" \
  DEPARTMENT_ID="${DEPARTMENT_ID:-D0002}" \
  INSTANCES="${INSTANCES:-127.0.0.1:${EC_SERVICE_PORT:-9099}}" \
  OWNER="${OWNER:-kaulie}" \
  HEALTH_PATH="${HEALTH_PATH:-/health}" \
  VERSION="${VERSION}" \
    bash client/ci/register-go-service.sh \
    || echo "[build][warn] 契约登记失败（不影响打包）：注册中心不可达或缺 swag" >&2
fi

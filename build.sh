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

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

# --- 构建缓存：稳定目录优先，构建前体检，坏了就自愈 ----------------------------
# 缓存隔离在仓库**之外**（可用 EC_BUILD_CACHE 覆盖）：不污染仓库，也不会被部署时对
# runtime 目录的 rsync --delete 牵连（模块缓存里解包出来的目录是只读的，落到被同步的
# 目录里会让 rsync 清不掉）。
#
# 默认**不再**放 TMPDIR：TMPDIR 下的文件会被系统回收（只留下目录骨架），而 go 判断
# 「工具链/模块是否已下载」只看目录在不在 —— 2026-09-20 那次部署事故就是整个
# event-center 缓存被清成了空目录骨架，于是 go 直接报
#   go: download go1.25.0: stat .../gomodcache/golang.org/toolchain@.../bin/go: no such file or directory
# 而不是重新解包，打包次次失败。所以默认放用户缓存目录，$HOME 不可用时才退回 TMPDIR：
#   - $HOME 里同时有 DEPLOYMENT、VERSION 两个**文件**，说明它其实是某个服务的部署目录
#     （runtime/<service>/ 下就是这两份标记），缓存不能落进去；这里用 -f 而不是 -e：
#     macOS 默认大小写不敏感，`-e $HOME/DEPLOYMENT` 会误命中用户自己的 ~/deployment 目录。
#   - 目录本身写不进去（只读 HOME）就继续找下一个候选。
# 另外每次构建前体检、失败后重试，见 heal_build_cache。
is_deployment_home() {
  [ -f "$1/DEPLOYMENT" ] && [ -f "$1/VERSION" ]
}

is_writable_dir() {
  mkdir -p "$1" 2>/dev/null || return 1
  : >"$1/.write-probe.$$" 2>/dev/null || return 1
  rm -f "$1/.write-probe.$$"
}

pick_build_cache() {
  local candidates=() d
  if [ -n "${EC_BUILD_CACHE:-}" ]; then
    candidates+=("${EC_BUILD_CACHE}")
  fi
  if [ -n "${HOME:-}" ] && ! is_deployment_home "${HOME}"; then
    candidates+=("${XDG_CACHE_HOME:-${HOME}/.cache}/event-center-build-cache")
  fi
  candidates+=("${TMPDIR:-/tmp}/event-center-build-cache")
  for d in "${candidates[@]}"; do
    if is_writable_dir "${d}"; then
      printf '%s' "${d}"
      return 0
    fi
  done
  return 1
}

BUILD_CACHE="$(pick_build_cache)" || {
  echo "[build][错误] 没有可写的构建缓存目录（EC_BUILD_CACHE / \$HOME/.cache / TMPDIR 都不可写）" >&2
  exit 1
}
echo "[build] 构建缓存：${BUILD_CACHE}（可用 EC_BUILD_CACHE 覆盖）"
export GOMODCACHE="${BUILD_CACHE}/gomodcache"
export GOCACHE="${BUILD_CACHE}/gocache"
export GOPATH="${BUILD_CACHE}/gopath"
export GOTMPDIR="${BUILD_CACHE}/gotmp"
mkdir -p "${GOMODCACHE}" "${GOCACHE}" "${GOPATH}" "${GOTMPDIR}"
# 默认用可达的模块/工具链代理（部分网络下 proxy.golang.org 走 IPv6 不可达）。
[ -n "${GOPROXY:-}" ] || export GOPROXY="https://goproxy.cn,direct"

# 解包出来的模块目录是只读的（dr-xr-xr-x）：删它里面的文件先要把自己和子目录补上写权限，
# 删它本身则要求父目录可写 —— 历史上父目录（golang.org/、github.com/）也可能是只读的，
# 所以两级都补一下，代价可以忽略。
rm_cache_ro() {
  chmod -R u+w "$1" 2>/dev/null || true
  chmod u+w "$(dirname "$1")" 2>/dev/null || true
  if ! rm -rf "$1"; then
    echo "[build][warn] 删不掉缓存目录（权限？）：$1" >&2
  fi
}

# 丢掉解包产物，保留 cache/download 里的 zip —— 重新解包是离线的，不必再下载。
reset_extracted_cache() {
  local d
  for d in "${GOMODCACHE}"/*; do
    [ -e "${d}" ] || continue
    case "${d}" in */cache) continue ;; esac
    rm_cache_ro "${d}"
  done
}

# 体检：go 不会自查缓存完整性，坏缓存必须由我们识别并丢掉，否则打包会一直失败。
heal_build_cache() {
  local tc d
  # 工具链只解包了一半（bin/go 不在）时，go 会 stat 失败直接退出而不是重下，直接丢掉。
  for tc in "${GOMODCACHE}"/golang.org/toolchain@*; do
    [ -d "${tc}" ] || continue
    if [ ! -x "${tc}/bin/go" ]; then
      echo "[build][warn] 丢弃不完整的 Go 工具链缓存：${tc}" >&2
      rm_cache_ro "${tc}"
    fi
  done
  # 模块目录里一个文件都没有 = 被清空成空壳，但 go 仍当作已下载 → 打包报
  # 「no required module provides package ...」。整块删掉，让 go 重新解包。
  while IFS= read -r d; do
    [ -n "${d}" ] || continue
    if [ -z "$(find "${d}" -type f -print -quit 2>/dev/null || true)" ]; then
      echo "[build][warn] 丢弃空壳缓存目录（一个文件都没有）：${d}" >&2
      rm_cache_ro "${d}"
    fi
  done < <(find "${GOMODCACHE}" -mindepth 1 -maxdepth 4 -type d -name '*@*' \
             -not -path "${GOMODCACHE}/cache/*" 2>/dev/null)
}

LDFLAGS="-s -w -X main.version=${VERSION}"

# 本机二进制：平台 runtime 直接运行它（packageFromGit 在控制面所在机器构建）。
go_build() {
  CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" \
    -o "${OUT}/bin/eventd" ./cmd/eventd
}

heal_build_cache

BUILD_LOG="${BUILD_CACHE}/build.log"
# 兜底：首次构建失败、且报错指向缓存不完整（zip 损坏、只解包了一半……）时，清掉解包
# 产物与构建缓存重试一次；zip 还在的话这一步是离线的。要看首次报错就别重试：
# SKIP_CACHE_RETRY=1。
CACHE_FAIL_RE='no such file or directory|no required module provides package|missing go.sum entry|checksum mismatch|not a valid zip|corrupt|unexpected EOF|does not contain package'
if ! go_build 2>&1 | tee "${BUILD_LOG}"; then
  status="${PIPESTATUS[0]}"
  if [ -n "${SKIP_CACHE_RETRY:-}" ] || ! grep -Eq "${CACHE_FAIL_RE}" "${BUILD_LOG}"; then
    echo "[build][错误] go build 失败（exit ${status}）" >&2
    exit "${status}"
  fi
  echo "[build][warn] 构建失败且报错指向构建缓存不完整；清掉缓存后重试一次" >&2
  reset_extracted_cache
  rm_cache_ro "${GOCACHE}"
  mkdir -p "${GOCACHE}"
  go_build
fi

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

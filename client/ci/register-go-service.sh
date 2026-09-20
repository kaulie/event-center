#!/usr/bin/env bash
#
# Go 服务专用：**读代码 → 生成 OpenAPI → 登记到注册中心** 一条命令（给 CI / build.sh / 部署后钩子调用）。
#
# 为什么这么做：Go 没有注解，等价物是 swaggo/swag 的**注释注解** ——
# 注解写在 handler 上（`@Router` / `@Summary` / `@Param` …），swag 从 AST 里读出来生成
# docs/swagger.json（Swagger 2.0；本中心 OpenAPI 3.x 与 Swagger 2.0 **都收**）。
# 于是注解是唯一真源：接口改了注解跟着改，CI 跑一次契约就刷新，没人需要手工维护规范文件。
#
# 用法（在**服务仓库根目录**执行，配置全走环境变量）：
#   SERVICE_NAME=event-center \
#   REGISTRY_NS=team-a REGISTRY_TOKEN=$NS_TOKEN \
#   DEPARTMENT_ID=D0005 INSTANCES=127.0.0.1:9099 \
#   OWNER=kaulie HEALTH_PATH=/health \
#   bash client/ci/register-go-service.sh
#
# 环境变量（★ 必填 / ☆ 可选）：
#   ★ SERVICE_NAME  注册用的服务名（小写字母/数字/._-）
#   ☆ SPEC_FILE     已有规范文件：给了就**跳过 swag init**（适合"规范在仓库里"的服务）
#   ☆ SWAG_MAIN     swag init -g 的入口；默认自动找 ./cmd/<service>/main.go → ./main.go
#   ☆ SWAG_OUT      swag 输出目录（默认 ./docs，产物 docs/swagger.json）
#   ☆ SWAG_ARGS     额外参数（如 "--parseDependency --parseInternal"）
#   ☆ SKIP_SWAG_INSTALL=1   已装好 swag 时不要联网安装
#   ☆ VERSION / OWNER / DESCRIPTION / TAGS / HEALTH_PATH / GIT_REPO
#   ☆ REGISTRY_URL / REGISTRY_NS / REGISTRY_TOKEN
#   ☆ DEPARTMENT_ID、REGISTRY_DEPARTMENT_ID 或 REGISTRY_DEPARTMENT_NAME（归属部门）
#   ☆ INSTANCES     实例地址，逗号分隔（如 127.0.0.1:9099,10.0.0.7:9099）
#   ☆ REGISTRY_SCRIPT  register.sh 路径（默认取与本脚本同级的 ../register.sh）
set -euo pipefail

die() { echo "[错误] $*" >&2; exit 1; }

SERVICE_NAME="${SERVICE_NAME:-}"
[ -n "${SERVICE_NAME}" ] || die "缺少 SERVICE_NAME（注册用的服务名）"

DEPARTMENT_ID="${DEPARTMENT_ID:-${REGISTRY_DEPARTMENT_ID:-}}"
SWAG_OUT="${SWAG_OUT:-docs}"
SWAG_ARGS="${SWAG_ARGS:-}"
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REGISTRY_SCRIPT="${REGISTRY_SCRIPT:-${SELF_DIR}/../register.sh}"
[ -f "${REGISTRY_SCRIPT}" ] || die "找不到 register.sh：${REGISTRY_SCRIPT}（可用 REGISTRY_SCRIPT 指定）"

# 版本号没给就取当前 commit（CI 里通常就是这次要登记的那个版本）。
if [ -z "${VERSION:-}" ]; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || true)"
fi

if [ -z "${SPEC_FILE:-}" ]; then
  command -v python3 >/dev/null 2>&1 || die "需要 python3"

  # 1) 找入口：swag 要从这里读 General API Info（@title/@version/@BasePath…）。
  MAIN="${SWAG_MAIN:-}"
  if [ -z "${MAIN}" ]; then
    for c in "cmd/${SERVICE_NAME}/main.go" "cmd/$(echo "${SERVICE_NAME}" | tr '-' '_')/main.go" "main.go"; do
      [ -f "${c}" ] && MAIN="${c}" && break
    done
  fi
  [ -n "${MAIN}" ] || die "找不到入口 main.go（用 SWAG_MAIN 指定，如 SWAG_MAIN=cmd/foo/main.go）"

  # 2) swag：从代码里的注解生成规范。
  if ! command -v swag >/dev/null 2>&1; then
    [ "${SKIP_SWAG_INSTALL:-0}" = "1" ] && die "环境里没有 swag，且 SKIP_SWAG_INSTALL=1"
    echo "==> 安装 swaggo/swag（首次）"
    go install github.com/swaggo/swag/cmd/swag@latest
    PATH="$(go env GOPATH)/bin:${PATH}"; export PATH
  fi
  echo "==> swag init -g ${MAIN} -o ${SWAG_OUT} ${SWAG_ARGS# }"
  # shellcheck disable=SC2086  # SWAG_ARGS 有意按空格分词
  swag init -g "${MAIN}" -o "${SWAG_OUT}" ${SWAG_ARGS}
  SPEC_FILE="${SWAG_OUT}/swagger.json"
  [ -f "${SPEC_FILE}" ] || die "swag 没有产出 ${SPEC_FILE}"

  paths="$(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1])).get("paths") or {}))' "${SPEC_FILE}")"
  echo "    从注解生成 ${SPEC_FILE}：${paths} 个路径"
  [ "${paths}" -gt 0 ] || die "生成出来的 paths 是空的：注解没被 swag 认出来（检查 main.go 顶部的 General API Info，以及 handler 上的 @Router/@Summary）"
else
  [ -f "${SPEC_FILE}" ] || die "找不到 SPEC_FILE：${SPEC_FILE}"
  echo "==> 用现成规范 ${SPEC_FILE}（跳过 swag init）"
fi

# 3) 交给 register.sh 登记：幂等（契约没变化时不写库），实例集合按声明式对齐。
ARGS=(--service "${SERVICE_NAME}" --file "${SPEC_FILE}")
[ -n "${VERSION:-}" ]         && ARGS+=(--version "${VERSION}")
[ -n "${OWNER:-}" ]           && ARGS+=(--owner "${OWNER}")
[ -n "${DESCRIPTION:-}" ]     && ARGS+=(--description "${DESCRIPTION}")
[ -n "${HEALTH_PATH:-}" ]     && ARGS+=(--health-path "${HEALTH_PATH}")
[ -n "${GIT_REPO:-}" ]        && ARGS+=(--git-repo "${GIT_REPO}")
[ -n "${DEPARTMENT_ID}" ]     && ARGS+=(--department "${DEPARTMENT_ID}")
[ -n "${DEPARTMENT_NAME:-}" ] && ARGS+=(--department-name "${DEPARTMENT_NAME}")
[ -n "${TAGS:-}" ]            && for t in $(echo "${TAGS}" | tr ',' ' '); do ARGS+=(--tag "${t}"); done
[ -n "${INSTANCES:-}" ]       && for i in $(echo "${INSTANCES}" | tr ',' ' '); do ARGS+=(--instance "${i}"); done

echo "==> 登记 ${SERVICE_NAME}（规范 ${SPEC_FILE}）"
exec bash "${REGISTRY_SCRIPT}" ${ARGS[@]+"${ARGS[@]}"}

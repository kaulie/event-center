#!/usr/bin/env bash
#
# 一次性注册脚本：把一个服务的**契约（含对外 API）与实例集合**登记进注册中心。
#
# 关键语义：这是**一次性调用**，调用完就退出 ——
# 它不是守护进程，不维持心跳，服务运行时也不需要和注册中心有任何连接。
# 改了契约或实例集合，再跑一次即可（契约按服务名幂等覆盖，实例集合按声明式整组对齐）。
#
# 用法：
#   client/register.sh --service event-center --file api/openapi.yaml \
#       --instance 127.0.0.1:9099 --instance 10.0.0.7:9099 \
#       --owner kaulie --version 1.4.2 --tag events
#
#   # 带上代码仓库地址（不传则默认取本地 git 的 remote.origin.url）
#   client/register.sh --service event-center --file api/openapi.yaml \
#       --git-repo https://github.com/kaulie/event-center.git
#
#   # 带上归属部门（部门数据的权威在组织接口，服务端会按目录把 ID/名称对齐补全）
#   client/register.sh --service event-center --file api/openapi.yaml \
#       --department D0001                 # 或用 --department-name SRE部门 / --department
#
#   # 只登记契约 / 只登记实例：
#   client/register.sh --service event-center --file api/openapi.yaml
#   client/register.sh --service event-center --instance 127.0.0.1:9099 --no-contract
#
#   # 服务自己暴露了 /openapi.json（FastAPI、@fastify/swagger、swag 起的 docs 接口等）：
#   # **由本脚本把规范取回来**再内联登记 —— 注册中心本身永远不出网（不抓 specUrl）。
#   client/register.sh --service event-center --spec-url http://127.0.0.1:9099/openapi.json
#
#   # 幂等：契约的 specHash 与库里一致时**跳过 PUT**（不产生无意义的 revision），
#   # 实例集合仍按声明式对齐。想无条件覆盖用 --force。
#
#   # 用 CI 的命名空间令牌（写接口需要令牌；本机未配置 admin 令牌时可不传）
#   REGISTRY_TOKEN=rt_xxx client/register.sh --service foo --file api/openapi.yaml
#
# 环境变量：
#   REGISTRY_URL      注册中心地址（默认 http://127.0.0.1:${SERVICE_PORT:-4240}）
#   REGISTRY_TOKEN    写令牌（admin token 或该命名空间的 token）
#   REGISTRY_NS       命名空间（默认 default）
#   REGISTRY_GIT_REPO 代码仓库地址（等价于 --git-repo；都不给时取 remote.origin.url）
#   REGISTRY_DEPARTMENT_ID    归属部门 ID（等价于 --department/--department-id）
#   REGISTRY_DEPARTMENT_NAME  归属部门名（等价于 --department-name）
#   SERVICE_PORT      注册中心监听端口（与服务端同一个变量；用于推导默认 URL）
set -euo pipefail

REGISTRY_URL="${REGISTRY_URL:-http://127.0.0.1:${SERVICE_PORT:-4240}}"
REGISTRY_TOKEN="${REGISTRY_TOKEN:-}"
NS="${REGISTRY_NS:-default}"

SERVICE=""
SPEC_FILE=""
SPEC_URL=""
FORCE=0
VERSION=""
OWNER=""
DESCRIPTION=""
HEALTH_PATH=""
GIT_REPO="${REGISTRY_GIT_REPO:-}"
NO_GIT_REPO=0
# 归属部门（可选）：ID 或名称给一个即可，服务端会拿组织接口的部门目录把另一个补全。
DEPARTMENT_ID="${REGISTRY_DEPARTMENT_ID:-}"
DEPARTMENT_NAME="${REGISTRY_DEPARTMENT_NAME:-}"
SCHEME="http"
INSTANCES=()
TAGS=()
NO_CONTRACT=0
NO_INSTANCES=0

usage() {
  # 打印文件头部的用法注释（到 `set -euo pipefail` 为止；改注释不用改这里）
  awk 'NR>1 && /^set -euo pipefail/ {exit} NR>1 {print}' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --service)      SERVICE="${2:?}"; shift 2 ;;
    --file)         SPEC_FILE="${2:?}"; shift 2 ;;
    --spec-url)     SPEC_URL="${2:?}"; shift 2 ;;
    --force)        FORCE=1; shift ;;
    --instance)     INSTANCES+=("${2:?}"); shift 2 ;;
    --tag)          TAGS+=("${2:?}"); shift 2 ;;
    --version)      VERSION="${2:?}"; shift 2 ;;
    --owner)        OWNER="${2:?}"; shift 2 ;;
    --description)  DESCRIPTION="${2:?}"; shift 2 ;;
    --health-path)  HEALTH_PATH="${2:?}"; shift 2 ;;
    --git-repo)     GIT_REPO="${2:?}"; shift 2 ;;
    --no-git-repo)  NO_GIT_REPO=1; shift ;;
    --department)      DEPARTMENT_ID="${2:?}"; shift 2 ;;
    --department-id)   DEPARTMENT_ID="${2:?}"; shift 2 ;;
    --department-name) DEPARTMENT_NAME="${2:?}"; shift 2 ;;
    --scheme)       SCHEME="${2:?}"; shift 2 ;;
    --ns)           NS="${2:?}"; shift 2 ;;
    --url)          REGISTRY_URL="${2:?}"; shift 2 ;;
    --token)        REGISTRY_TOKEN="${2:?}"; shift 2 ;;
    --no-contract)  NO_CONTRACT=1; shift ;;
    --no-instances) NO_INSTANCES=1; shift ;;
    -h|--help)      usage 0 ;;
    *) echo "未知参数：$1" >&2; usage 1 ;;
  esac
done

[ -n "${SERVICE}" ] || { echo "缺少 --service" >&2; exit 1; }
if [ "${NO_CONTRACT}" -eq 0 ] && [ -z "${SPEC_FILE}" ] && [ -z "${SPEC_URL}" ]; then
  echo "缺少 --file（内联 OpenAPI 文件）或 --spec-url（服务自己暴露的规范地址）；若只想登记实例，请加 --no-contract" >&2
  exit 1
fi
command -v python3 >/dev/null 2>&1 || { echo "需要 python3 来安全地拼 JSON" >&2; exit 1; }

# 仓库地址：显式参数/环境变量优先；没给就从当前 git 仓库的 remote.origin.url 推断
# （CI 里通常就是它自己，省一次手填），并打印出来便于核对；--no-git-repo 可关闭推断。
if [ "${NO_CONTRACT}" -eq 0 ] && [ -z "${GIT_REPO}" ] && [ "${NO_GIT_REPO}" -eq 0 ]; then
  INFERRED="$(git config --get remote.origin.url 2>/dev/null || true)"
  if [ -n "${INFERRED}" ]; then
    GIT_REPO="${INFERRED}"
    echo "==> gitRepoUrl 取自本地 remote.origin.url：${GIT_REPO}"
    echo "    （覆盖用 --git-repo，想留空用 --no-git-repo）"
  fi
fi

AUTH=()
[ -n "${REGISTRY_TOKEN}" ] && AUTH=(-H "Authorization: Bearer ${REGISTRY_TOKEN}")
# 注意必须写成 ${AUTH[@]+"${AUTH[@]}"}：bash 3.2（macOS 默认）在 `set -u` 下对空数组用
# "${AUTH[@]}" 会以 "unbound variable" 直接退出 —— 表现就是"不带令牌必崩"。
# 展开为空时这条命令等于没传这个 header，语义不变。

service_url="${REGISTRY_URL}/v1/namespaces/${NS}/services/${SERVICE}"

# ---- 1) 契约（含对外 API）----
if [ "${NO_CONTRACT}" -eq 0 ]; then
  # --spec-url：由**调用方**把服务暴露的规范取回来（注册中心自己永远不出网）。
  if [ -n "${SPEC_URL}" ]; then
    SPEC_TMP="$(mktemp "${TMPDIR:-/tmp}/registry-spec.XXXXXX")"
    trap 'rm -f "${SPEC_TMP}"' EXIT
    echo "==> 从 ${SPEC_URL} 取回规范（本脚本取，注册中心只收）"
    if ! curl -fsSL --max-time 30 "${SPEC_URL}" -o "${SPEC_TMP}"; then
      echo "[错误] 取规范失败：${SPEC_URL}" >&2
      exit 1
    fi
    SPEC_FILE="${SPEC_TMP}"
    echo "    已取回 $(wc -c <"${SPEC_FILE}" | tr -d ' ') 字节"
  fi
  [ -f "${SPEC_FILE}" ] || { echo "找不到规范文件：${SPEC_FILE}" >&2; exit 1; }

  # 幂等：先比"原文 sha256"与服务端存的 specHash（同一个文件 → 同一个值），
  # 一致就跳过 PUT —— 否则每次 CI/部署都往变更表里写一条没有意义的 revision。
  local_hash="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "${SPEC_FILE}")"
  current_hash="$(curl -sS --max-time 10 "${service_url}" 2>/dev/null | python3 -c \
    'import json,sys; d=json.load(sys.stdin); print(((d.get("service") or {}).get("api") or {}).get("specHash",""))' \
    2>/dev/null || true)"
  put=1
  if [ "${FORCE}" -eq 0 ] && [ -n "${current_hash}" ] && [ "${current_hash}" = "${local_hash}" ]; then
    echo "==> 契约无变化（specHash ${local_hash:0:8} 与现有一致）：跳过登记（要强制覆盖加 --force）"
    put=0
  fi
  if [ "${put}" -eq 1 ]; then
  payload="$(SERVICE="${SERVICE}" SPEC_FILE="${SPEC_FILE}" VERSION="${VERSION}" OWNER="${OWNER}" \
             DESCRIPTION="${DESCRIPTION}" HEALTH_PATH="${HEALTH_PATH}" GIT_REPO="${GIT_REPO}" \
             DEPARTMENT_ID="${DEPARTMENT_ID}" DEPARTMENT_NAME="${DEPARTMENT_NAME}" \
             TAGS="$(IFS=,; echo "${TAGS[*]:-}")" python3 - <<'PY'
import json, os

def build():
    spec = open(os.environ["SPEC_FILE"], encoding="utf-8").read()
    body = {
        "version": os.environ["VERSION"],
        "owner": os.environ["OWNER"],
        "description": os.environ["DESCRIPTION"],
        "basePath": "/",
        "api": {"protocols": ["http"], "spec": spec},
    }
    if os.environ["HEALTH_PATH"]:
        body["healthPath"] = os.environ["HEALTH_PATH"]
    # 注意只在非空时带上：PUT 是整份契约覆盖，带上空值会把已有仓库地址清掉。
    if os.environ.get("GIT_REPO"):
        body["gitRepoUrl"] = os.environ["GIT_REPO"]
    # 归属部门：同样只在非空时带上（给 ID 或名称都行，服务端按组织接口的目录对齐）。
    if os.environ.get("DEPARTMENT_ID"):
        body["departmentId"] = os.environ["DEPARTMENT_ID"]
    if os.environ.get("DEPARTMENT_NAME"):
        body["departmentName"] = os.environ["DEPARTMENT_NAME"]
    tags = [t for t in os.environ.get("TAGS", "").split(",") if t]
    if tags:
        body["tags"] = tags
    print(json.dumps(body))

build()
PY
)"

  echo "==> 登记契约 ${NS}/${SERVICE}（内联规范 ${SPEC_FILE}）"
  code="$(curl -sS -o /tmp/registry-register-out.json -w '%{http_code}' \
    -X PUT "${service_url}" \
    -H 'content-type: application/json' ${AUTH[@]+"${AUTH[@]}"} \
    -d "${payload}")"
  if [ "${code}" != "200" ] && [ "${code}" != "201" ]; then
    echo "[错误] 契约登记失败（HTTP ${code}）：" >&2
    cat /tmp/registry-register-out.json >&2; echo >&2
    exit 1
  fi
  python3 -c 'import json,sys; d=json.load(open("/tmp/registry-register-out.json")); \
s=d["service"]; print("    端点 %d 个，spec=%s，revision=%s" % (len(s["api"]["endpoints"]), \
(s["api"].get("specHash","") or "")[:8], s.get("revision"))); \
note=d.get("departmentNote"); print("    部门：%s%s" % ((s.get("departmentName") or s.get("departmentId") or "(未登记)"), \
(" —— " + note) if note else ""))'
  fi
fi

# ---- 2) 实例集合（声明式整组对齐）----
if [ "${NO_INSTANCES}" -eq 0 ]; then
  if [ "${#INSTANCES[@]}" -eq 0 ]; then
    echo "==> 未提供 --instance：跳过实例登记（若要清空实例集合，请显式传 --instance-set-empty）"
  else
    payload="$(SCHEME="${SCHEME}" INSTANCES="$(IFS=,; echo "${INSTANCES[*]}")" python3 - <<'PY'
import json, os

items = []
for raw in os.environ["INSTANCES"].split(","):
    raw = raw.strip()
    if not raw:
        continue
    if ":" in raw:
        host, port = raw.rsplit(":", 1)
    else:
        host, port = raw, "80"
    items.append({"scheme": os.environ["SCHEME"], "host": host, "port": int(port)})
print(json.dumps({"instances": items}))
PY
)"
    echo "==> 声明式同步实例集合（${#INSTANCES[@]} 个）"
    code="$(curl -sS -o /tmp/registry-register-inst.json -w '%{http_code}' \
      -X PUT "${service_url}/instances" \
      -H 'content-type: application/json' ${AUTH[@]+"${AUTH[@]}"} \
      -d "${payload}")"
    if [ "${code}" != "200" ]; then
      echo "[错误] 实例同步失败（HTTP ${code}）：" >&2
      cat /tmp/registry-register-inst.json >&2; echo >&2
      exit 1
    fi
    python3 -c 'import json; d=json.load(open("/tmp/registry-register-inst.json")); \
print("    created=%d updated=%d deleted=%d unchanged=%d" % (d["created"], d["updated"], d["deleted"], d["unchanged"]))'
  fi
fi

echo "==> 完成。"
echo "    服务详情：${REGISTRY_URL}/v1/namespaces/${NS}/services/${SERVICE}"
echo "    控制面板：${REGISTRY_URL}/panel/"

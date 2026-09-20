# event-center

统一事件中心：接收外部事件源（GitHub / K8s / 告警 / 任意系统）的事件注入，
持久化为一条带时序编号的事件日志，并以 pub/sub 方式分发给下游。

- **注入**：对外暴露 Webhook（`/github-events-ingress`）与通用注入接口（`/v1/ingest/{source}`）
- **持久化**：所有事件落库（SQLite + WAL），可查询、可回放
- **类型化**：每个事件带 `provider`（生产者）+ `type`（如 `github.pull_request.opened`）
- **时序递增**：全局 `seq` + 流内 `stream_seq`，下游可按游标拉取或订阅推送
- **Pub/Sub**：**push webhook**（重试 + 退避 + DLQ）+ **cursor pull**（long-poll）

设计细节见 [`docs/DESIGN.md`](docs/DESIGN.md)。接口契约的真源是**代码里的 swag 注解**
（`cmd/eventd/main.go` 的 General API Info + 每个 handler 的 `@Summary/@Tags/@Router`），
`make swagger` 生成 [`api/swagger.json`](api/swagger.json)，没有再手工维护的规范文件。

## 快速开始

```bash
# 1. 启动（零依赖，纯 Go + SQLite 单文件）
export EVENTD_GITHUB_SECRET='your-github-webhook-secret'
go run ./cmd/eventd
# 监听 :8080，数据落在 ./data/eventd.db

# 或用 docker compose
cd deploy && docker compose up --build
```

### 注入一个事件（通用源）

```bash
# 注册一个来源（开发模式下无需 admin token；生产请设置 EVENTD_ADMIN_TOKEN）
curl -sX PUT localhost:8080/v1/sources/cicd \
  -d '{"kind":"webhook","secret":"s3cr3t","verify_mode":"bearer","default_stream":"cicd"}'

# 注入
curl -sX POST localhost:8080/v1/ingest/cicd \
  -H 'Authorization: Bearer s3cr3t' \
  -d '{"type":"deploy.finished","subject":"service:api","data":{"env":"prod"}}'
# → 201 {"status":"accepted","event":{"seq":1,"stream_seq":1,"type":"cicd.deploy.finished",...}}
```

### 注入 GitHub 事件

把 GitHub Webhook 指向 `https://event-center.115-190-153-53.sslip.io/github-events-ingress`，Secret 设为
`EVENTD_GITHUB_SECRET`。事件会被自动归一到 `github.<event>[.<action>]`，
并用 `X-GitHub-Delivery` 去重（GitHub 重投不会产生重复事件）。

### 下游一：订阅推送（push webhook）

```bash
curl -sX POST localhost:8080/v1/subscriptions -d '{
  "name": "ci-on-main-push",
  "stream": "github",
  "type_filters": ["github.push"],
  "subject_pattern": "repo:kaulie/*",
  "delivery": "push",
  "endpoint": "https://ci.example.com/hooks/event-center"
}'
# → 201，响应里带一次性可见的 "secret"（用于校验 X-EventCenter-Signature）
```

投递体：

```jsonc
{
  "subscription": "sub_01J8...",
  "delivery_id": "dlv_01J8...",
  "attempt": 1,
  "count": 2,
  "events": [ { /* 事件信封 */ } ]
}
```

验签（下游侧）：

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write(rawBody)
if s := r.Header.Get("X-EventCenter-Signature"); s != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
    return errors.New("bad signature")
}
```

### 下游二：按游标拉取

```bash
curl -s 'localhost:8080/v1/streams/github/events?after=0&limit=100'
# → {"events":[...],"next_cursor":100,"has_more":true}

# long-poll：挂起等新事件（最多 30s），有新事件立即返回
curl -s 'localhost:8080/v1/streams/github/events?after=100&wait=30s'

# 提交位点（订阅自己的 key）
curl -sX POST localhost:8080/v1/subscriptions/sub_01J8.../ack \
  -H "X-API-Key: $SUB_SECRET" -d '{"cursor":100}'
```

`after` 语义：具体流按 `stream_seq` 比较；`/v1/streams/all/events` 按全局 `seq` 比较。

## API

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| POST | `/github-events-ingress` | GitHub 注入（HMAC 校验，按 delivery id 去重）—— 唯一入口 | source secret |
| POST | `/v1/ingest/{source}` | 通用注入（信封体） | source secret |
| GET | `/v1/streams` | 流列表 + 全局 seq | admin / api key |
| GET | `/v1/streams/{stream}/events` | 拉取事件（`after`/`limit`/`wait`） | admin / api key |
| GET | `/v1/events/{id}` | 按 id 查事件 | admin / api key |
| POST | `/v1/subscriptions/{id}/ack` | 提交消费位点（只前进） | 订阅 key / admin |
| GET/PUT/DELETE | `/v1/sources[/{id}]` | 来源管理 | admin |
| GET/POST/DELETE | `/v1/subscriptions[/{id}]` | 订阅管理 | admin |
| POST | `/v1/subscriptions/{id}/pause`、`/resume` | 暂停/恢复投递 | admin |
| GET | `/v1/deliveries` | 投递记录（含 DLQ：`?status=dead`） | admin |
| POST | `/v1/deliveries/requeue` | 重投 DLQ | admin |
| GET | `/health` `/healthz` `/readyz` `/metrics` | 运维 | 无 |

## 配置

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `EVENTD_HTTP_ADDR` | （未设置） | 监听地址（`host:port`）；优先级最高 |
| `SERVICE_PORT` | （未设置） | 启动端口（仅端口，或 `host:port`）；部署控制面按服务契约注入。`EVENTD_HTTP_ADDR` 未设置时生效，两者都没有才用默认 `:8080` |
| `EVENTD_DB_PATH` | `./data/eventd.db` | SQLite 路径（`:memory:` 用于测试） |
| `EVENTD_ADMIN_TOKEN` | 空 | 管理接口 token；为空时不校验（仅开发） |
| `EVENTD_GITHUB_SECRET` | 空 | 首次启动时据此播种 `github` 来源 |
| `EVENTD_RETENTION_DAYS` | `0` | >0 时每日清理超期事件；0=永久保留 |
| `EVENTD_INGRESS_LOG_PATH` | 空 | 入口审计 JSONL 落盘路径（空=只进 journald） |
| `EVENTD_INGRESS_LOG_BODY` | `false` | 是否连**成功**请求的原始 body 也记入日志 |
| `EVENTD_INGRESS_LOG_BODY_MAX` | `8192` | 记录 body 时的截断长度 |
| `EVENTD_PULL_DEFAULT_LIMIT` / `EVENTD_PULL_MAX_LIMIT` | `100` / `1000` | 拉取分页 |
| `EVENTD_PULL_WAIT_MAX` | `30s` | long-poll 上限 |
| `EVENTD_PUSH_ENABLED` / `EVENTD_DISPATCHER_DISABLED` | `true` / `false` | 推送开关 |
| `EVENTD_PUSH_BATCH_SIZE` | `50` | 每批投递事件数 |
| `EVENTD_PUSH_MAX_ATTEMPTS` | `6` | 超过则进 DLQ |
| `EVENTD_PUSH_BASE_BACKOFF` / `EVENTD_PUSH_MAX_BACKOFF` | `2s` / `10m` | 重试退避 |
| `EVENTD_PUSH_TIMEOUT` | `10s` | 单次投递超时 |
| `EVENTD_PUSH_POLL_INTERVAL` | `1s` | 队列轮询间隔 |

监听地址的取值顺序是 `EVENTD_HTTP_ADDR` → `SERVICE_PORT` → 内置默认 `:8080`，
只有一个生效，启动日志里的 `addr_source` 会写明用的是哪一个。`SERVICE_PORT` 只给端口
（`9099`）时主机名留空，进程绑定该端口的所有网卡；要限定回环，用 `EVENTD_HTTP_ADDR`
写全 `host:port`（`scripts/start.sh` 就是这么做的：`127.0.0.1:${SERVICE_PORT}`）。
变量设了但不可用（非数字/越界）时**不阻止启动**：退回默认并打一条 WARN，
把被忽略的值原样写进日志——比反复重启、只看到健康检查一直失败要好排查。

## 开发

```bash
make test           # 全部测试
make race           # 竞态检测
make run            # 本地运行
make build          # 产出 bin/eventd
make lint           # gofmt 检查 + go vet
make swagger        # 按代码里的注解重新生成 api/swagger.json
make swagger-check  # 校验 api/swagger.json 与注解一致（CI 用，不一致即失败）
```

### 打包/发版（build.sh）

发版包由 `build.sh` 产出（`outputs/`，其中必须有 `outputs/scripts/restart.sh`，平台按
「agent-control-plane-deployment」规范检查）。调用方是控制面 `packageFromGit`，或本机
`/Users/gaolei/deployment/bin/release.sh event-center [ref]`：

```bash
APP_VERSION="$(git rev-parse --short=8 HEAD)" ./build.sh   # 产出 outputs/
REGISTER_CONTRACT=0 APP_VERSION=dev ./build.sh             # 只打包，不登记契约
```

Go 构建缓存放在仓库**之外**，默认 `$XDG_CACHE_HOME/event-center-build-cache`（macOS 上即
`~/.cache/event-center-build-cache`），可用 `EC_BUILD_CACHE` 覆盖。它必须落在不会被系统
清理的目录：go 判断「工具链/模块是否已下载」只看目录在不在，缓存被清空成空目录骨架时它
会直接报 `go: download go1.25.0: stat .../bin/go: no such file or directory`，而不是重新
解包 —— 打包会次次失败。因此 `build.sh` 每次都会先体检缓存（丢掉没解包完整的工具链和
空壳模块目录），并在报错指向缓存不完整时自动清掉解包产物重试一次
（`SKIP_CACHE_RETRY=1` 可关掉重试）。首次打包（缓存为空）需要下载工具链与模块，之后是增量。

## 服务契约：注解即真源

服务契约（对外 API + 元信息）**不手工维护规范文件**，而是写在代码里，用
[swaggo/swag](https://github.com/swaggo/swag) 的注解表达；注解只影响生成规范，运行时零依赖：

- `cmd/eventd/main.go` 顶部：General API Info（`@title` / `@version` / `@description` /
  `@BasePath` / 鉴权定义 / tag 说明）；
- 每个 handler 上方：`@Summary` / `@Tags` / `@Router`（外加 `@Param` / `@Success` /
  `@Failure` / `@Security`）。请求体与响应体的类型来自 `internal/api/dto.go` 和
  `internal/model/`，所以规范里的结构就是 handler 真正编解码的结构。

```bash
make swagger                 # 读注解 → api/swagger.json（生成物，别手改）
make swagger-check           # 改了 handler 忘了重新生成：这里会红
```

生成物登记到**服务中心**（service-registry，`127.0.0.1:4240`）是一条命令，幂等，
重复跑无副作用（规范没变化时连 revision 都不刷）：

```bash
SERVICE_NAME=event-center REGISTRY_URL=http://127.0.0.1:4240 \
SWAG_MAIN=cmd/eventd/main.go SWAG_OUT=api \
SWAG_ARGS="--parseInternal --outputTypes json" \
DEPARTMENT_ID=D0002 INSTANCES=127.0.0.1:9099 OWNER=kaulie HEALTH_PATH=/health \
  bash client/ci/register-go-service.sh
```

（`REGISTRY_URL` 必须写死：脚本默认取 `http://127.0.0.1:${SERVICE_PORT}`，而 `SERVICE_PORT`
在 CI/沙箱里常被"当前服务的端口"占用，不写死就会把契约 PUT 到别的服务上。）

它挂在两处自动化里，改完接口只需合入即可：

| 触发点 | 位置 | 说明 |
|---|---|---|
| 发版/打包 | `build.sh` 末尾 | 打包完成后顺手登记；失败只告警，**不挡发布**（`REGISTER_CONTRACT=0` 可跳过） |
| CI | `.github/workflows/register-contract.yml` | push 到 `main` 或手动触发；先 `make swagger-check`，再登记 |

注册中心默认只绑 `127.0.0.1`，所以这两处都必须跑在**本机 / self-hosted runner**。脚本本身
（`client/`）是注册中心 `client/` 的原样拷贝，升级方式见 [`client/README.md`](client/README.md)。

## 交付语义（务必阅读）

投递为 **at-least-once**：网络抖动或下游非 2xx 会重试，下游可能收到重复事件。
请在消费侧按事件 `id`（或 `dedupe_key`）做幂等。**不提供 exactly-once。**

## 入口审计日志（排查数据问题的关键）

每个进入 event-center 的注入请求都会留下一条结构化记录，**无论成功还是被拒**：

```json
{"time":"2026-09-14T15:02:11.234Z","request_id":"d-1","path":"/github-events-ingress",
 "source":"github","status":202,"outcome":"accepted","duration_ms":1,
 "remote_ip":"140.82.115.1","body_sha256":"9f2c…","body_bytes":78,
 "event_id":"evt_01J8…","seq":42,"stream_seq":42,
 "provider":"github","type":"github.push","dedupe_key":"d-1"}
```

字段用途：

| 字段 | 排查什么 |
|---|---|
| `request_id` | 响应头 `X-Request-ID` 回显（优先用调用方的 `X-Request-ID`/`X-Correlation-ID`/`X-GitHub-Delivery`），可直接按它检索整条链路 |
| `outcome` | `accepted` / `duplicate` / `rejected` / `error` |
| `reason` | 被拒原因：签名校验失败、JSON 非法、来源未注册/被禁用、body 过大… |
| `body_sha256` / `body_bytes` | 收到的原始负载指纹（用于确认"到底收到了什么"） |
| `event_id` / `seq` | 成功时可直接去库里取回原文，或用 `/v1/events/{id}` 查 |
| `body` | **被拒请求**会带原始 body（它没有任何其它副本）；成功请求默认不带（已在库里），可用 `EVENTD_INGRESS_LOG_BODY=true` 打开 |

两种落点，互为补充：

1. **journald**（`journalctl -u event-center`）—— 线上即时排障；
2. **JSONL 审计文件**（`EVENTD_INGRESS_LOG_PATH`，部署脚本默认
   `/var/log/event-center/ingress.jsonl`，0600，logrotate 保留 90 天、单文件 100M）
   —— 独立于 journald 轮转与保留策略，**重启不丢**。

> ⚠️ 注意：本机 `/var/log/journal` 默认不存在时 journald 是**易失**的（重启即丢），
> 所以排查数据问题请以审计文件为准；该文件的 `copytruncate` 轮转方式是为
> "进程持有 fd 持续追加" 这个场景专门选的，不要改成 `create`。

排查示例：

```bash
# 最近被拒的请求及原因
grep '"outcome":"rejected"' /var/log/event-center/ingress.jsonl | tail -20

# 某个 GitHub delivery 是否收到、结果如何
grep '"request_id":"<delivery-id>"' /var/log/event-center/ingress.jsonl

# 某条事件是何时、从哪个 IP 进来的（拿到 request_id 后回查）
curl -s localhost:9099/v1/events/evt_01J8... -H "Authorization: Bearer $TOKEN"
```

## 现状与边界

已实现 M1：GitHub 注入、通用注入、持久化、双序号、push（重试/退避/DLQ）、
cursor 拉取（long-poll）、订阅/来源管理、指标与健康检查。

尚未实现（见 `docs/DESIGN.md` 路线图）：SSE 实时流、K8s/Alertmanager 适配器、
Postgres/分区、多实例租约、多租户。当前为**单实例、单写者**。

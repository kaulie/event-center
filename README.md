# event-center

统一事件中心：接收外部事件源（GitHub / K8s / 告警 / 任意系统）的事件注入，
持久化为一条带时序编号的事件日志，并以 pub/sub 方式分发给下游。

- **注入**：对外暴露 Webhook（`/webhooks/github`）与通用注入接口（`/v1/ingest/{source}`）
- **持久化**：所有事件落库（SQLite + WAL），可查询、可回放
- **类型化**：每个事件带 `provider`（生产者）+ `type`（如 `github.pull_request.opened`）
- **时序递增**：全局 `seq` + 流内 `stream_seq`，下游可按游标拉取或订阅推送
- **Pub/Sub**：**push webhook**（重试 + 退避 + DLQ）+ **cursor pull**（long-poll）

设计细节见 [`docs/DESIGN.md`](docs/DESIGN.md)，接口契约见 [`api/openapi.yaml`](api/openapi.yaml)。

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

把 GitHub Webhook 指向 `https://<host>/webhooks/github`，Secret 设为
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
| POST | `/webhooks/github` | GitHub 注入（HMAC 校验，按 delivery id 去重） | source secret |
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
| GET | `/healthz` `/readyz` `/metrics` | 运维 | 无 |

## 配置

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `EVENTD_HTTP_ADDR` | `:8080` | 监听地址 |
| `EVENTD_DB_PATH` | `./data/eventd.db` | SQLite 路径（`:memory:` 用于测试） |
| `EVENTD_ADMIN_TOKEN` | 空 | 管理接口 token；为空时不校验（仅开发） |
| `EVENTD_GITHUB_SECRET` | 空 | 首次启动时据此播种 `github` 来源 |
| `EVENTD_RETENTION_DAYS` | `0` | >0 时每日清理超期事件；0=永久保留 |
| `EVENTD_PULL_DEFAULT_LIMIT` / `EVENTD_PULL_MAX_LIMIT` | `100` / `1000` | 拉取分页 |
| `EVENTD_PULL_WAIT_MAX` | `30s` | long-poll 上限 |
| `EVENTD_PUSH_ENABLED` / `EVENTD_DISPATCHER_DISABLED` | `true` / `false` | 推送开关 |
| `EVENTD_PUSH_BATCH_SIZE` | `50` | 每批投递事件数 |
| `EVENTD_PUSH_MAX_ATTEMPTS` | `6` | 超过则进 DLQ |
| `EVENTD_PUSH_BASE_BACKOFF` / `EVENTD_PUSH_MAX_BACKOFF` | `2s` / `10m` | 重试退避 |
| `EVENTD_PUSH_TIMEOUT` | `10s` | 单次投递超时 |
| `EVENTD_PUSH_POLL_INTERVAL` | `1s` | 队列轮询间隔 |

## 开发

```bash
make test        # 全部测试
make race        # 竞态检测
make run         # 本地运行
make build       # 产出 bin/eventd
make lint        # gofmt 检查 + go vet
```

## 交付语义（务必阅读）

投递为 **at-least-once**：网络抖动或下游非 2xx 会重试，下游可能收到重复事件。
请在消费侧按事件 `id`（或 `dedupe_key`）做幂等。**不提供 exactly-once。**

## 现状与边界

已实现 M1：GitHub 注入、通用注入、持久化、双序号、push（重试/退避/DLQ）、
cursor 拉取（long-poll）、订阅/来源管理、指标与健康检查。

尚未实现（见 `docs/DESIGN.md` 路线图）：SSE 实时流、K8s/Alertmanager 适配器、
Postgres/分区、多实例租约、多租户。当前为**单实例、单写者**。

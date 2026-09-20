# 事件中心（event-center）设计

> 状态：M1 已实现（本仓库）。M2/M3 见文末路线图。

## 1. 目标

统一的事件中心：接收外部事件源（GitHub、K8s 等）的**注入**，**持久化**为一条
可回放的事件日志，并以 **pub/sub** 形式分发给下游消费方。

四项硬性要求：

| # | 要求 | 实现 |
|---|------|------|
| 1 | 对外开放 Webhook 地址供注入 | `POST /webhooks/{provider}`、`POST /v1/ingest/{source}` |
| 2 | 事件持久化 | SQLite（WAL）单表 `events`，不丢；可选 retention |
| 3 | 区分事件类型（provider/producer） | 信封含 `provider` + `type`（命名空间化） |
| 4 | 时序递增，便于下游按需消费/拉取 | 全局 `seq` + 流内 `stream_seq` 双序号；push + cursor 拉取 |

**已确认的选型**：Go；先 SQLite（单表、不分区）；pub/sub 先做 push webhook；
K8s 预留、先做 GitHub；全局 `seq` + `stream_seq` 双序号；单租户；独立服务。

## 2. 总体架构

```
外部事件源                      event-center                      下游消费方
──────────                     ─────────────                     ──────────
GitHub ──HMAC-SHA256──▶  POST /github-events-ingress
通用源 ──Bearer───────▶  POST /v1/ingest/{source}
                              │
                              ▼  归一化信封 → 去重 → 分配 seq/stream_seq
                        ┌───────────────┐
                        │  events (SQLite) │  唯一顺序源 + 持久化
                        └───────┬───────┘
                                │ 扇出（写入 deliveries 队列）
                                ▼
                        ┌───────────────┐        ┌─ Push Dispatcher ─▶ 下游 webhook
                        │  deliveries    │───────▶│  重试/退避/DLQ
                        └───────────────┘        └─ Pull API ────────▶ 下游按游标拉取
```

一条事件只存一份，各订阅以「过滤条件 + 自己的游标」在这份数据上滑动。

## 3. 事件模型

```jsonc
{
  "seq": 1234,                 // 全局单调递增（SQLite AUTOINCREMENT）
  "stream_seq": 87,            // 流内单调递增
  "id": "evt_01J8Q...",        // ULID 风格，可按时间排序
  "stream": "github",          // 通道，订阅的粗粒度过滤维度
  "provider": "github",        // 生产者（来源）
  "type": "github.pull_request.opened",  // 命名空间化类型
  "subject": "repo:kaulie/event-center", // 路由键
  "source_time": "…",          // 事件源声明的时间（可选）
  "received_at": "…",          // 平台接收时间
  "dedupe_key": "…",           // GitHub delivery id 等
  "headers": { "X-GitHub-Event": "pull_request" },
  "data_type": "application/json",
  "data": { …原始负载，原样保存… }
}
```

**类型命名**：`<provider>.<category>[.<action>]`
- `github.push`、`github.pull_request.opened`、`github.workflow_run.completed`
- 预留：`k8s.event.warning`、`k8s.deploy.rollout`、`k8s.node.notready`

**订阅过滤**（见 `internal/filter`）：
- `type_filters`：按 `.` 分段 glob。`*` 匹配一段，`**` 匹配一段或多段。
  `github.*` → `github.push`；`github.**` → `github.pull_request.opened`。
- `provider_filters`：单段 glob。
- `subject_pattern`：路径式 glob（`*` 不跨 `/`），如 `repo:kaulie/*`。
- 省略即全匹配。

## 4. 时序与顺序保证

- `seq`：`INTEGER PRIMARY KEY AUTOINCREMENT`，**单调递增且永不复用**。
- `stream_seq`：由 `streams.last_seq` 在同一事务内自增，流内无空洞。

**为什么不会漏读**：SQLite 只有一个写者，`Store` 用 `writeMu` 把「分配 seq →
插入事件」放进同一个事务，因此**提交顺序 = seq 顺序**，下游用 `seq > cursor`
轮询不会出现「低 seq 晚提交」的可见性空洞。

`after` 语义：具体流按 `stream_seq` 比较；伪流 `all` 按全局 `seq` 比较。

## 5. Pub/Sub 消费模型

### 5.1 Push（本期重点）

- 注入时立即把命中的 push 订阅写入 `deliveries` 队列（扇出）。
- Dispatcher 批量投递（默认 50 条/批）、并发按订阅（默认 4）：
  - `2xx` → `delivered`，并推进订阅游标。
  - `5xx` / `429` / `408` / 网络错误 → **指数退避重试**（`2^n`，上限 10min，带抖动）。
  - `4xx`（非上述） → 不可重试，直接进 **DLQ**（`status=dead`）。
  - 超出 `max_attempts`（默认 6） → 进 DLQ。
- 投递请求头（下游可验签）：
  `X-EventCenter-Signature: sha256=...`（HMAC over raw body，密钥=订阅 secret）、
  `X-EventCenter-Delivery`、`X-EventCenter-Subscription`、`X-EventCenter-Attempt`。
- 交付语义 **at-least-once**：下游需幂等，可用 `id` / `dedupe_key` 去重。
- DLQ 可查看（`GET /v1/deliveries?status=dead`）并重投（`POST /v1/deliveries/requeue`）。
- 崩溃恢复：启动时把 `inflight` 行退回 `pending`，不丢任务、不漏投。
- **暂停**：`pause` 后仍继续入队（不丢事件），只是不投递；`resume` 后按顺序补投。

### 5.2 Pull（按需拉取）

- `GET /v1/streams/{stream}/events?after=<cursor>&limit=<n>&wait=<dur>`
- `wait` 触发 **long-poll**：无新事件时挂起（上限 `EVENTD_PULL_WAIT_MAX`，默认 30s），
  有新事件立即返回，返回体带 `next_cursor` 与 `has_more`。
- `POST /v1/subscriptions/{id}/ack {"cursor":N}` 提交位点，**游标只前进不回退**，
  实现断点续传。
- `after=0` 即从头回放。

## 6. 去重（幂等注入）

`UNIQUE(provider, dedupe_key) WHERE dedupe_key <> ''`：

- GitHub 用 `X-GitHub-Delivery`，重投/重试不会产生重复事件；
- 重复时返回 `200 {"status":"duplicate", "event":<已有事件>}`（不重复扇出）；
- 无 `dedupe_key` 的事件永不判重（允许多次相同内容）。

## 7. 安全

| 面 | 机制 |
|---|---|
| 注入 | GitHub：`X-Hub-Signature-256` HMAC 校验（时间窗 + 常量时间比较）；通用源：`bearer` 或 `hmac_sha256`（按 source 配置）；可用 admin API 启停 source |
| 消费 | `admin token` 或订阅级 `X-API-Key`；创建订阅时自动生成 secret，仅创建响应返回一次 |
| 出站 | 推送带 `X-EventCenter-Signature`，下游可验签 |
| 管理 | `/v1/sources`、`/v1/subscriptions`、`/v1/deliveries` 需 admin token；未配置 token 时放行（仅限开发环境，启动会告警） |

## 8. 存储与保留

单表 `events` + `streams` / `sources` / `subscriptions` / `deliveries`。
按用户要求**不做分区**。可选 `EVENTD_RETENTION_DAYS`：>0 时每日清理
`received_at` 超期事件及其投递记录；=0（默认）永久保留。

> 时间列使用**定宽**格式 `2006-01-02T15:04:05.000000000Z`。`time.RFC3339Nano`
> 会裁剪尾随 0，导致 SQL 里字符串比较出现 `.1Z > .15Z` 的错误排序，故不使用。

## 9. 可观测性

- `GET /healthz`、`GET /readyz`（探测 DB）、`GET /metrics`（Prometheus 文本格式）。
- **入口审计**：每次注入（含被拒请求）都产生一条结构化记录，含 `request_id`
  （回显在 `X-Request-ID`）、`outcome`、`reason`、body 指纹，成功时还带
  `event_id`/`seq`。被拒请求额外保留原始 body——它在别处没有任何副本。
  落点为 journald + 可选 JSONL 文件（`EVENTD_INGRESS_LOG_PATH`），后者独立于
  journald 的轮转与保留策略，是本机"回溯数据问题"的可靠依据。
- 关键指标：注入量（按 provider/stream/type）、去重量、注入失败、入队量、
  推送成功/重试/DLQ、待投递队列深度、全局 seq、各流 seq、HTTP 计数。
- 结构化 JSON 日志（`log/slog`），含 method/path/status/duration_ms。

## 10. 代码结构

```
cmd/eventd/              进程入口（装配 + 优雅退出 + retention janitor）
internal/config/         环境变量配置
internal/model/          领域类型 + 信封归一化
internal/store/          SQLite 持久化（events/sources/subscriptions/deliveries）
internal/service/        用例：注入 + 扇出
internal/filter/         订阅过滤（类型/来源/主体 glob）
internal/verify/         Webhook 验签（HMAC / Bearer）
internal/dispatch/       Push 投递：重试、退避、DLQ
internal/api/            HTTP 路由、中间件、注入/消费/管理/健康
internal/api/dto.go      接口的线上形态（注解引用它们生成契约）
internal/metrics/        轻量 Prometheus 注册表（零依赖）
api/swagger.json         接口契约（由 handler 上的 swag 注解生成；勿手改）
client/                  服务契约登记脚本（注册中心 client/ 的原样拷贝，见 client/README.md）
deploy/                  Dockerfile / docker-compose
```

## 11. 路线图

- **M2**：SSE 实时流（`/v1/streams/{stream}/live`）、订阅编辑 API、批量 ack、
  指标补齐（投递延迟直方图）、K8s/Alertmanager 适配器（`k8s.event.*`、
  `alertmanager.*`，`provider=k8s`）。
- **M3**：高吞吐/多实例：写路径换手给 NATS JetStream / Kafka 作为顺序源，
  SQLite → Postgres 并引入分区与归档；多租户与按 namespace 的 RBAC；
  跨区域副本。

### 已知边界（本期有意为之）

- 单写者：注入吞吐受 SQLite 写锁限制（实测足够应对中小规模；高吞吐走 M3）。
- 单实例：dispatcher 未做分布式租约，多实例会重复投递（at-least-once 语义下
  不致命，但会放大重复）。M3 引入租约表。
- 不做 exactly-once。

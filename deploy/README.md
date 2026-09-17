# 打包与部署（部署系统规范）

本项目按 **agent-control-plane-deployment**（部署控制面，`http://127.0.0.1:4220`，
面板 `/panel/`）的规范组织打包与部署脚本。

## 规范摘要

| 环节 | 约定 |
|---|---|
| 打包 | 仓库根 `build.sh`，调用方传 `APP_VERSION=<8 位短 hash>`，cwd = 仓库根；**必须产出 `outputs/`**，其中**必须含 `scripts/restart.sh`** |
| 发版包 | `outputs/` 的完整内容 + `VERSION` / `COMMIT` / `GIT_REPO_URL`（后三者由调用方写入） |
| 制品存储 | `local` / `github_release` / `aliyun`（可插拔，由控制面负责上传下载；本机当前是 `aliyun`） |
| 部署 | 下载制品 → `rsync -a --delete` 到 `runtimeDir` → 执行 `restartCmd` → 探活 `healthUrl` |
| 保留路径 | **只有** `backend/.env`、`backend/data/`、`backend/runtime.pid`、`backend/server.log`、`backend/.watchdog-paused` 会在部署时保留，其余一律被包内容替换 |
| 注入环境 | `restartCmd` 以 cwd=`runtimeDir` 执行，并注入 `SERVICE_PORT`（取自服务契约的端口/healthUrl）、`RUNTIME_DIR`、`APP_VERSION` |
| 健康检查 | 约定 `http://127.0.0.1:<port>/health`（应用同时保留 `/healthz`） |

> **端口以 `SERVICE_PORT` 为准**：控制面按服务契约注入 `SERVICE_PORT`，`scripts/start.sh`
> 优先读它（兼容旧名 `PORT`），读不到才退回脚本默认值。应用自身也认这个变量
> （见 `internal/config`）：`EVENTD_HTTP_ADDR` > `SERVICE_PORT` > 内置默认。
> 注意**不要**去读裸 `PORT`：CI/沙箱里它常已被别的服务占用（本机 `PORT=4211`），
> 跟着它走会把服务绑到别人的端口上。

## 本项目的落地

```
build.sh                     # 打包：outputs/bin/eventd（平台 runtime 用）
                             #       EC_BUILD_LINUX=1 时额外产出 outputs/bin/eventd-linux-amd64
scripts/start.sh             # 启动：生成/读取 backend/.env，探活 /health，写 pid
scripts/stop.sh              # 停止：TERM → KILL
scripts/restart.sh           # 重启（控制面默认 restartCmd）
deploy/platform/register-service.sh   # 在控制面登记服务契约（幂等）
```

runtime 布局（`EC_RUNTIME_DIR`，默认 `/Users/gaolei/runtime/event-center`）：

```
bin/eventd              ← 发版包
scripts/*.sh            ← 发版包
backend/.env            ← 保留：密钥 + 可选覆盖（首次启动自动生成，0600）
backend/data/           ← 保留：SQLite 事件库
backend/ingress.jsonl   ← 保留：入口审计
backend/runtime.pid     ← 保留：进程号
backend/server.log      ← 保留：日志
VERSION / DEPLOYMENT / COMMIT   ← 平台写入
```

> **为什么运行时状态必须放 `backend/`**：部署是 `rsync --delete`，只有上面那几个
> 路径被保留。把库或密钥放在 `bin/`、`scripts/` 或 runtime 根目录，下一次部署就会
> 被删掉。

## 首次接入（一次性）

```bash
# 1. 登记服务契约（幂等，不触发部署）
deploy/platform/register-service.sh

# 2. 打包 + 部署（等价于面板上点「打包→部署」）
curl -sS -X POST http://127.0.0.1:4220/api/deploy-notify \
  -H 'content-type: application/json' -d '{"serviceId":"event-center"}'
# 或只用已有制品部署：
#   curl -sS -X POST http://127.0.0.1:4220/api/deploys \
#     -H 'content-type: application/json' -d '{"serviceId":"event-center","deployment":"deployment-<hash>"}'
```

进度：面板 `/panel/` 的「部署流水线 / 部署任务」页，或
`GET /api/pipelines`、`GET /api/deploys`。

## 独立脚本（不经控制面）

```bash
/Users/gaolei/deployment/bin/release.sh event-center main     # 打包 → deployment-<hash>
/Users/gaolei/deployment/bin/deploy.sh  event-center deployment-<hash>
```

## 与 cloud-server 部署的关系

`deploy/cloud-server/` 是**另一条部署路径**（远端 Linux + systemd + nginx），
它复用同一个 `build.sh` 产物（`outputs/bin/eventd-linux-amd64`），但启停用
systemd 而不是本目录的 `scripts/`。两条路径共用一份打包逻辑，不重复构建：

| 目标 | 打包 | 启停 | 入口 |
|---|---|---|---|
| 平台 runtime（本机） | `build.sh` → `outputs/` | `scripts/*.sh` | `127.0.0.1:9099` |
| cloud-server（远端） | 同上，取 `eventd-linux-amd64` | systemd unit | nginx `:443` → loopback |

## 可选：graceful restart

控制面支持 `restartNotifyUrl` / `restartPollUrl`（部署前通知 + 轮询是否可重启，
最大等待 `gracefulRestartMaxWaitMs`）。本项目当前**未实现**，即部署时直接重启
（`restartCmd`），GitHub 的失败投递可重投。若要零丢事件，可加一对端点并在契约里登记。

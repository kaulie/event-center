# client/ —— 服务契约登记（从注册中心 vendor 过来的）

这两个脚本**不是本仓库的代码**，是 [kaulie/service-registry](https://github.com/kaulie/service-registry)
里 `client/register.sh` 与 `client/ci/register-go-service.sh` 的原样拷贝，放在这里是为了让 CI /
发版脚本离线可跑（注册中心的脚本自己会浅克隆一份，见下游 `client/ci/github-actions.example.yml`，
本仓库选择 vendor）。**不要手改**：要升级就整份覆盖，并更新下面的 provenance。

| provenance | 值 |
|---|---|
| 上游 | `https://github.com/kaulie/service-registry` |
| 版本 | `main` @ `1e245a82705d09646f26228e36257bcba7fcc32c`（2026-09-20） |
| `register.sh` sha256 | `26459b790511327b76df77496dfb49638f4572b7d4f25827e079b9adbd47c3a2` |
| `register-go-service.sh` sha256 | `7bbf77efa63ae4543458d9b03f80055a3c7669aba3d72f4c5e135a10edc35a8b` |

刷新（在能访问 GitHub 的机器上）：

```bash
tmp="$(mktemp -d)"
git clone --depth 1 https://github.com/kaulie/service-registry "$tmp/service-registry"
cp "$tmp/service-registry/client/register.sh" client/register.sh
cp "$tmp/service-registry/client/ci/register-go-service.sh" client/ci/register-go-service.sh
shasum -a 256 client/register.sh client/ci/register-go-service.sh   # 更新上表
```

## 用法：一条命令（读注解 → 生成规范 → 上报）

契约的真源是**代码里的 swag 注解**（`cmd/eventd/main.go` 的 General API Info + 每个 handler 上的
`@Summary/@Tags/@Router/@Param/@Success`），`api/swagger.json` 是生成物。下面这条命令会自己
`swag init` 再从注解生成规范，然后幂等地登记到注册中心（契约没变化时不会刷 revision）：

```bash
SERVICE_NAME=event-center \
REGISTRY_URL=http://127.0.0.1:4240 \
SWAG_MAIN=cmd/eventd/main.go \
SWAG_OUT=api \
SWAG_ARGS="--parseInternal --outputTypes json" \
DEPARTMENT_ID=D0002 \
INSTANCES=127.0.0.1:9099 \
OWNER=kaulie HEALTH_PATH=/health VERSION="$(git describe --tags --always)" \
  bash client/ci/register-go-service.sh
```

- `REGISTRY_URL` **必须显式给**：`register.sh` 的默认值是 `http://127.0.0.1:${SERVICE_PORT:-4240}`，
  而 `SERVICE_PORT` 在 CI/沙箱里常已被"当前服务的端口"占用（本机预设 `4211`），不写死就会把
  契约 PUT 到**别的服务**上（症状是 404 + 一段不像注册中心的 JSON）。想让默认值可用，
  或者直接 `export SERVICE_PORT=4240`。
- `SWAG_MAIN=cmd/eventd/main.go`：脚本默认探测 `./cmd/<service>/main.go`，而本仓库的入口是
  `cmd/eventd`（服务名 `event-center` 对不上），必须显式给。
- `SWAG_OUT=api`：生成物固定为仓库里的 `api/swagger.json`（默认会写到 `docs/`）。
- `SWAG_ARGS="--parseInternal --outputTypes json"`：与 `make swagger` 同参，保证两条路径产出
  **同一份** `api/swagger.json`；不写的话 swag 的默认输出还会多带 `api/docs.go` 与
  `api/swagger.yaml` 两个用不上的产物。
- `INSTANCES` / `DEPARTMENT_ID` / `OWNER` 都可用环境变量覆盖，部署换端口时不用改仓库。
- 注册中心默认只绑 `127.0.0.1:4240`（写接口默认开放，刻意不暴露到网络），所以这条命令要跑在
  **本机 / self-hosted runner** 上；GitHub-hosted runner 够不到。

自动化现在挂在两处：`.github/workflows/register-contract.yml`（CI）和 `build.sh` 末尾（发版）。

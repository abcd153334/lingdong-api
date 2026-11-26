# 灵动 API

全类型 AI 模型中转网关：对外提供 OpenAI / Claude / Gemini 兼容协议，对内做渠道选路、
规格适配、计费与任务编排。定位是**可自托管的平台基座**，不是某个上游的 SDK 封装。

## 核心能力

| 面 | 能力 |
|---|---|
| 接入 | OpenAI / Claude / Gemini / SUNO 协议；流式（SSE）与非流式 |
| 路由 | 按分组、能力、在售状态选渠道；熔断与自动探测恢复 |
| 计费 | 按 token / 按张 / 按秒 / 按条 / 按次；分组折扣与最低消费 |
| 任务 | 异步提交、轮询、产物转存、终态复查、超时退款 |
| 运营 | 渠道/模型/定价/应用/密钥/公告，全部存库、全节点共享 |
| 可观测 | 调用记录（含报文留痕）、`/api/health` 分层探针、`/metrics` |

## 协议与接口

```
POST /v1/chat/completions      POST /v1/messages        POST /v1beta/models/*:generateContent
POST /v1/images/generations    POST /v1/videos          POST /v1/audio/speech
GET  /v1/tasks/{id}            GET  /v1/tasks/{id}/artifacts
POST /suno/submit/{kind}       GET  /suno/fetch/{id}
```

完整端点与字段：部署后打开管理台「文档站」（`/admin/docs`），内容由数据库驱动。

## 快速开始

前置：Docker 与 Docker Compose v2。

```bash
git clone https://github.com/abcd153334/lingdong-api.git && cd lingdong-api
bash deploy/ld.sh up        # 自动生成配置 → 起 3 个容器 → 等健康
curl -fsS http://127.0.0.1:8088/api/health
```

初始管理员口令只在首次启动打印一次：
`docker compose -f deploy/docker-compose.yml logs backend | grep 初始口令`。

本机 `http://127.0.0.1:8088` 即可打开站点（管理员登录 → 首次强制改密 → 建渠道 → 建 KEY）。
**这个 8088 是整套系统唯一的对外端口**，且只绑回环：对外服务要在它前面放一层终止 TLS 的入口，
两种都随仓提供 —— 宝塔面板反代见 [`deploy/edge/bt/`](deploy/edge/bt/README.md)，
独立 nginx 见 [`deploy/edge/`](deploy/edge/README.md)。完整步骤见
[`deploy/README.md`](deploy/README.md)。

> **页面与 API 在同一个进程里**：前端产物用 `go:embed` 打进二进制，所以没有独立的
> nginx 容器，也没有第二层转发头可配错。生产建议加 TLS；明文入口能登录，
> 但会话票据会以明文过链路（仅适合首次验证）。

> 本系统**不支持 SQLite，Redis 不可省**。账务约束与 worker leader 锁（资金互斥）
> 依赖 PostgreSQL 与 Redis，因此是硬依赖而非可选缓存。

### 部署与运维命令

| 场景 | 命令 |
|---|---|
| 起停 | `bash deploy/ld.sh up\|down\|restart` |
| 升级 | `bash deploy/scripts/upgrade.sh <TAG>`（`--app-only` 只换应用镜像） |
| 回滚 | `bash deploy/scripts/upgrade.sh <旧TAG> --no-backup`（`data_epoch` 变过须连带恢复备份） |
| 备份 / 恢复 | `bash deploy/scripts/backup.sh` / `bash deploy/scripts/restore.sh` |
| 健康检查 | `bash deploy/scripts/healthcheck.sh --expect-version <TAG> --require-db` |
| 打标签发布 | `bash deploy/scripts/tag-release.sh <TAG>` |
| 一键更新代理 | `deploy/agent/install.sh`（systemd 常驻，不挂 `docker.sock` 进 Web） |

## 开发与构建

```bash
# 后端
cd backend && go build ./... && go vet ./... && go test -p 1 ./internal/...

# 前端
cd web && pnpm install && pnpm build        # tsc --noEmit && vite build

# 全量门禁
powershell -File scripts/verify.ps1
```

集成测试需要 PostgreSQL：

```bash
LD_TEST_DSN='postgres://user:pass@127.0.0.1:5432/ldtest_ci?sslmode=disable' \
LD_REQUIRE_TEST_DSN=1 go test -p 1 ./internal/...
```

`LD_REQUIRE_TEST_DSN=1` 会让缺失 DSN 的集成用例**失败而不是跳过**——不带它跑出的
「全绿」不算数。

## 目录结构

```
backend/            Go + Gin（入口 main.go，业务在 internal/）
web/                前端（Vite + React + TS）
deploy/             编排、镜像、部署与升级脚本、更新代理
scripts/            质量门禁（verify.ps1、check-repo-hygiene.sh 等）
testdata/           前后端共用的规格夹具
```

## 仓库约定

本仓**只承载系统代码**：设计文档、厂商资料、原型、备份、第三方源码副本一律不入库。
提交前必须通过：

```bash
bash scripts/check-repo-hygiene.sh
```

它做白名单校验、敏感值扫描（含藏在预签名 URL 里的云厂商 AK）与误产物识别，
是「哪些文件能进仓库」的唯一判据。

## 发布新版本

改完代码，一条命令：

```bash
bash scripts/release.sh "修登录页按钮错位"
```

它按固定顺序做五件事，任何一步失败就停：跑门禁 → 提交 → 推代码 → 自动算下一个
版本号（`v2.2.14` → `v2.2.15`）→ 打 tag 并推送。`--minor` / `--major` 换递增位，
`--no-tag` 只推代码不发版本，`--dry-run` 只看会做什么。

**为什么顺序不能颠倒**：先推 tag 后推代码，等于发布一个云端还不存在的版本 ——
CI 找不到那个 commit，用户端会拿到一个永远装不上的更新。脚本把这条顺序写死了。

**重复执行是安全的**：没有改动时它直接拒绝，不会凭空多一个版本号（否则误按一次
回车就多一轮 CI，用户还会收到一条"有新版本"却什么都没变的提示）。

推完 tag 就不用管了，CI 会自动产出双架构镜像与发布清单（2–11 分钟）：

```
https://github.com/abcd153334/lingdong-api/actions
```

之后新装的用户直接拿到新版本；已在跑的用户 6 小时内看到「有新版本」提示，
点一下就更新。

手工等价（脚本坏了也不至于卡住）：

```bash
git add -A && git commit -m "说明" && git push origin main
git tag -a v2.2.15 -m "说明" && git push origin v2.2.15
```


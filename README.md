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
cp deploy/.env.example deploy/.env && chmod 600 deploy/.env   # 填 4 项必填
bash deploy/scripts/preflight.sh                              # 环境自检
bash deploy/ld.sh up                                          # 起四容器
curl -fsS http://127.0.0.1/api/health                         # 分层健康探针
```

浏览器打开站点 → 管理员登录（首次强制改密）→ 建渠道 → 建 KEY。

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

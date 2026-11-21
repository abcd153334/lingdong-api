# syntax=docker/dockerfile:1
# 灵动 API · 应用镜像（v2.2.0）
#
# 一个镜像 = 一个进程 = 前端页面 + API。前端产物在**构建期**被拷进
# backend/internal/webui/dist，由 go:embed 打进二进制（见 internal/webui）。
# 不再有 nginx 容器、不再有第二层反代 —— 少一份会配错的转发头配置。
#
# 构建上下文 = 仓库根目录（见 docker-compose.yml 的 context: ..）

# ---------- 阶段 1：前端产物 ----------
FROM node:22-alpine AS web

# 国内 npm 镜像（构建期网络受限环境必需）
ENV COREPACK_NPM_REGISTRY=https://registry.npmmirror.com
ENV npm_config_registry=https://registry.npmmirror.com

# pnpm 必须**锁版本**，不能用 `corepack enable` 放任它自己解析。
# 没有 packageManager 字段时 corepack 取内置默认版本，而 web/pnpm-lock.yaml 是 pnpm 12
# 写的 —— 大版本不匹配时 `--frozen-lockfile` 直接失败（v2.2.0 的首次 CI 发布就挂在这里，
# 报错只有一句 "process ... did not complete successfully"）。
# 版本与 web/package.json 的 packageManager 字段保持一致。
RUN npm install -g pnpm@12.14.0

WORKDIR /web

# 先只拷清单，让依赖层单独命中缓存：改一行前端代码不必重装依赖。
COPY web/package.json web/pnpm-lock.yaml ./
# --frozen-lockfile：锁文件与 package.json 不一致时**直接失败**。
# 不加这个参数，pnpm 会静默改写锁文件并装出"另一套依赖"——
# 镜像与本地/CI 就不是同一棵依赖树了，而构建仍然"成功"。
RUN pnpm install --frozen-lockfile

COPY web/ ./
RUN pnpm build

# ---------- 阶段 2：后端二进制（内嵌前端） ----------
# 构建镜像的 Go 版本必须 **≥ go.mod 的 go 指令**。用 1.23 编译 1.22 模块是正常且受支持的；
# 反过来会直接失败。门禁 check-version.sh 断言的是「≥」而不是「相等」。
FROM golang:1.23-alpine AS builder

# 版本号由发布流程注入（tag-release.sh / compose build.args → 这里 → -X）。
# 默认 dev：本地随手 build 时不至于把某个正式版本号写进镜像。
ARG VERSION=dev

WORKDIR /src

# 国内 Go module 代理（构建期网络受限环境必需；goproxy.cn 镜像）
ENV GOPROXY=https://goproxy.cn,direct

# 先取依赖清单以复用缓存；go.sum 缺失时也能构建（后续 go build 会补全）
COPY backend/go.mod ./
COPY backend/go.sum* ./
RUN go mod download

COPY backend/ ./
# 前端产物落在 go:embed 的路径上。仓库里只有一个 .keep 占位，
# 保证纯净 clone 下 `go build ./...` 能过；真正的 dist 只存在于镜像构建期。
COPY --from=web /web/dist ./internal/webui/dist

# -X 只能覆盖字符串 **var**：controller.Version 写成 const 时链接器会静默忽略，
# 镜像里的版本号永远是源码字面量（回滚无法确认跑的是哪个 tag）。
#
# -tags nodynamic 是**启动必需**，不是性能取舍 —— 缺了它容器会以退出码 139 无限重启、
# 且一行日志都不打。链路是：service/upload 用了 github.com/gen2brain/webp，
# 它在 (unix|darwin|windows) && !nodynamic 下编译 webp_dynamic.go → 引入
# github.com/ebitengine/purego → purego 在 linux && !cgo 下启用 internal/fakecgo，
# 后者用 go:linkname 注册 _cgo_init。Go 运行时在 rt0_go 里一旦看到 _cgo_init 就调用它，
# 而那一刻**任何 Go init() 都还没执行**：fakecgo.malloc 里的 call5(mallocABI0, …)
# 目标地址仍是 0 → 跳到地址 0 → SIGSEGV（dmesg: ip 0000000000000000）。
# 打上 nodynamic 后 purego 整个退出编译，webp 改走内嵌 libwebp 的 WASM（wazero）实现，
# 二进制回到真静态链接（program headers 里不再有 PT_INTERP / PT_DYNAMIC）。
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -tags nodynamic \
      -ldflags="-s -w -X 'lingdong-api/internal/controller.Version=${VERSION}'" \
      -o /out/server .

# ---------- 阶段 3：运行镜像 ----------
FROM alpine:3.21

# ca-certificates（出站 HTTPS）、tzdata（TIMESTAMPTZ 展示）、wget（健康检查）
RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app
COPY --from=builder /out/server /app/server

# 非 root 运行：进程被攻破时不再拥有容器内 root，
# 且 STORAGE_DIR 的落盘文件归 uid 10001 所有（备份/迁移不必再 sudo）。
# 先建目录并 chown，再切用户 —— 否则挂载卷的属主是 root，应用写入直接 EACCES。
RUN addgroup -g 10001 -S app && adduser -u 10001 -S app -G app \
 && mkdir -p /app/data && chown -R 10001:10001 /app
USER 10001:10001

EXPOSE 8080

# 健康检查命中通用契约端点 GET /api/health（deep=1 时才真的探 DB/Redis）
HEALTHCHECK --interval=10s --timeout=6s --retries=6 --start-period=60s \
  CMD wget -qO- 'http://127.0.0.1:8080/api/health?deep=1' >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/server"]

# syntax=docker/dockerfile:1
# 灵动 API · 后端镜像（多阶段：golang 构建 → 精简运行镜像）
# 构建上下文 = 仓库根目录（见 docker-compose.yml 的 context: ..）

# ---------- 构建阶段 ----------
# 构建镜像的 Go 版本必须 **≥ go.mod 的 go 指令**（这里是 go 1.22）。
# 用 1.23 编译 1.22 模块是正常且受支持的；反过来（用 1.22 编 1.23 模块）会直接失败。
# 门禁 check-version.sh 断言的是「≥」而不是「相等」—— 相等会把升级工具链变成改源码。
FROM golang:1.23-alpine AS builder

# 版本号由发布流程注入（tag-release.sh → compose build.args → 这里 → -X）。
# 默认 dev：本地随手 build 时不至于把某个正式版本号写进镜像。
ARG VERSION=dev

WORKDIR /src

# 国内 Go module 代理（构建期网络受限环境必需；goproxy.cn 镜像）
ENV GOPROXY=https://goproxy.cn,direct

# 先取依赖清单以复用缓存；go.sum 缺失时也能构建（后续 go build 会补全）
COPY backend/go.mod ./
COPY backend/go.sum* ./
RUN go mod download

# 拷贝后端源码并编译为静态可执行文件（module lingdong-api，入口 main.go）
#
# -X 只能覆盖字符串 **var**：controller.Version 写成 const 时链接器会静默忽略，
# 镜像里的版本号永远是源码字面量（回滚无法确认跑的是哪个 tag）。
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X 'lingdong-api/internal/controller.Version=${VERSION}'" \
      -o /out/server .

# ---------- 运行阶段 ----------
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


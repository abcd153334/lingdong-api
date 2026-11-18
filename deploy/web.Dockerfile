# syntax=docker/dockerfile:1
# 灵动 API · 前端镜像（多阶段：node 构建 web/ → nginx 服务 dist）
# 构建上下文 = 仓库根目录（见 docker-compose.yml 的 context: ..）

# ---------- 构建阶段 ----------
FROM node:22-alpine AS builder

WORKDIR /app

# 国内 npm 镜像（构建期网络受限环境必需）
ENV COREPACK_NPM_REGISTRY=https://registry.npmmirror.com
ENV npm_config_registry=https://registry.npmmirror.com

# 启用 pnpm（版本由 corepack 决定；web/pnpm-lock.yaml 的 lockfileVersion 是 9.0）。
RUN corepack enable

COPY web/ ./
# --frozen-lockfile：锁文件与 package.json 不一致时**直接失败**。
# 不加这个参数，pnpm 会静默改写锁文件并装出"另一套依赖"——
# 镜像与本地/CI 就不是同一棵依赖树了，而构建仍然"成功"。
RUN pnpm install --frozen-lockfile && pnpm build

# ---------- 运行阶段（nginx 静态服务 + API 反向代理）----------
# 仍用官方镜像（以 root master 启动后降权到 nginx worker）：它需要绑定 80。
# 若要去掉 root，改用 nginxinc/nginx-unprivileged 并把 listen 改成 8080 +
# compose 端口映射同步改 80:8080 —— 这是一次配套改动，不能只改镜像名。
FROM nginx:1.29-alpine

# 拷贝站点配置（静态资源 + /api、/v1、/f/ 反代 backend:8080 + SPA fallback）
COPY deploy/nginx.conf /etc/nginx/conf.d/default.conf

# 构建产物落盘为静态站点
COPY --from=builder /app/dist /usr/share/nginx/html

EXPOSE 80

HEALTHCHECK --interval=10s --timeout=5s --retries=6 --start-period=10s \
  CMD wget -qO- http://127.0.0.1/ >/dev/null 2>&1 || exit 1

CMD ["nginx", "-g", "daemon off;"]

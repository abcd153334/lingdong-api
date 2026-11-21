#!/usr/bin/env bash
# 公共库：路径、日志、.env 读取、前置断言。所有部署脚本都 source 本文件。
#
# 为什么不用 `source .env`：口令里可能出现空格、引号、`$`、`#`。
# shell 会展开变量、截断注释，最终读到的是"另一个口令"，而报错信息完全看不出来。
# 这里逐行 `KEY=` 取值，只做去引号与去 CR，不解释任何元字符。

set -euo pipefail

# 三个路径必须都相对「本文件」推导 —— 本文件在 deploy/scripts/lib/ 下，
# 而各脚本按 SCRIPT_DIR=deploy/scripts、ROOT_DIR=仓库根 使用它们
# （见 scripts/check-version.sh 等独立脚本的同类推导）。
#
# 历史缺陷：这里曾直接用 BASH_SOURCE 的目录当 SCRIPT_DIR，于是
# SCRIPT_DIR=deploy/scripts/lib、DEPLOY_DIR=deploy/scripts、ROOT_DIR=deploy，
# 三者各浅一级 —— ENV_FILE/COMPOSE_FILE/.release/fingerprint.sql/preflight.sh
# 全部指向不存在的路径，`bash deploy/scripts/preflight.sh` 第一步就报
# 「找不到 deploy/scripts/.env」，所有 source 本文件的脚本都不可用。
LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_DIR="$(cd "$LIB_DIR/.." && pwd)"
DEPLOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_DIR="$(cd "$DEPLOY_DIR/.." && pwd)"
ENV_FILE="${ENV_FILE:-$DEPLOY_DIR/.env}"
COMPOSE_FILE="${COMPOSE_FILE:-$DEPLOY_DIR/docker-compose.yml}"

log()  { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
warn() { printf '  \033[33mWARN\033[0m %s\n' "$*" >&2; }
die()  { printf '  \033[31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }

# read_env KEY [默认值]：读取 .env 中的值（不 source）
read_env() {
  local key="$1" default="${2-}" line
  [ -f "$ENV_FILE" ] || { printf '%s' "$default"; return 0; }
  line="$(grep -E "^[[:space:]]*${key}=" "$ENV_FILE" | tail -n 1 || true)"
  [ -n "$line" ] || { printf '%s' "$default"; return 0; }
  line="${line#*=}"
  line="${line%$'\r'}"
  case "$line" in
    \"*\") line="${line#\"}"; line="${line%\"}" ;;
    \'*\') line="${line#\'}"; line="${line%\'}" ;;
  esac
  printf '%s' "$line"
}

env_has() { grep -qE "^[[:space:]]*$1=" "$ENV_FILE" 2>/dev/null; }

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1（未安装或不在 PATH）"; }

require_docker() {
  require_cmd docker
  docker compose version >/dev/null 2>&1 || die "docker compose v2 不可用（需要 Docker Engine ≥24）"
  [ -f "$COMPOSE_FILE" ] || die "找不到编排文件：$COMPOSE_FILE"
  [ -f "$ENV_FILE" ] || die "找不到 $ENV_FILE（先 cp deploy/.env.example deploy/.env）"
}

# 解析出后端实际使用的 DSN：显式 DATABASE_URL 优先，否则按 POSTGRES_PASSWORD 派生。
resolve_dsn() {
  local explicit; explicit="$(read_env DATABASE_URL '')"
  if [ -n "$explicit" ]; then printf '%s' "$explicit"; return 0; fi
  local pw user db
  pw="$(read_env POSTGRES_PASSWORD '')"
  user="$(read_env POSTGRES_USER 'lingdong')"
  db="$(read_env POSTGRES_DB 'lingdong')"
  [ -n "$pw" ] || die "POSTGRES_PASSWORD 为空：无法派生 DATABASE_URL"
  printf 'postgres://%s:%s@postgres:5432/%s?sslmode=disable' "$user" "$pw" "$db"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

#!/usr/bin/env bash
# compose 封装：统一从 deploy/ 目录执行，保证 .env 被自动读取（compose 以 -f 所在目录为项目目录）。
# 只做转发，不掺业务逻辑 —— 所有判断留在具体脚本里，排障时不必猜"包装层做了什么"。

compose() {
  ( cd "$DEPLOY_DIR" && docker compose -f "$(basename "$COMPOSE_FILE")" "$@" )
}

# compose_tag TAG ARGS...：带镜像标签执行（升级/回滚用）
compose_tag() {
  local tag="$1"; shift
  ( cd "$DEPLOY_DIR" && TAG="$tag" docker compose -f "$(basename "$COMPOSE_FILE")" "$@" )
}

compose_ps_running() {
  compose ps --status running --services 2>/dev/null | sort
}

#!/usr/bin/env bash
# 健康检查：浅探（进程活着）+ 深探（DB/Redis 真可用）。
#
# 为什么要分两层：容器 healthcheck 用深探（Redis 挂了要标 unhealthy 让人看见），
# 但"能不能对外接流量"是另一个问题——deep 失败时站点仍是可用的（门户静态 + API 503）。
# 本脚本把两者都打出来，避免值班把"unhealthy"直接读成"全站挂了"。
#
# 用法：bash deploy/scripts/healthcheck.sh [--wait 90] [--base URL]
#                                          [--expect-version vX.Y.Z] [--require-db]
#
# 升级校验必须同时给 --expect-version 与 --require-db：
#   - 只探 200 抓不到"容器没重建 / -ldflags 注入失效"（旧进程仍在服务，照样 200）；
#   - deep 在 Redis 挂时返回 503，但那时 DB 是好的。所以 --require-db **只看
#     data.deps.db 这个字段**，不把 HTTP 状态当判据 —— 否则 Redis 重启的 10 秒里
#     会把一个健康的新版本判成失败并触发回滚。

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

BASE="http://127.0.0.1:8080"
WAIT=0
EXPECT=""
REQUIRE_DB=0
while [ $# -gt 0 ]; do
  case "$1" in
    --wait) WAIT="$2"; shift 2 ;;
    --base) BASE="$2"; shift 2 ;;
    --expect-version) EXPECT="$2"; shift 2 ;;
    --require-db) REQUIRE_DB=1; shift ;;
    -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
    *) die "未知参数：$1" ;;
  esac
done

require_cmd curl

probe() {
  local url="$1" out code
  out="$(curl -sS --max-time 5 -w '\n%{http_code}' "$url" 2>/dev/null || true)"
  code="${out##*$'\n'}"
  printf '%s\n%s' "${out%$'\n'*}" "$code"
}

# 从 gin 的紧凑 JSON 里取一个字符串字段（响应是 {code,message,data:{...}} 三层，
# 但字段名在整个响应里唯一，按 `"key":"value"` 直接取即可，不引入 jq 依赖）。
json_str() {
  printf '%s' "$1" | tr -d '\n' | sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p"
}

deadline=$(( $(date +%s) + WAIT ))
seen_version=""
while :; do
  shallow="$(probe "$BASE/api/health")"
  code="${shallow##*$'\n'}"
  body="${shallow%$'\n'*}"
  if [ "$code" = "200" ]; then
    if [ -z "$EXPECT" ]; then break; fi
    seen_version="$(json_str "$body" version)"
    if [ "$seen_version" = "$EXPECT" ]; then break; fi
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    if [ "$code" = "200" ] && [ -n "$EXPECT" ]; then
      die "版本核对失败：期望 $EXPECT，实际 ${seen_version:-<空>}（$BASE/api/health）—— 镜像可能没重建，或 -ldflags 注入失效"
    fi
    die "浅探失败：HTTP $code（$BASE/api/health）—— 先看 docker compose logs backend"
  fi
  sleep 2
done

log "浅探（进程存活）"
printf '%s\n' "$body" | tr ',' '\n' | sed -n 's/.*"\(status\|version\|mode\|uptime\|node\)":\("[^"]*"\|[0-9]*\).*/  \1 = \2/p'
if [ -n "$EXPECT" ]; then ok "版本核对通过：$EXPECT"; fi

deep="$(probe "$BASE/api/health?deep=1")"
dcode="${deep##*$'\n'}"
dbody="${deep%$'\n'*}"
log "深探（DB / Redis）"
printf '%s\n' "$dbody" | tr ',' '\n' | sed -n 's/.*"\(db\|redis\|status\)":\("[^"]*"\).*/  \1 = \2/p'

if [ "$REQUIRE_DB" = "1" ]; then
  # 只看 deps.db：HTTP 可能因 Redis 不可用而是 503，但 DB 好就说明新版本能正常服务。
  db_state="$(json_str "$dbody" db)"
  while [ "$db_state" != "ok" ] && [ "$(date +%s)" -lt "$deadline" ]; do
    sleep 2
    deep="$(probe "$BASE/api/health?deep=1")"
    dbody="${deep%$'\n'*}"
    db_state="$(json_str "$dbody" db)"
  done
  if [ "$db_state" != "ok" ]; then
    die "深探未通过：deps.db=${db_state:-<缺失>}（HTTP ${dcode}）—— 数据库不可用，本次切换不算成功"
  fi
  ok "深探通过：db=ok（HTTP $dcode）"
  exit 0
fi

echo
if [ "$dcode" = "200" ]; then
  ok "深探通过（$dcode）"
else
  warn "深探未通过（HTTP $dcode）：依赖有异常。门户仍可访问，API 会返回 503/降级；按上面的 db/redis 字段定位"
fi

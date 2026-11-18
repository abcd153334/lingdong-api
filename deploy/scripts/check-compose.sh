#!/usr/bin/env bash
# G2：编排门禁。
#
# 正向：单机 / 集群（full、api 两个 profile）都能被 compose 解析。
# 负向：缺 TAG、缺 APP_ENCRYPTION_KEY、缺 POSTGRES_PASSWORD 必须**失败** ——
#       一个"少填也能起来"的模板，等于把密钥闸门交给运气。
#
# 没有 docker 时输出 SKIP 并以 0 退出，但**这不是通过**：
# 报告里必须把它列为"未验证"。静默变绿比不跑更危险。
#
# 用法：bash deploy/scripts/check-compose.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TMP_ENV="/tmp/ld-compose-check.env"
fail=0

if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  echo "SKIP 未安装 docker / compose v2：编排未验证（不得计为通过）"
  exit 0
fi

cd "$DEPLOY_DIR"

# 一份最小可解析的 env（只用于 config，不会真的启动）
cat > "$TMP_ENV" <<'EOF'
APP_ENV=prod
POSTGRES_PASSWORD=dummy_password_for_config
REDIS_PASSWORD=dummy_password_for_config
APP_ENCRYPTION_KEY=0123456789abcdef0123456789abcdef
EOF

echo "== 正向：单机"
if docker compose --env-file "$TMP_ENV" -f docker-compose.yml config -q 2>/tmp/ld-c1.err; then
  echo "PASS docker-compose.yml 可解析"
else
  echo "FAIL docker-compose.yml 无法解析：$(head -2 /tmp/ld-c1.err | tr '\n' ' ')"
  fail=1
fi

echo "== 正向：集群 full / api"
for p in full api; do
  if TAG=v0.0.0 docker compose --env-file "$TMP_ENV" -f cluster/docker-compose.node.yml \
       --profile "$p" config -q 2>/tmp/ld-c2.err; then
    echo "PASS cluster --profile $p 可解析"
  else
    echo "FAIL cluster --profile $p 无法解析：$(head -2 /tmp/ld-c2.err | tr '\n' ' ')"
    fail=1
  fi
done

echo "== 负向：缺 TAG 必须失败"
if TAG= docker compose --env-file "$TMP_ENV" -f cluster/docker-compose.node.yml \
     --profile api config -q >/dev/null 2>&1; then
  echo "FAIL 集群编排缺 TAG 仍然解析成功：禁止 latest 的闸门失效"
  fail=1
else
  echo "PASS 缺 TAG 被拒绝"
fi

echo "== 负向：缺 APP_ENCRYPTION_KEY 必须失败"
sed '/^APP_ENCRYPTION_KEY=/d' "$TMP_ENV" > "$TMP_ENV.nokey"
if docker compose --env-file "$TMP_ENV.nokey" -f docker-compose.yml config -q >/dev/null 2>&1; then
  echo "FAIL 缺 APP_ENCRYPTION_KEY 仍然解析成功：密钥闸门失效"
  fail=1
else
  echo "PASS 缺 APP_ENCRYPTION_KEY 被拒绝"
fi

exit "$fail"

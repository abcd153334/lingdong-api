#!/usr/bin/env bash
# G1：环境变量覆盖门禁。
#
# 查两件事：
#   1. 代码读取的变量，必须在 deploy/.env.example 登记 —— 否则"有这个开关"没人知道；
#   2. .env.example 里的变量，代码必须真的读 —— 否则是幽灵变量（写了不生效）。
#
# 原先还有第 3 项「docs/deployment/configuration.md 与模板同步」。文档已按
# 「公开仓只放系统代码」的铁律移出仓库，这一项随之删除（生成器 gen-env-doc.sh 同去）。
# 保留的这两项不依赖任何文档，仍是有意义的一致性检查。
#
# 扫描范围刻意收窄：只认 `os.Getenv("X")` / `getenv*("X")`，且排除 _test.go。
# 不这么做的话，任何字符串字面量都会被当变量（实测 76 个里有一半是误报），
# 门禁一旦是噪音，下一步就是被关掉。
#
# 用法：bash deploy/scripts/check-env-coverage.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
TPL="$ROOT_DIR/deploy/.env.example"

[ -f "$TPL" ] || { echo "FAIL 找不到 $TPL" >&2; exit 1; }

# 豁免：测试专用 / 已废弃 / 内部开关（改动这里必须写理由）
EXEMPT="
LD_TEST_DSN              集成测试库（不进部署模板）
LD_REQUIRE_TEST_DSN      门禁开关（缺 DSN 时必须变红）
LD_CAPTCHA_TEST_CODE     验证码测试码（非 dev/test 会被拒绝启动）
LD_STORAGE_GC_SELFTEST   存储 GC 自检
JWT_SECRET               已废弃（改用服务端会话）
BILLING_DRYRUN           计费演练开关（研发用）
LD_CAP_ROUTING           能力路由内部开关
LD_CATALOG_REQUIRE_BINDING 目录绑定校验内部开关
LD_CLAUDE_MESSAGES       协议适配内部开关
LD_DUOMI_NEW_MODELS      适配器内部开关
LD_RECLAIM_NO_EVIDENCE   回收策略内部开关
LD_SUBMIT_IDEMPOTENT     提交幂等内部开关
LD_TASK_USAGE_SETTLE     任务结算内部开关
APP_PORT                 编排层宿主端口（compose 用，Go 不读）
POSTGRES_PASSWORD        编排层库口令（compose 用，Go 读 DATABASE_URL）
REDIS_PASSWORD           编排层 Redis 口令（compose 用，Go 读同名变量时也用它）
"

code_vars="$(cd "$ROOT_DIR/backend" && grep -rhoE '(os\.Getenv|getenv[A-Za-z]*)\("[A-Z0-9_]+"' \
  --include=*.go --exclude=*_test.go . | sed -E 's/.*\("//; s/"$//' | sort -u)"
tpl_vars="$(grep -oE '^#?[[:space:]]*[A-Z][A-Z0-9_]*=' "$TPL" | sed -E 's/^#?[[:space:]]*//; s/=$//' | sort -u)"

is_exempt() { printf '%s\n' "$EXEMPT" | awk '{print $1}' | grep -qx "$1"; }

fail=0

missing="$(comm -23 <(printf '%s\n' "$code_vars") <(printf '%s\n' "$tpl_vars") | while read -r v; do
  [ -n "$v" ] || continue
  is_exempt "$v" || echo "$v"
done)"

if [ -n "$missing" ]; then
  echo "FAIL 代码读取但模板未登记（要么加进 .env.example，要么进 EXEMPT 并写理由）："
  printf '%s\n' "$missing" | sed 's/^/  - /'
  fail=1
else
  echo "PASS 代码读取的变量已全部登记或豁免"
fi

ghost="$(comm -13 <(printf '%s\n' "$code_vars") <(printf '%s\n' "$tpl_vars") | while read -r v; do
  [ -n "$v" ] || continue
  # compose 层变量（PG/网络）由编排读取，Go 不读属正常
  case "$v" in POSTGRES_*|LD_SUBNET|APP_PORT) continue ;; esac
  # 通过常量间接读取的变量
  case "$v" in LD_BOOTSTRAP_ADMIN_PASSWORD|LD_DB_MAX_OPEN|LD_DB_MAX_IDLE) continue ;; esac
  echo "$v"
done)"

if [ -n "$ghost" ]; then
  echo "FAIL 模板有但代码不读（幽灵变量：写了不生效）："
  printf '%s\n' "$ghost" | sed 's/^/  - /'
  fail=1
else
  echo "PASS 模板里没有幽灵变量"
fi

exit "$fail"

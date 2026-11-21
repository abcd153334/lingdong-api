#!/usr/bin/env bash
# 恢复：把 dump 恢复到**指定的新库**，并与备份自带的指纹逐项比对。
#
# 为什么强制 --target-dsn：恢复最重要的不是"灌回去"，而是"灌回去之后能证明它是对的"。
# 直接覆盖生产库会同时干掉现场证据；正确做法是新库恢复 → 比对指纹 → 抽样产物 → 再切。
#
# 用法：
#   bash deploy/scripts/restore.sh <dump> --target-dsn <DSN> --i-know \
#     [--verify <verify.txt>] [--storage <storage.tgz>] [--force]

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
source "$(dirname "${BASH_SOURCE[0]}")/lib/compose.sh"

if [ $# -lt 1 ]; then
  sed -n '2,9p' "$0"; exit 1
fi

DUMP="$1"; shift
TARGET_DSN=""
VERIFY_FILE=""
STORAGE_TGZ=""
CONFIRM=0
FORCE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --target-dsn) TARGET_DSN="$2"; shift 2 ;;
    --verify) VERIFY_FILE="$2"; shift 2 ;;
    --storage) STORAGE_TGZ="$2"; shift 2 ;;
    --i-know) CONFIRM=1; shift ;;
    --force) FORCE=1; shift ;;
    -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
    *) die "未知参数：$1" ;;
  esac
done

[ -f "$DUMP" ] || die "找不到备份文件：$DUMP"
[ "$CONFIRM" = "1" ] || die "这是破坏性操作：必须显式加 --i-know（并在非生产库上先演练）"
[ -n "$TARGET_DSN" ] || die "必须指定 --target-dsn（不要把备份直接覆盖到生产库）"
require_docker

# 默认找同名 verify 文件
if [ -z "$VERIFY_FILE" ]; then
  guess="${DUMP%.dump}.verify.txt"
  if [ -f "$guess" ]; then VERIFY_FILE="$guess"; fi
fi

log "目标库：$(printf '%s' "$TARGET_DSN" | sed -E 's#(://[^:]+:)[^@]+@#\1***@#')"

# ---------- 1. 目标库非空则拒绝 ----------
tables="$(compose exec -T postgres psql "$TARGET_DSN" --no-psqlrc --tuples-only --no-align \
  -c "select count(*) from information_schema.tables where table_schema='public'" 2>/dev/null | tr -d '\r' || true)"
if [ -z "$tables" ]; then
  die "连不上目标库（检查 --target-dsn；容器内可达性也要确认）"
fi
if [ "$tables" -gt 0 ] && [ "$FORCE" != "1" ]; then
  die "目标库已有 $tables 张表。恢复会 --clean 掉它们：确认是空库，或加 --force 明确承担后果"
fi

# ---------- 2. 恢复 ----------
log "开始恢复（pg_restore --clean --if-exists）"
if ! compose exec -T postgres pg_restore --clean --if-exists --no-owner --no-acl \
      -d "$TARGET_DSN" < "$DUMP"; then
  warn "pg_restore 返回非 0：常见于「对象已存在」类告警，需看上面的具体报错再判断是否成功"
fi

# ---------- 3. 指纹比对 ----------
actual="$(compose exec -T postgres psql "$TARGET_DSN" --no-psqlrc --tuples-only --no-align \
  < "$SCRIPT_DIR/lib/fingerprint.sql" | tr -d '\r')"
log "恢复后指纹："
printf '%s\n' "$actual"

if [ -n "$VERIFY_FILE" ] && [ -f "$VERIFY_FILE" ]; then
  expected="$(grep -E '^fingerprint = ' "$VERIFY_FILE" | head -1 | sed 's/^fingerprint = //')"
  if [ -n "$expected" ]; then
    norm() { printf '%s' "$1" | tr -d '[:space:]|'; }
    if [ "$(norm "$expected")" = "$(norm "$actual")" ]; then
      ok "9 项账务指纹与备份一致"
    else
      echo "  期望：$expected"
      echo "  实际：$actual"
      die "指纹不一致：这份恢复不可用，不要切流"
    fi
  else
    warn "verify 文件里没有 fingerprint 行，跳过比对"
  fi
else
  warn "没有 verify 文件，无法自动比对指纹 —— 请人工核对上面的 9 个数"
fi

# ---------- 4. 产物（可选，但本地存储部署必须做） ----------
if [ -n "$STORAGE_TGZ" ]; then
  [ -f "$STORAGE_TGZ" ] || die "找不到产物包：$STORAGE_TGZ"
  log "解包产物 → backend:/app/data"
  compose exec -T backend mkdir -p /app/data
  if ! compose exec -T backend tar xzf - -C /app/data < "$STORAGE_TGZ"; then
    die "产物解包失败（backend 容器是否在运行？）"
  fi
  # 只解不验等于没验：从 files 表抽 10 条 storage_key，逐个确认文件在盘上。
  sample="$(compose exec -T postgres psql "$TARGET_DSN" --no-psqlrc --tuples-only --no-align \
    -c "select storage_key from files where coalesce(storage_key,'') <> '' order by id limit 10" 2>/dev/null | tr -d '\r' || true)"
  missing=0
  checked=0
  while IFS= read -r k; do
    [ -n "$k" ] || continue
    checked=$((checked + 1))
    if ! compose exec -T backend sh -c 'test -f "$0"' "/app/data/$k"; then
      echo "  缺失：$k"
      missing=$((missing + 1))
    fi
  done <<< "$sample"
  if [ "$missing" -gt 0 ]; then
    die "产物抽样有 $missing/$checked 个文件缺失：恢复不完整，不要切流"
  fi
  ok "产物抽样通过（$checked 条）"
fi

# ---------- 5. 后续人工步骤 ----------
cat <<'EOF'

下一步（脚本故意不做，避免"自动跑完就以为完事"）：
  1. 恢复对象存储 / storagedata 卷（产物与上传件）——否则历史直链会 410
     （本地存储请用 --storage <tgz>，脚本会解包并抽样校验）
  2. 确认 APP_ENCRYPTION_KEY 与备份同源（不同源时，渠道密钥与媒体签名全部解不开）
  3. 启动 backend（会跑迁移），再抽查：管理台能登录、随机 20 条历史产物可下载
  4. 最后才切 DNS/反代
EOF

#!/usr/bin/env bash
# 备份：PostgreSQL dump + 9 项账务指纹 + 文件 sha256。
#
# 关键设计：pg_dump / psql 都在 **postgres 容器内**执行，宿主机不需要装客户端。
# （历史脚本要求宿主有 psql，而生产宿主机通常只有 Docker —— 等于备份从未在目标形态上跑过。）
#
# 不备份 Redis：它只存限流桶与 leader 锁，会话在 PG、配置轮询 DB。
# 恢复陈旧 AOF 会把旧 leader 锁带回来，制造"刚恢复就抢不到 leader"的假故障。
#
# 用法：bash deploy/scripts/backup.sh [--out DIR] [--keep-days N] [--with-storage|--without-storage] [--dsn DSN]
#
# 产物（/app/data）默认**自动判断**：active 为空或 none 时一并打包，
# 因为那是本机磁盘 —— 只备数据库的话，盘坏 = 产物全部丢失。
# 用云存储时默认不打包，只提示确认版本控制/跨区复制。

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
source "$(dirname "${BASH_SOURCE[0]}")/lib/compose.sh"

OUT_DIR="$DEPLOY_DIR/backups"
KEEP_DAYS=14
# auto：按 storage.active 判断（默认行为）；1：强制打包；0：强制跳过。
WITH_STORAGE=auto
DSN=""

while [ $# -gt 0 ]; do
  case "$1" in
    --out) OUT_DIR="$2"; shift 2 ;;
    --keep-days) KEEP_DAYS="$2"; shift 2 ;;
    --with-storage) WITH_STORAGE=1; shift ;;
    --without-storage) WITH_STORAGE=0; shift ;;
    --dsn) DSN="$2"; shift 2 ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) die "未知参数：$1" ;;
  esac
done

require_docker
[ -n "$DSN" ] || DSN="$(resolve_dsn)"

mkdir -p "$OUT_DIR"
ts="$(date +%Y%m%d-%H%M%S)"
dump="$OUT_DIR/lingdong-$ts.dump"
meta="$OUT_DIR/lingdong-$ts.verify.txt"

log "备份数据库 → $dump"
# 容器内的 pg_dump 与库同版本；-Fc 便于 pg_restore --clean 且支持并行
if ! compose exec -T postgres pg_dump "$DSN" --format=custom --no-owner --no-acl > "$dump"; then
  rm -f "$dump"
  die "pg_dump 失败（检查 DATABASE_URL 与 postgres 容器状态）"
fi
[ -s "$dump" ] || die "dump 为空：$dump"
ok "dump 完成（$(du -h "$dump" | awk '{print $1}')）"

log "采集账务指纹"
# 全新实例（业务表还没建）没有"账务"可对：此时指纹记为 empty，让发布能继续走，
# 但**仍然校验 dump 本身是真的 custom 格式归档**——否则"空备份"会被当成好备份。
#
# 为什么必须区分「空库」与「采集失败」：v2.1.3 之前两者都走 die，结果是**第一次部署
# 永远过不去**（upgrade 要求"无备份不升级"，而空库上备份必失败），运维只能 --no-backup
# 硬闯，正好绕开了唯一的那道保护。
has_core="$(compose exec -T postgres psql "$DSN" --no-psqlrc --tuples-only --no-align \
  -c "select count(*) from information_schema.tables where table_schema='public' and table_name in ('users','transactions','usages')" \
  2>/dev/null | tr -d '\r' | head -n1)"
if [ "${has_core:-0}" -ge 3 ] 2>/dev/null; then
  fingerprint="$(compose exec -T postgres psql "$DSN" --no-psqlrc --tuples-only --no-align \
    < "$SCRIPT_DIR/lib/fingerprint.sql" | tr -d '\r')"
  [ -n "$fingerprint" ] || die "指纹采集失败：备份文件已生成但**未校验**，不要把它当成可用备份"
else
  head -c 5 "$dump" | grep -q 'PGDMP' \
    || die "dump 不是有效的 PostgreSQL custom 归档：$dump（这份文件不能用来恢复）"
  fingerprint="empty"
  warn "目标库还没有业务表（全新实例）：指纹记为 empty。此时 data_epoch 不可用，"
  warn "  首次发布完成、业务表建好之后，请再跑一次 backup（或 tag）把真实指纹记账。"
fi

log "判定对象存储位置"
# active 为空或 none 都表示本地卷（registry.go 把两者都归到 localStore）。
storage_active="$(compose exec -T postgres psql "$DSN" --no-psqlrc --tuples-only --no-align \
  -c "select coalesce(value->>'active','') from system_config where key='storage'" 2>/dev/null \
  | tr -d '\r' | head -n1)"
[ -n "$storage_active" ] || storage_active="none"

if [ "$WITH_STORAGE" = "auto" ]; then
  case "$storage_active" in
    none)
      # 默认存储就是本机磁盘：只备数据库等于盘坏即丢全部产物。
      WITH_STORAGE=1
      ;;
    *)
      WITH_STORAGE=0
      warn "对象存储为 $storage_active：产物不在本地卷，本次只备数据库。"
      warn "  请确认该存储已开启版本控制 / 跨区复制，否则产物仍无第二份。"
      ;;
  esac
fi

{
  echo "backup_file = $(basename "$dump")"
  echo "created_at  = $(date '+%Y-%m-%d %H:%M:%S')"
  echo "size_bytes  = $(wc -c < "$dump" | tr -d ' ')"
  echo "keep_days   = $KEEP_DAYS"
  echo "fingerprint = $fingerprint"
  echo "sha256      = $(sha256_of "$dump")"
  echo "storage_active = $storage_active"
  echo "note        = APP_ENCRYPTION_KEY 不在此文件中，必须单独保管"
} > "$meta"
ok "指纹已写入 $(basename "$meta")"

if [ "$WITH_STORAGE" = "1" ]; then
  tgz="$OUT_DIR/lingdong-storage-$ts.tgz"
  log "打包本地文件副本（storagedata）→ $tgz"
  if compose exec -T backend tar czf - -C /app/data . > "$tgz"; then
    echo "storage_file = $(basename "$tgz")" >> "$meta"
    echo "storage_sha256 = $(sha256_of "$tgz")" >> "$meta"
    ok "storage 打包完成"
  else
    rm -f "$tgz"
    # 走到这里说明本次就是"本地存储"模式（云存储已在上面跳过）。
    # 本地模式下打包失败 = 产物没有任何备份，必须让整个备份失败，
    # 否则运维会拿着一份"成功"的报告，直到盘坏那天才发现产物全丢。
    die "storage 打包失败：当前是本地存储，产物未备份（检查 backend 容器是否在运行、/app/data 是否可读）"
  fi
fi

# 清理过期备份（只动自己命名规则的文件，且先打印再删）
if [ "$KEEP_DAYS" -gt 0 ]; then
  log "清理 ${KEEP_DAYS} 天前的备份"
  find "$OUT_DIR" -maxdepth 1 -name 'lingdong-*.dump' -mtime "+$KEEP_DAYS" -print -delete || true
  find "$OUT_DIR" -maxdepth 1 -name 'lingdong-*.verify.txt' -mtime "+$KEEP_DAYS" -print -delete || true
  find "$OUT_DIR" -maxdepth 1 -name 'lingdong-storage-*.tgz' -mtime "+$KEEP_DAYS" -print -delete || true
fi

echo
log "完成。指纹："
printf '%s\n' "$fingerprint"
warn "备份与数据库同盘时，盘坏 = 两份一起没。请把 dump + verify 复制到异地，并把 APP_ENCRYPTION_KEY 单独保管。"

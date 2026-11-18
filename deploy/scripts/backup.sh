#!/usr/bin/env bash
# 备份：PostgreSQL dump + 9 项账务指纹 + 文件 sha256。
#
# 关键设计：pg_dump / psql 都在 **postgres 容器内**执行，宿主机不需要装客户端。
# （历史脚本要求宿主有 psql，而生产宿主机通常只有 Docker —— 等于备份从未在目标形态上跑过。）
#
# 不备份 Redis：它只存限流桶与 leader 锁，会话在 PG、配置轮询 DB。
# 恢复陈旧 AOF 会把旧 leader 锁带回来，制造"刚恢复就抢不到 leader"的假故障。
#
# 用法：bash deploy/scripts/backup.sh [--out DIR] [--keep-days N] [--with-storage] [--dsn DSN]

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
source "$(dirname "${BASH_SOURCE[0]}")/lib/compose.sh"

OUT_DIR="$DEPLOY_DIR/backups"
KEEP_DAYS=14
WITH_STORAGE=0
DSN=""

while [ $# -gt 0 ]; do
  case "$1" in
    --out) OUT_DIR="$2"; shift 2 ;;
    --keep-days) KEEP_DAYS="$2"; shift 2 ;;
    --with-storage) WITH_STORAGE=1; shift ;;
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
fingerprint="$(compose exec -T postgres psql "$DSN" --no-psqlrc --tuples-only --no-align \
  < "$SCRIPT_DIR/lib/fingerprint.sql" | tr -d '\r')"
[ -n "$fingerprint" ] || die "指纹采集失败：备份文件已生成但**未校验**，不要把它当成可用备份"

{
  echo "backup_file = $(basename "$dump")"
  echo "created_at  = $(date '+%Y-%m-%d %H:%M:%S')"
  echo "size_bytes  = $(wc -c < "$dump" | tr -d ' ')"
  echo "keep_days   = $KEEP_DAYS"
  echo "fingerprint = $fingerprint"
  echo "sha256      = $(sha256_of "$dump")"
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
    warn "storage 打包失败（若已切云存储属正常：对象不在本地卷）"
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

#!/usr/bin/env bash
# 发布：构建并记录一个可回滚的版本（version / commit / image_id / data_epoch / previous）。
#
# 为什么需要它：单机编排的镜像标签来自 TAG，而"打哪个 tag"不能靠人手敲 ——
# 手敲就会出现"三台机器三个 tag"、"回滚时不知道上一版是什么"。本脚本把三件事固定下来：
#   1) 版本号只能来自源码或显式参数，且拒绝 local/latest；
#   2) 记录镜像 ID（不是 tag：tag 可以被覆盖，ID 不会）；
#   3) 记录 data_epoch（data_migrations 的「条数/名称哈希」）——它决定"能不能只回滚镜像"。
#      只跟踪**数据迁移**，不跟踪 DDL：AutoMigrate 只增列/增表，旧代码能忽略；
#      而数据迁移会改写存量行，回退镜像看见的是"已被改过的数据"。
#
# 用法：bash deploy/scripts/tag-release.sh [TAG] [--pull]
#
#   --pull  优先拉 GHCR 上的发布镜像（几十秒），拉不到再本地构建（3–8 分钟）。
#           一键更新的 agent 走这条；手工发版建议不带（验的是这份源码能不能构建）。

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
source "$(dirname "${BASH_SOURCE[0]}")/lib/compose.sh"

TAG_ARG=""
PULL=0
while [ $# -gt 0 ]; do
  case "$1" in
    # --pull：优先用发布镜像（几十秒），拉不到再本地构建（3–8 分钟）。
    # 一键更新的 agent 走这条路：宿主机不必有 Go/pnpm 依赖镜像可达的构建网络。
    --pull) PULL=1; shift ;;
    -*) die "未知参数：$1" ;;
    *)
      [ -z "$TAG_ARG" ] || die "只能给一个 TAG（多余参数：$1）"
      TAG_ARG="$1"; shift ;;
  esac
done
if [ -z "$TAG_ARG" ]; then
  TAG_ARG="$(grep -E '^var Version = ' "$ROOT_DIR/backend/internal/controller/health.go" \
    | sed -E 's/.*"(.*)".*/\1/')"
fi
[ -n "$TAG_ARG" ] || die "无法确定版本号：请显式传 TAG（例：bash deploy/scripts/tag-release.sh v2.1.1）"

case "$TAG_ARG" in
  local|latest) die "拒绝使用 $TAG_ARG 作为发布版本：它无法回答「现在跑的是哪一版」" ;;
esac

require_docker
require_cmd awk

IMG="$(app_image "$TAG_ARG")"
pulled=0
if [ "$PULL" = "1" ] && docker image inspect "$IMG" >/dev/null 2>&1; then
  # 本地已有同一 tag：直接复用。重复更新到同一版不该再花几分钟构建。
  log "本地已有 $IMG，跳过构建"
  pulled=1
elif [ "$PULL" = "1" ]; then
  log "拉取发布镜像 $IMG"
  if ( cd "$DEPLOY_DIR" && TAG="$TAG_ARG" docker compose -f "$(basename "$COMPOSE_FILE")" pull backend ); then
    pulled=1
  else
    warn "拉不到 $IMG（网络或仓库权限），改用本地构建"
  fi
fi
if [ "$pulled" = "0" ]; then
  log "构建镜像 tag=$TAG_ARG"
  ( cd "$DEPLOY_DIR" && TAG="$TAG_ARG" docker compose -f "$(basename "$COMPOSE_FILE")" build )
fi

backend_id="$(docker image inspect -f '{{.Id}}' "$IMG" 2>/dev/null || true)"
[ -n "$backend_id" ] || die "构建/拉取后找不到镜像 $IMG"

# data_epoch：DataMigration 集合的指纹（条数 + 名称哈希），拿不到就写 unknown，绝不编一个数字。
#
# 为什么不是 max(id)：data_migrations 的主键是 `name`（model/data_migration.go:14），
# **没有 id 列**。旧写法 `coalesce(max(id),0)` 必然报 42703，而错误被 2>/dev/null 吞掉 ——
# 于是 .release 里的 data_epoch 从上线起永远是 unknown，"数据迁移变了必须连带恢复数据"
# 这条保护也就从未生效。改成「条数/名称哈希」：它同时能发现"多了一条"和"换了一条"。
epoch="unknown"
if docker inspect -f '{{.State.Running}}' lingdong-postgres 2>/dev/null | grep -q true; then
  dsn="$(resolve_dsn)"
  epoch_err="$(mktemp)"
  row="$(compose exec -T postgres psql "$dsn" --no-psqlrc --tuples-only --no-align \
    -c "select count(*) || '/' || md5(coalesce(string_agg(name, ',' order by name), '')) from data_migrations" \
    2>"$epoch_err" | tr -d '\r' || true)"
  if [ -n "$row" ]; then
    epoch="$row"
  else
    # 不吞错：算不出来必须让人看见原因，而不是悄悄写 unknown 让回滚保护失效。
    warn "无法读取 data_epoch（将写 unknown）：$(tr -d '\r' < "$epoch_err" | head -3 | tr '\n' ' ')"
  fi
  rm -f "$epoch_err"
fi

previous=""
if [ -f "$DEPLOY_DIR/.release" ]; then
  previous="$(grep -E '^version=' "$DEPLOY_DIR/.release" | cut -d= -f2- || true)"
fi

# 构建对应的提交：回答"这一版到底是哪份代码"。镜像 ID 只说"同一个镜像"，不说"哪个提交"。
# 没有 .git（离线包解压的目录）时留空，不阻断发布。
commit="$(git -C "$ROOT_DIR" rev-parse HEAD 2>/dev/null || true)"

{
  echo "version=$TAG_ARG"
  echo "commit=$commit"
  echo "built_at=$(date '+%Y-%m-%d %H:%M:%S %z')"
  echo "image=$IMG"
  echo "image_id=$backend_id"
  echo "data_epoch=$epoch"
  echo "previous=$previous"
} > "$DEPLOY_DIR/.release"

{
  echo "---"
  echo "version=$TAG_ARG"
  echo "commit=$commit"
  echo "built_at=$(date '+%Y-%m-%d %H:%M:%S %z')"
  echo "data_epoch=$epoch"
  echo "previous=$previous"
} >> "$DEPLOY_DIR/.release.history"

ok "已写入 deploy/.release"
cat "$DEPLOY_DIR/.release"
echo
log "发布：TAG=$TAG_ARG bash deploy/scripts/upgrade.sh $TAG_ARG"
if [ -n "$previous" ]; then
  log "回滚：TAG=$previous bash deploy/scripts/upgrade.sh $previous   （data_epoch 变了则必须连带恢复数据）"
fi

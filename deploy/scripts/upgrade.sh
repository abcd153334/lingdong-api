#!/usr/bin/env bash
# 升级 / 回滚：把"某个已构建的版本"切上去，并等到它真的健康。
#
# 与 tag-release.sh 的分工：那边**构建并记账**，这边**只切版本**。
# 回滚时绝不能重新构建 —— 用当前源码重新构建一个旧 tag，会把"旧版本"变成"披着旧号的新代码"，
# 是最难发现的一类事故。
#
# 用法：bash deploy/scripts/upgrade.sh <TAG> [--no-backup] [--no-preflight] [--app-only]

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
source "$(dirname "${BASH_SOURCE[0]}")/lib/compose.sh"

TAG_ARG="${1-}"
shift 2>/dev/null || true
DO_BACKUP=1
DO_PREFLIGHT=1
APP_ONLY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --no-backup) DO_BACKUP=0; shift ;;
    --no-preflight) DO_PREFLIGHT=0; shift ;;
    # 只切应用容器：`up -d` 不带服务名会重新评估 postgres/redis。
    # 新 tag 一旦改动了这两个服务的编排段落（加环境变量、换镜像），
    # docker 就会**重建数据库容器** —— 在有连接的情况下重建 PG 是纯风险，
    # 且停机时间远超"切换应用"的几十秒。一键更新走这个开关。
    --app-only) APP_ONLY=1; shift ;;
    *) die "未知参数：$1" ;;
  esac
done

[ -n "$TAG_ARG" ] || die "用法：bash deploy/scripts/upgrade.sh <TAG>（TAG 由 tag-release.sh 产出）"
case "$TAG_ARG" in
  local|latest) die "拒绝 $TAG_ARG：发布与回滚必须用明确版本号" ;;
esac

require_docker

# 镜像必须已经存在：不存在说明"忘了构建"，此时 up -d 会拿到旧的同名镜像或直接失败
IMG="$(app_image "$TAG_ARG")"
if ! docker image inspect "$IMG" >/dev/null 2>&1; then
  die "找不到镜像 $IMG —— 先执行 tag-release.sh $TAG_ARG，或确认这是一次回滚（旧镜像被清理了就得重新构建同一份源码）"
fi

if [ "$DO_PREFLIGHT" = "1" ]; then
  bash "$SCRIPT_DIR/preflight.sh" || die "preflight 未通过，已中止升级"
fi

if [ "$DO_BACKUP" = "1" ]; then
  log "升级前备份（失败即中止）"
  bash "$SCRIPT_DIR/backup.sh" || die "备份失败，已中止升级（无备份不升级）"
fi

prev=""
if [ -f "$DEPLOY_DIR/.release" ]; then
  prev="$(grep -E '^previous=' "$DEPLOY_DIR/.release" | cut -d= -f2- || true)"
fi
epoch_now="$(grep -E '^data_epoch=' "$DEPLOY_DIR/.release" 2>/dev/null | cut -d= -f2- || true)"

if [ "$APP_ONLY" = "1" ]; then scope="仅应用容器"; else scope="全量"; fi
log "切换镜像 → TAG=$TAG_ARG（$scope）"
if [ "$APP_ONLY" = "1" ]; then
  compose_tag "$TAG_ARG" up -d --no-deps backend
else
  compose_tag "$TAG_ARG" up -d
fi

log "等待健康（最多 120s）"
# 必须带 --expect-version 与 --require-db —— 这是 healthcheck.sh 头部写明的约定，
# 而调用点此前一直没传，等于把门禁做成了摆设：
#   - 不带 --expect-version：`up -d` 没真正换掉镜像（或被同名旧镜像顶替）时旧进程照样 200，
#     于是「已切到 vX」是个谎；真机上 v2.1.2 后端一直在崩溃重启，检查却报 PASS；
#   - 不带 --require-db：把「能返回 200」当成「DB 真能用」。
if bash "$SCRIPT_DIR/healthcheck.sh" --wait 120 --expect-version "$TAG_ARG" --require-db; then
  ok "已切到 $TAG_ARG"
else
  warn "新版本未通过健康检查"
  cat <<EOF

立即回滚（二选一）：
  A. data_epoch 未变（本次只改了代码）：
     TAG=${prev:-<上一版>} bash deploy/scripts/upgrade.sh ${prev:-<上一版>} --no-backup
  B. data_epoch 已变（跑过数据迁移）：
     仅回退镜像不够，必须连带恢复升级前的备份（restore.sh 走 upgrade 前的快照）
当前记录：data_epoch=${epoch_now:-unknown}
EOF
  exit 1
fi

echo
log "版本信息"
cat "$DEPLOY_DIR/.release" 2>/dev/null || warn "没有 .release（这次不是通过 tag-release 发布的？）"

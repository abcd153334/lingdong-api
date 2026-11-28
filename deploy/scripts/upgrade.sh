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
# 期望版本号默认等于 TAG。回滚时必须能分开：回退镜像的 tag 是 revert-<job>，
# 而它跑出来的 /api/health.version 是 PREV_TAG（v2.2.6）。以前两者绑死，
# 结果是"回滚切换成功、健康校验必失败"——回滚成了必然报错的一条路。
EXPECT_VER=""
while [ $# -gt 0 ]; do
  case "$1" in
    --no-backup) DO_BACKUP=0; shift ;;
    --no-preflight) DO_PREFLIGHT=0; shift ;;
    --expect-version) EXPECT_VER="${2-}"; shift 2 ;;
    # 只切应用容器：`up -d` 不带服务名会重新评估 postgres/redis。
    # 新 tag 一旦改动了这两个服务的编排段落（加环境变量、换镜像），
    # docker 就会**重建数据库容器** —— 在有连接的情况下重建 PG 是纯风险，
    # 且停机时间远超"切换应用"的几十秒。一键更新走这个开关。
    --app-only) APP_ONLY=1; shift ;;
    *) die "未知参数：$1" ;;
  esac
done
EXPECT_VER="${EXPECT_VER:-$TAG_ARG}"

[ -n "$TAG_ARG" ] || die "用法：bash deploy/scripts/upgrade.sh <TAG>（TAG 由 tag-release.sh 产出）"
case "$TAG_ARG" in
  local|latest) die "拒绝 $TAG_ARG：发布与回滚必须用明确版本号" ;;
esac

require_docker

# 镜像与源码/编排是两套东西：upgrade 只切**镜像**，而 compose、.env.example 随 tag 变。
# 只切镜像的结果是"跑着 v2.3.1、编排还是 v2.2.14"——新版本新增的环境变量不会传进容器
# （2026-11-28 从 v2.2.14 引导升到 v2.3.1 实测踩到：LD_BUILTIN_PRESETS 没生效，
#  必须手工 checkout 才补上）。
#
# 这里**不自动 checkout**：本脚本就在这个工作区里，执行中改写自己会让 bash 读到半截语句
# （同一原因让执行器副本住在 /opt/lingdong/updater）。只把偏差说清楚，由人做那一步。
if [ -d "$DEPLOY_DIR/../.git" ]; then
  cur_tag="$(git -C "$DEPLOY_DIR/.." describe --tags --exact-match 2>/dev/null || true)"
  if [ "$cur_tag" != "$TAG_ARG" ]; then
    warn "源码/编排未对齐：当前 checkout=${cur_tag:-无 tag}，目标=$TAG_ARG"
    cat <<EOF
  镜像会切到 $TAG_ARG，但 deploy/ 下的编排与脚本仍是当前 checkout 的版本。
  若要新编排生效（新增的环境变量、端口、依赖都在这层），先在 $(cd "$DEPLOY_DIR/.." && pwd) 执行：
    git fetch --tags --force && git checkout $TAG_ARG
  然后再执行一次本命令（幂等，会再备份一次）。

EOF
  fi
fi

# 镜像必须存在，否则 `up -d` 会拿旧的同名镜像或直接失败。
# 本地没有就先试着拉发布镜像：升级/回滚的常规姿势本来就是"拉一个已发布的版本"，
# 以前这里直接 die，用户得先手动 `TAG=… docker compose pull` 才能升级 —— README
# 里也只能写成两步。拉不到（离线 / 私有仓库 / 该 tag 从未发布）再报错并给本地构建的出路。
IMG="$(app_image "$TAG_ARG")"
if ! docker image inspect "$IMG" >/dev/null 2>&1; then
  log "本地没有 $IMG，尝试从镜像仓库拉取"
  if ( cd "$DEPLOY_DIR" && TAG="$TAG_ARG" docker compose -f "$COMPOSE_FILE" pull backend ); then
    docker image inspect "$IMG" >/dev/null 2>&1 \
      || die "已执行 pull，但镜像 $IMG 仍不存在（检查编排里的 LD_IMAGE 与 TAG 是否与本次一致）"
  else
    die "拉不到镜像 $IMG —— 离线环境请先执行 tag-release.sh $TAG_ARG 构建同一份源码；本次未做任何变更"
  fi
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
if bash "$SCRIPT_DIR/healthcheck.sh" --wait 120 --expect-version "$EXPECT_VER" --require-db; then
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

# 记账：upgrade 以前只切镜像、不写 .release，于是**窗口期最长的两条路**（手工升级与
# 一键更新）走完之后，.release 仍停在上一版：agent 记的"回退点"指向更早的版本，
# 界面上的版本信息是错的，data_epoch 保护也对着一份过期基准在比。
# 复用 tag-release.sh 的写法，不在这里重算一遍（data_epoch 的算法只该有一处）。
recorded="$(grep -E '^version=' "$DEPLOY_DIR/.release" 2>/dev/null | cut -d= -f2- || true)"
if [ "$recorded" != "$TAG_ARG" ]; then
  if bash "$SCRIPT_DIR/tag-release.sh" "$TAG_ARG" --record-only >/dev/null 2>&1; then
    ok "已记账 deploy/.release（${recorded:-空} → $TAG_ARG）"
  else
    warn "写 deploy/.release 失败（不影响运行，但回退点会不准）；可手动补：bash deploy/scripts/tag-release.sh $TAG_ARG --record-only"
  fi
fi

echo
log "版本信息"
cat "$DEPLOY_DIR/.release" 2>/dev/null || warn "没有 .release（这次不是通过 tag-release 发布的？）"

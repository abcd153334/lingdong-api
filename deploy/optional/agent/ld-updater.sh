#!/usr/bin/env bash
# ld-updater —— 宿主机上的「一键更新」执行器。
#
# 它**不在仓库里运行**：install.sh 会把它复制到 /opt/lingdong/updater/。
# 原因很具体：一次更新会 `git checkout` 整个工作区，而 bash 是按下标边读边执行的，
# 脚本文件在执行途中被改写会读到半截语句（经典事故）。仓库里的副本只是发布源。
#
# 它也**不 source 仓库里的任何文件**（lib/common.sh 之类）：那些文件会随版本变，
# 而正在跑的 agent 用的是旧版本 —— 依赖它们等于把"新旧混用"写进设计。
# 仓库只提供被调用的脚本（preflight/tag-release/backup/upgrade/healthcheck），
# 以子进程方式执行：它们读到的就是刚检出的那一版，这是期望行为。
#
# 与后端的契约：唯一真相是 PostgreSQL 的 release_update_jobs（见
# backend/internal/service/releaseupdate）。本脚本只做四件事：
#   读写任务行、执行固定命令序列、把结果写回、把回滚目标先记下来。
#
# 用法：
#   ld-updater.sh                 常驻（systemd 拉起）
#   ld-updater.sh --dry-run --tag v2.2.0   只打印将要执行的命令，不碰 DB / 不碰容器

set -euo pipefail

CONF="${CONF:-/etc/lingdong/updater.conf}"
# shellcheck disable=SC1090
[ -f "$CONF" ] && . "$CONF"

REPO_PATH="${REPO_PATH:-/opt/lingdong/app}"
INTERVAL="${INTERVAL:-5}"
LOCK_FILE="${LOCK_FILE:-/var/lock/ld-updater.lock}"
# 探活地址：走**对外入口端口**（宿主 127.0.0.1:8088 → web → backend），
# 与用户真实链路一致。比探 backend 的 8080 更能反映"这次切换到底成没成"：
# nginx 配置写错、上游指错服务，在 8080 上完全看不出来。
# 换端口不必改这里 —— 默认读编排的 APP_PORT，读不到就退回 8088。
HEALTH_URL="${HEALTH_URL:-}"
AGENT_VERSION="1"

DRY=0
DRY_TAG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1; shift ;;
    --tag) DRY_TAG="${2-}"; shift 2 ;;
    --repo) REPO_PATH="${2-}"; shift 2 ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done

ts() { date '+%Y-%m-%d %H:%M:%S'; }
log() { printf '[%s] %s\n' "$(ts)" "$*"; }
warn() { printf '[%s] WARN %s\n' "$(ts)" "$*" >&2; }
die() { printf '[%s] FAIL %s\n' "$(ts)" "$*" >&2; exit 1; }

# run 是所有"有副作用命令"的唯一入口：--dry-run 下只打印。
run() {
  if [ "$DRY" = "1" ]; then
    printf '  DRY-RUN %s\n' "$*"
    return 0
  fi
  "$@"
}

# ---------------------------------------------------------------------------
# 配置与数据库访问
# ---------------------------------------------------------------------------

ENV_FILE="$REPO_PATH/deploy/.env"
COMPOSE_FILE="$REPO_PATH/deploy/docker-compose.yml"

# read_env 与 deploy/scripts/lib/common.sh 同规则（不 source：.env 里可能有 $ 与空格）。
read_env() {
  local key="$1" default="${2-}" line
  [ -f "$ENV_FILE" ] || { printf '%s' "$default"; return 0; }
  line="$(grep -E "^[[:space:]]*${key}=" "$ENV_FILE" | tail -n 1 || true)"
  [ -n "$line" ] || { printf '%s' "$default"; return 0; }
  line="${line#*=}"
  line="${line%$'\r'}"
  case "$line" in
    \"*\") line="${line#\"}"; line="${line%\"}" ;;
    \'*\') line="${line#\'}"; line="${line%\'}" ;;
  esac
  printf '%s' "$line"
}

# 镜像名必须与 deploy/scripts/lib/common.sh:app_image 同规则，但**不能 source** 它
# （见文件头：仓库文件会随版本变，而正在跑的 agent 是旧版）。代价是这里内联一份。
#
# 这一份是 v2.2.0 改形态时漏掉的：前端内嵌后 web 容器被删、镜像统一成
# ghcr.io/<owner>/lingdong-api:<tag>，而本脚本还在找 lingdong-api-backend /
# lingdong-api-web。症状不是"更新失败"，是第 4 步就 rollback_unavailable 退出 ——
# 一次更新都跑不成，且看起来像"回退镜像被清理了"。
APP_IMAGE_REPO="ghcr.io/abcd153334/lingdong-api"

app_repo() {
  local explicit
  explicit="$(read_env LD_IMAGE '')"
  if [ -n "$explicit" ]; then
    case "$explicit" in
      *:*) printf '%s' "${explicit%:*}" ;;
      *)   printf '%s' "$explicit" ;;
    esac
    return 0
  fi
  printf '%s' "$APP_IMAGE_REPO"
}

app_image() { printf '%s:%s' "$(app_repo)" "${1:-latest}"; }

# db：执行一条 SQL，返回制表符分隔的行。
# 走容器内的 psql：宿主机不需要装客户端（生产宿主机通常只有 Docker）。
db() {
  local sql="$1"; shift
  ( cd "$REPO_PATH/deploy" && \
    docker compose -f "$(basename "$COMPOSE_FILE")" exec -T \
      -e PGPASSWORD="$PG_PASS" postgres \
      psql -U "$PG_USER" -d "$PG_DB" --no-psqlrc --no-align --tuples-only \
           --field-separator=$'\t' "$@" -c "$sql" )
}

load_db_conf() {
  [ -f "$ENV_FILE" ] || die "找不到 $ENV_FILE（生产目录应是独立 clone，且已按 .env.example 生成配置）"
  PG_USER="$(read_env POSTGRES_USER 'lingdong')"
  PG_DB="$(read_env POSTGRES_DB 'lingdong')"
  PG_PASS="$(read_env POSTGRES_PASSWORD '')"
  [ -n "$PG_PASS" ] || die "POSTGRES_PASSWORD 为空：无法访问任务表"
}

# ---------------------------------------------------------------------------
# 任务行写入
# ---------------------------------------------------------------------------

# note STATUS STEP PROGRESS MESSAGE
# 消息会进管理台与审计：必须去换行、截断，否则一行 JSON 会变成两行 SQL 参数。
note() {
  local st="$1" step="$2" pr="$3" msg="${4//$'\n'/ }"
  msg="${msg:0:200}"
  db "UPDATE release_update_jobs
         SET status=:'st', step=:'step', progress=:'pr', message=:'msg', last_heartbeat_at=now()
       WHERE id=:'id'" \
     -v st="$st" -v step="$step" -v pr="$pr" -v msg="$msg" -v id="$JOB_ID" >/dev/null
}

# finish OK|FAIL|MANUAL CODE MESSAGE [ROLLBACK_RESULT]
finish() {
  local kind="$1" code="$2" msg="${3//$'\n'/ }" rb="${4-}"
  msg="${msg:0:200}"
  local status="success"
  case "$kind" in
    OK) status="success" ;;
    FAIL) status="failed" ;;
    MANUAL) status="manual_required" ;;
    *) status="$kind" ;;
  esac
  local prog=100
  db "UPDATE release_update_jobs
         SET status=:'st', step=:'st', progress=:'pr', message=:'msg',
             failure_code=NULLIF(:'code',''), rollback_result=NULLIF(:'rb',''),
             last_heartbeat_at=now(), finished_at=now()
       WHERE id=:'id'" \
     -v st="$status" -v pr="$prog" -v msg="$msg" -v code="$code" -v rb="$rb" -v id="$JOB_ID" >/dev/null
}

# ---------------------------------------------------------------------------
# 心跳（独立子进程）
# ---------------------------------------------------------------------------

HB_PID=""
start_heartbeat() {
  (
    while :; do
      # 构建是 3–8 分钟的阻塞调用，主循环在这期间刷新不了心跳；
      # 不独立出来的话，后端会在第 90 秒把任务判成"失联"，而机器正在正常构建。
      db "UPDATE release_update_jobs SET last_heartbeat_at=now() WHERE id=:'id'" -v id="$JOB_ID" >/dev/null 2>&1 || true
      sleep 10
    done
  ) &
  HB_PID=$!
}
stop_heartbeat() {
  if [ -n "${HB_PID:-}" ]; then
    kill "$HB_PID" 2>/dev/null || true
    wait "$HB_PID" 2>/dev/null || true
    HB_PID=""
  fi
}

# ---------------------------------------------------------------------------
# 只读事实
# ---------------------------------------------------------------------------

# data_epoch：数据迁移集合的指纹。**直接查库**，不读 deploy/.release ——
# 那个文件在切换前就被 tag-release 改写成新版本，用它比前后永远得出"没变"。
data_epoch() {
  db "select count(*) || '/' || md5(coalesce(string_agg(name, ',' order by name), '')) from data_migrations" 2>/dev/null | tr -d '\r'
}

running_version() {
  local url="$HEALTH_URL"
  if [ -z "$url" ]; then
    local port=""
    # 从 .env 读 APP_PORT（与 compose 同一个变量；旧文件里可能还是 WEB_PORT），
    # 再退回编排默认值 8088。
    if [ -f "$ENV_FILE" ]; then
      port="$(grep -E '^APP_PORT=' "$ENV_FILE" 2>/dev/null | tail -n1 | cut -d= -f2- | tr -d '\r "')"
      [ -n "$port" ] || port="$(grep -E '^WEB_PORT=' "$ENV_FILE" 2>/dev/null | tail -n1 | cut -d= -f2- | tr -d '\r "')"
    fi
    url="http://127.0.0.1:${port:-8088}/api/health"
  fi
  curl -sS --max-time 3 "$url" 2>/dev/null \
    | tr -d '\n' | sed -n 's/.*"version":"\([^"]*\)".*/\1/p'
}

release_field() { # key
  [ -f "$REPO_PATH/deploy/.release" ] || return 0
  grep -E "^$1=" "$REPO_PATH/deploy/.release" | cut -d= -f2- || true
}

agent_seen() {
  local tag; tag="$(release_field version)"
  db "INSERT INTO release_agent_state (id, host, version, repo_path, current_tag, seen_at)
      VALUES (1, :'host', :'ver', :'repo', :'tag', now())
      ON CONFLICT (id) DO UPDATE
        SET host=excluded.host, version=excluded.version, repo_path=excluded.repo_path,
            current_tag=excluded.current_tag, seen_at=now()" \
     -v host="$(hostname)" -v ver="$AGENT_VERSION" -v repo="$REPO_PATH" -v tag="$tag" >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
# 领取
# ---------------------------------------------------------------------------

# claim 原子领取一条 queued 任务；没有则输出空。
# UPDATE ... WHERE id=(SELECT ... FOR UPDATE SKIP LOCKED) 一条语句完成，
# 不依赖 flock（flock 只防同机两个进程，DB 约束防的是所有情况）。
claim() {
  db "UPDATE release_update_jobs
         SET status='fetching', step='fetching', progress=5,
             agent_host=:'host', started_at=now(), last_heartbeat_at=now(),
             attempt=attempt+1
       WHERE id = (
         SELECT id FROM release_update_jobs
          WHERE status='queued'
          ORDER BY id
          LIMIT 1
          FOR UPDATE SKIP LOCKED
       )
       RETURNING id, target_tag, coalesce(target_commit,''), coalesce(request_id,'')" \
     -v host="$(hostname)" 2>/dev/null | tr -d '\r'
}

# ---------------------------------------------------------------------------
# 更新流程
# ---------------------------------------------------------------------------

# 切换前失败：把检出恢复到"正在跑的那一版"，不让工作和运行的代码偏离。
restore_checkout() {
  if [ -n "${PREV_COMMIT:-}" ]; then
    git -C "$REPO_PATH" checkout --detach "$PREV_COMMIT" >/dev/null 2>&1 \
      || warn "恢复检出到 $PREV_COMMIT 失败：生产目录现在停在新 tag 上，但容器仍是旧版"
  fi
  if [ "${RELEASE_WRITTEN:-0}" = "1" ] && [ -f "${RELEASE_BAK:-}" ]; then
    cp -f "$RELEASE_BAK" "$REPO_PATH/deploy/.release" || warn "恢复 .release 失败"
  fi
}

fail_pre_switch() { # code message
  restore_checkout
  finish FAIL "$1" "$2"
}

do_job() {
  PREV_COMMIT=""
  PREV_TAG=""
  RELEASE_BAK=""
  RELEASE_WRITTEN=0

  # 1) tag 形状（与后端同一套正则：它会被拼进 git / docker 参数）
  if ! [[ "$TARGET_TAG" =~ ^v?[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}([-+][0-9A-Za-z.]+)?$ ]]; then
    finish FAIL tag_invalid "目标版本格式不合法，已拒绝"
    return
  fi

  # 2) 取代码并确认 tag 真实存在（"任意 tag"的最终闸门在这里，不在后端）
  note fetching fetching 8 "拉取版本信息"
  if ! run timeout 180 git -C "$REPO_PATH" fetch --tags --prune --force origin; then
    finish FAIL fetch_failed "拉取代码失败（检查部署机的仓库凭证与网络）"
    return
  fi
  local commit
  if ! commit="$(git -C "$REPO_PATH" rev-parse -q --verify "refs/tags/$TARGET_TAG^{commit}")"; then
    finish FAIL tag_not_found "远端没有这个 tag，已拒绝"
    return
  fi
  if [ -n "$TARGET_COMMIT" ]; then
    case "$commit" in
      "$TARGET_COMMIT"*) ;;
      *) finish FAIL commit_mismatch "tag 指向的提交与清单声明不一致（tag 可能被移动过）"; return ;;
    esac
  fi

  # 3) 工作区必须干净：更新会 checkout，脏工作区上的本地改动会被静默丢弃
  local dirty
  dirty="$(git -C "$REPO_PATH" status --porcelain -uall || true)"
  if [ -n "$dirty" ]; then
    finish FAIL dirty_worktree "生产目录有未提交改动，已拒绝（先处理：$(printf '%s' "$dirty" | head -3 | tr '\n' ' ')）"
    return
  fi

  # 4) 记回退点：切换前一秒才知道要回退到哪，就已经晚了。
  #    记的是**镜像 ID**（tag 可被覆盖，ID 不会），并在回退时按 ID 重新 tag 一份。
  PREV_COMMIT="$(git -C "$REPO_PATH" rev-parse HEAD)"
  local prev_backend epoch_before
  PREV_TAG="$(release_field version)"
  prev_backend="$(docker image inspect -f '{{.Id}}' "$(app_image "${PREV_TAG:-none}")" 2>/dev/null || true)"
  epoch_before="$(data_epoch)"
  if [ -z "$prev_backend" ] || [ -z "$epoch_before" ]; then
    finish FAIL rollback_unavailable "无法确定回退目标（缺上一版镜像或读不到数据库），为避免无路可退，本次不切换"
    return
  fi
  db "UPDATE release_update_jobs
         SET rollback_ref=:'ref', rollback_kind='image', data_epoch_before=:'ep'
       WHERE id=:'id'" \
     -v ref="$prev_backend" -v ep="$epoch_before" -v id="$JOB_ID" >/dev/null

  # 5) 磁盘：每次构建会多一套镜像（各 1–2GB），空间不足时构建中途失败最难收拾
  local droot avail_kb
  droot="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)"
  avail_kb="$(df -Pk "$droot" | awk 'NR==2{print $4}')"
  if [ "${avail_kb:-0}" -lt $((12 * 1024 * 1024)) ]; then
    finish FAIL disk_low "Docker 数据盘可用空间不足 12GB，已中止（清理旧镜像后再试）"
    return
  fi

  # 6) 检出到 tag（不加 -f：脏工作区已经在上面拦下了）
  note fetching checkout 10 "检出 $TARGET_TAG"
  if ! run git -C "$REPO_PATH" checkout --detach "refs/tags/$TARGET_TAG"; then
    finish FAIL checkout_failed "检出失败"
    return
  fi

  # 7) 体检（.env / 端口 / 必填项）
  note building preflight 15 "环境体检"
  if ! run bash "$REPO_PATH/deploy/scripts/preflight.sh"; then
    fail_pre_switch preflight_failed "环境体检未通过，已恢复原检出"
    return
  fi

  # 8) 准备镜像：**优先拉预构建镜像**（几十秒），拉不到才本地构建（3–8 分钟）。
  #    只走 tag-release.sh 一个入口 —— 它同时负责写 .release（version/commit/
  #    image_id/data_epoch/previous）。在这里另写一份记账 = 两套真相。
  note building build 20 "准备镜像（优先拉预构建，几十秒；否则本地构建 3–8 分钟）"
  RELEASE_BAK="$(mktemp)"
  [ -f "$REPO_PATH/deploy/.release" ] && cp -f "$REPO_PATH/deploy/.release" "$RELEASE_BAK" || true
  RELEASE_WRITTEN=1
  if ! run bash "$REPO_PATH/deploy/scripts/tag-release.sh" "$TARGET_TAG" --pull; then
    fail_pre_switch build_failed "镜像准备失败（拉取与本地构建都失败），已恢复原检出"
    return
  fi
  note building built 60 "镜像已就绪"

  # 9) 备份（放在构建之后：先备份会让备份比切换早 8 分钟，这段时间账还在动）
  note backing_up backup 65 "备份数据库"
  if ! run bash "$REPO_PATH/deploy/scripts/backup.sh" --out "$REPO_PATH/deploy/backups"; then
    fail_pre_switch backup_failed "备份失败，已中止（无备份不升级）"
    return
  fi

  # 10) 切换（只动应用容器：不带 --app-only 会重新评估 pg/redis）
  note switching switch 75 "切换容器（服务将中断 30–60 秒）"
  if ! run bash "$REPO_PATH/deploy/scripts/upgrade.sh" "$TARGET_TAG" \
        --app-only --no-backup --no-preflight; then
    rollback "switch_failed" "新版本启动失败"
    return
  fi

  # 11) 校验：必须"版本对 + DB 可用"，浅探 200 不算成功（旧进程也会 200）
  note verifying verify 90 "校验版本与依赖"
  if ! run bash "$REPO_PATH/deploy/scripts/healthcheck.sh" \
        --wait 120 --expect-version "$TARGET_TAG" --require-db; then
    rollback "verify_failed" "新版本未通过校验"
    return
  fi

  # 12) 数据纪元：变了说明跑过数据迁移，回退镜像会得到"旧代码 + 已改过的数据"
  local epoch_after
  epoch_after="$(data_epoch)"
  db "UPDATE release_update_jobs SET data_epoch_after=:'ep' WHERE id=:'id'" \
     -v ep="$epoch_after" -v id="$JOB_ID" >/dev/null || true
  if [ "$epoch_after" != "$epoch_before" ]; then
    finish MANUAL data_epoch_changed "更新已完成，但期间执行了数据迁移；如需回退必须连带恢复备份（不能只回退镜像）" "skipped"
    return
  fi

  finish OK "" "已更新到 $TARGET_TAG" "skipped"
  prune_images "$TARGET_TAG" "$PREV_TAG"
}

# rollback CODE MESSAGE
#
# 只试一次：回滚失败再回滚没有意义，且会把状态搅乱。失败即转人工。
rollback() {
  local code="$1" msg="$2"
  note rolling_back rollback 95 "校验未通过，回滚到上一版"

  local epoch_now
  epoch_now="$(data_epoch)"
  local epoch_before; epoch_before="$(db "select coalesce(data_epoch_before,'') from release_update_jobs where id=:'id'" -v id="$JOB_ID" | tr -d '\r')"
  if [ -n "$epoch_before" ] && [ -n "$epoch_now" ] && [ "$epoch_now" != "$epoch_before" ]; then
    finish MANUAL data_epoch_changed "新版本已执行数据迁移，回退镜像会得到「旧代码 + 新数据」，需人工处理" "skipped"
    return
  fi

  # 回退镜像：优先用"上一版 tag"（镜像通常还在，prune 保留最近 4 个）；
  # 不在了就按第 4 步记下的镜像 ID 重新 tag 一份。
  # 两个名字都必须由 app_image 推导 —— 旧实现写死 lingdong-api-backend 与
  # lingdong-api-web，在 v2.2.0 之后就再也找不到镜像，回滚必然 rollback_unavailable。
  local rb_tag="$PREV_TAG" rb_ver="$PREV_TAG"
  if [ -z "$rb_tag" ] || ! docker image inspect "$(app_image "$rb_tag")" >/dev/null 2>&1; then
    rb_tag="revert-$JOB_ID"
    docker tag "$(db "select coalesce(rollback_ref,'') from release_update_jobs where id=:'id'" -v id="$JOB_ID" | tr -d '\r')" \
      "$(app_image "$rb_tag")" 2>/dev/null || true
  fi
  if ! docker image inspect "$(app_image "$rb_tag")" >/dev/null 2>&1; then
    finish MANUAL rollback_unavailable "$msg；回退镜像已被清理，无法自动回滚" "failed"
    return
  fi

  # --expect-version 必须传上一版的版本号：rb_tag 在 fallback 分支里是 revert-<id>，
  # 拿它当期望版本会让健康校验永远不通过（而那时切换其实已经成功了）。
  if ! bash "$REPO_PATH/deploy/scripts/upgrade.sh" "$rb_tag" \
        --expect-version "$rb_ver" --app-only --no-backup --no-preflight; then
    finish MANUAL rollback_failed "$msg；回滚切换也失败，请立即人工介入" "failed"
    return
  fi

  # .release 在切换前已被写成新版本；不还原的话，下次升级会拿这个错值当 previous。
  restore_checkout
  finish rolled_back "$code" "$msg，已自动回滚到上一版" "ok"
}

# prune_images CURRENT PREV：保留"当前 + 上一版 + 最近 2 个"，其余删掉。
#
# 为什么不用 `docker image prune -f --filter until=24h`：它只删 dangling 镜像，
# 而发布镜像都带 tag，永远不会被它删 —— 磁盘会一路涨到构建失败。
prune_images() {
  local cur="$1" prev="$2" repo tag
  repo="$(app_repo)"
  # 按创建时间倒序，只保留最近 4 个（当前 + 上一版 + 2 个余量）。
  # 不用 `docker image prune --filter until=24h`：它只删 dangling 镜像，
  # 发布镜像都带 tag，永远不会被它删掉 —— 磁盘会一路涨到构建失败。
  while read -r tag; do
    [ -n "$tag" ] || continue
    case "$tag" in
      "<none>"|"$cur"|"$prev") continue ;;
    esac
    docker rmi "$repo:$tag" >/dev/null 2>&1 || true
  done < <(docker images --format '{{.CreatedAt}}|{{.Tag}}' "$repo" 2>/dev/null \
             | sort -r | awk -F'|' 'NR>4{print $2}')
  docker image prune -f >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
# 主循环
# ---------------------------------------------------------------------------

dry_run_plan() {
  cat <<EOF
DRY-RUN：将按以下顺序执行（不碰数据库、不碰容器）
  repo            = $REPO_PATH
  target tag      = ${DRY_TAG:-<从任务表领取>}
  1  git fetch --tags --prune --force origin
  2  git rev-parse --verify refs/tags/<tag>^{commit}
  3  git status --porcelain -uall           （必须为空）
  4  记录 rollback_ref(镜像 ID) / data_epoch_before
  5  df \$(docker info --format '{{.DockerRootDir}}')  （≥12GB）
  6  git checkout --detach refs/tags/<tag>
  7  bash deploy/scripts/preflight.sh
  8  bash deploy/scripts/tag-release.sh <tag> --pull   （先拉预构建，拉不到再构建）
  9  bash deploy/scripts/backup.sh --out deploy/backups
 10  bash deploy/scripts/upgrade.sh <tag> --app-only --no-backup --no-preflight
 11  bash deploy/scripts/healthcheck.sh --wait 120 --expect-version <tag> --require-db
 12  比较 data_epoch（变了则转人工，不自动回滚）
 13  保留 当前/上一版/最近2个 镜像
EOF
}

main() {
  [ "$DRY" = "1" ] && { dry_run_plan; exit 0; }

  command -v docker >/dev/null 2>&1 || die "缺少 docker"
  command -v git >/dev/null 2>&1 || die "缺少 git"
  load_db_conf
  [ -d "$REPO_PATH/.git" ] || die "$REPO_PATH 不是 git 工作区（生产目录应是独立 clone）"

  # 同机只允许一个实例：DB 的部分唯一索引管"全局一个更新"，flock 管"本机别开两个进程"。
  exec 9>"$LOCK_FILE"
  flock -n 9 || die "已有另一个 ld-updater 在运行"

  log "启动：repo=$REPO_PATH interval=${INTERVAL}s"
  trap 'stop_heartbeat' EXIT

  local last_seen=0
  while :; do
    # 心跳表每 15s 刷一次即可（后端用它回答"这台机器装没装更新程序"）
    if [ $(( $(date +%s) - last_seen )) -ge 15 ]; then
      agent_seen
      last_seen=$(date +%s)
    fi

    local row=""
    if ! row="$(claim)"; then
      warn "领取任务失败（数据库不可用？）：5 秒后重试"
      sleep "$INTERVAL"
      continue
    fi
    if [ -z "$row" ]; then
      sleep "$INTERVAL"
      continue
    fi

    JOB_ID=""; TARGET_TAG=""; TARGET_COMMIT=""; REQUEST_ID=""
    IFS=$'\t' read -r JOB_ID TARGET_TAG TARGET_COMMIT REQUEST_ID <<< "$row"
    log "领取任务 #$JOB_ID → $TARGET_TAG"
    start_heartbeat
    # 刻意**不**写成 `if ! do_job`：那会让 do_job 内部的所有命令失去 set -e
    # （bash 对 if 条件里的函数关闭 errexit），中途失败会带着错状态继续往下执行。
    # 这里让它照常跑：意外失败 → 进程退出 → systemd 重启 → 后端按心跳把任务判死。
    do_job
    stop_heartbeat
    log "任务 #$JOB_ID 结束"
  done
}

main "$@"

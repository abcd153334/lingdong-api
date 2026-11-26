#!/usr/bin/env bash
# release.sh —— 一条命令把改动变成一个新版本。
#
# 为什么要它：发一次版本本来是五步（add / commit / push / tag / push tag），
# 其中两个坑新手必踩 —— "忘了 push 就 tag"（发出去一个云端不存在的版本）、
# "tag 打错号"（用户端版本号永远是错的，且改不回来）。这里把五步压成一步，
# 顺序写死，任何一步失败就停，不往下走。
#
# 用法（在仓库任意位置都行，脚本会自己 cd 到根目录）：
#   bash scripts/release.sh "修登录页按钮错位"        # 存档 + 发小版本（v2.2.14 → v2.2.15）
#   bash scripts/release.sh "新增批量导入" --minor    # 发中版本（v2.2.14 → v2.3.0）
#   bash scripts/release.sh "接口重构" --major        # 发大版本（v2.2.14 → v3.0.0）
#   bash scripts/release.sh "改了点东西" --no-tag     # 只推代码，不发版本
#   bash scripts/release.sh --dry-run "说明"          # 只看会做什么，不动仓库
#
# 发完之后不用管：CI 会构建双架构镜像 + 生成发布清单（2–11 分钟）。
set -euo pipefail

cd "$(dirname "$0")/.."

BRANCH="main"
DRY=0
NO_TAG=0
BUMP="patch"
MSG=""

die()  { printf '\n✗ %s\n' "$*" >&2; exit 1; }
info() { printf '  %s\n' "$*"; }
step() { printf '\n▸ %s\n' "$*"; }
run()  {
  if [ "$DRY" = "1" ]; then
    printf '  [dry-run] %s\n' "$*"
  else
    "$@"
  fi
}

usage() {
  sed -n '8,18p' "$0" | sed 's/^# \{0,1\}//'
}

for a in "$@"; do
  case "$a" in
    --dry-run) DRY=1 ;;
    --no-tag)  NO_TAG=1 ;;
    --minor)   BUMP="minor" ;;
    --major)   BUMP="major" ;;
    -h|--help) usage; exit 0 ;;
    -*)        die "不认识的参数：$a（-h 看用法）" ;;
    *)         MSG="$a" ;;
  esac
done

# ---------------------------------------------------------------------------
# 1. 先确认自己在哪儿、在哪个分支
# ---------------------------------------------------------------------------
step "检查仓库状态"

[ -d .git ] || die "这里不是 git 仓库：$(pwd)"

cur="$(git branch --show-current)"
[ "$cur" = "$BRANCH" ] || die "当前在 $cur 分支，发版只允许在 $BRANCH"
info "分支 $cur"

dirty="$(git status --porcelain)"
if [ -n "$dirty" ]; then
  [ -n "$MSG" ] || die "有改动但没有说明。用法：bash scripts/release.sh \"改了什么\""
  info "改动 $(printf '%s\n' "$dirty" | wc -l | tr -d ' ') 个文件"
else
  info "没有未提交的改动"
  [ "$NO_TAG" = "1" ] && die "没有改动，也没什么可推的"
fi

# 门禁：把密钥/内网地址挡在提交之前。这一步比"发错了再回滚"便宜得多。
if [ "$DRY" != "1" ]; then
  step "跑仓库门禁"
  bash scripts/check-repo-hygiene.sh >/tmp/ld-hygiene.log 2>&1 \
    || { tail -20 /tmp/ld-hygiene.log; die "仓库卫生检查没过，先修上面的问题"; }
  info "卫生检查通过"
fi

# ---------------------------------------------------------------------------
# 2. 算下一个版本号（在提交之前算，好让提交信息里带上它）
# ---------------------------------------------------------------------------
next=""
if [ "$NO_TAG" != "1" ]; then
  step "算下一个版本号"
  latest="$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)"
  base="${latest#v}"
  IFS=. read -r MA MI PA <<< "$base"
  case "$BUMP" in
    major) MA=$((MA + 1)); MI=0; PA=0 ;;
    minor) MI=$((MI + 1)); PA=0 ;;
    *)     PA=$((PA + 1)) ;;
  esac
  next="v$MA.$MI.$PA"
  info "当前 $latest → 下一个 $next"
  git rev-parse -q --verify "refs/tags/$next" >/dev/null \
    && die "$next 已经存在（本地）。换一个号，或先删掉它：git tag -d $next"
fi

# ---------------------------------------------------------------------------
# 3. 提交
# ---------------------------------------------------------------------------
if [ -n "$dirty" ]; then
  step "提交"
  run git add -A
  run git commit -q -m "$MSG"
  info "已提交：$MSG"
fi

# ---------------------------------------------------------------------------
# 4. 推代码 —— 必须在打 tag 之前。反过来会发一个云端不存在的版本。
# ---------------------------------------------------------------------------
step "推送代码到 origin/$BRANCH"
run git push -q origin "$BRANCH"
info "已推送"

if [ "$NO_TAG" = "1" ]; then
  printf '\n✅ 代码已上传（没有发版本，用户端不会有更新提示）\n'
  exit 0
fi

# ---------------------------------------------------------------------------
# 5. 打 tag 并推送
# ---------------------------------------------------------------------------
step "打版本号 $next"
run git tag -a "$next" -m "$MSG"

step "推送 $next"
run git push -q origin "$next"

if [ "$DRY" = "1" ]; then
  printf '\n[dry-run] 以上动作都没有真正执行。去掉 --dry-run 才会生效。\n'
  exit 0
fi

cat <<EOF

✅ $next 已发布

   CI 正在构建镜像并生成发布清单（2–11 分钟，不用等）：
   https://github.com/abcd153334/lingdong-api/actions

   构建完成后：
   · 新装的用户直接拿到 $next
   · 已在跑的用户 6 小时内看到「有新版本」，点一下就更新
EOF

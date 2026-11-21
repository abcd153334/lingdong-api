#!/usr/bin/env bash
# 生成发布清单 release.json（管理台「版本与更新」的数据来源）。
#
# 为什么要有脚本：清单里有一个逃不掉的承诺 —— `tag` 必须真的存在于远端仓库。
# 手写时最容易出的错是"版本号写了 v2.2.1、tag 却是 v2.2.0"，而症状是
# 管理台提示有新版、点更新后 agent 报 `tag_not_found`。
#
# 用法：
#   bash deploy/scripts/gen-release-manifest.sh v2.2.0 --notes "修复计费口径"
#   bash deploy/scripts/gen-release-manifest.sh v2.2.0 --out /tmp/release.json
#
# 产出可直接上传到对象存储（公开只读）或放在任意 https 静态位置。

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

TAG_ARG="${1-}"
[ -n "$TAG_ARG" ] || die "用法：bash deploy/scripts/gen-release-manifest.sh <TAG> [--notes 文案] [--out 路径]"
shift

NOTES=""
OUT=""
URL=""
while [ $# -gt 0 ]; do
  case "$1" in
    --notes) NOTES="${2-}"; shift 2 ;;
    --out) OUT="${2-}"; shift 2 ;;
    --url) URL="${2-}"; shift 2 ;;
    *) die "未知参数：$1" ;;
  esac
done

case "$TAG_ARG" in
  local|latest) die "拒绝 $TAG_ARG：清单里的版本必须是可追溯的发布 tag" ;;
esac

# 与后端 releasecheck 同一套形状（vX.Y.Z + 可选后缀）。两边不一致的后果是
# "发布者按清单写对了、管理台却判非法"。
if ! printf '%s' "$TAG_ARG" | grep -Eq '^v?[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}([-+][0-9A-Za-z.]+)?$'; then
  die "版本号形状不合法：$TAG_ARG（应为 vX.Y.Z）"
fi

# commit 只在能解析到 tag 时写入：它是"tag 没被移动"的凭据，
# 编一个假的比不写更糟（agent 会按 commit_mismatch 拒绝更新）。
COMMIT="$(git -C "$ROOT_DIR" rev-parse -q --verify "refs/tags/$TAG_ARG^{commit}" 2>/dev/null || true)"
if [ -z "$COMMIT" ]; then
  warn "本地找不到 tag $TAG_ARG（还没推上来？）：清单里不写 commit"
fi

json_escape() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\n\r'
}

PUBLISHED="$(date '+%Y-%m-%d')"
body="$(cat <<EOF
{
  "schema": 1,
  "latest": {
    "version": "$(json_escape "$TAG_ARG")",
    "tag": "$(json_escape "$TAG_ARG")",
    "published_at": "$PUBLISHED",
    "notes": "$(json_escape "$NOTES")",
    "commit": "$COMMIT",
    "url": "$(json_escape "$URL")"
  }
}
EOF
)"

if [ -n "$OUT" ]; then
  printf '%s\n' "$body" > "$OUT"
  ok "已写入 $OUT"
else
  printf '%s\n' "$body"
fi

cat >&2 <<EOF

注意：**正常发版不需要跑这个脚本**。推 v* tag 时
  .github/workflows/docker.yml 会自动生成清单并推送到 release-manifest 分支，
  地址固定为：
  https://raw.githubusercontent.com/abcd153334/lingdong-api/release-manifest/release.json

本脚本只在两种情况下用：
  · 离线环境 / 私有发布渠道（没有 GitHub Actions）
  · 想手工指定 notes 或改清单内容

手工路径：
  1. 确认 tag 已推送到远端：git push origin $TAG_ARG
  2. 把上面的 JSON 放到一个 https **直出**地址（不能是 3xx 重定向：
     抓取端禁重定向，重定向会被判 http_status）
  3. 管理台「系统配置 → 版本与更新」填入该地址（留空则用官方清单）
EOF

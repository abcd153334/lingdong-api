#!/usr/bin/env bash
# 首启口令落一份到本机（0600），防止「日志翻不到就永久丢失」（v2.2.3）。
#
# 为什么需要它：初始口令只在**首次建号**的那一次启动里打印一行，之后任何重启都不会再打
# （设计如此：随机口令长期留在日志里，等于把公开口令又装回去）。用户常见的翻车姿势是
# `ld.sh up` 的输出被后续构建日志刷走，或者先 `down -v` 重建了一次 —— 口令就此消失，
# 只能靠建号前的备份恢复。
#
# 落点在家目录、不在仓库里：`.env` 与仓库都可能被提交/打包，家目录 + 600 是交接凭据
# 该待的地方。也不打印口令本身，只在终端里说「存到哪了」。
#
# 幂等：目标文件已存在就什么都不做（下次启动不该覆盖第一次的交接凭据）。
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

OUT="${LD_ADMIN_CRED_FILE:-$HOME/.lingdong-admin-initial}"

[ -f "$OUT" ] && exit 0
require_docker

line="$( cd "$DEPLOY_DIR" && docker compose -f "$COMPOSE_FILE" logs --no-color --tail 2000 backend 2>/dev/null \
        | grep -F '初始口令' | tail -n 1 || true )"
[ -n "$line" ] || exit 0

# 日志是 zap 的 JSON 编码：..."password":"<base64url>"...
# 随机口令的字符集由 seed.go 决定（base64.RawURLEncoding → A-Za-z0-9_-），
# 环境变量显式指定时可能是任意字符，所以只在"能认出随机串形状"时才落盘。
pw="$(printf '%s' "$line" | sed -n 's/.*"password"[[:space:]]*:[[:space:]]*"\([A-Za-z0-9_-]\{8,\}\)".*/\1/p' | tail -n 1)"
[ -n "$pw" ] || exit 0

umask 077
cat > "$OUT" <<EOF
username=admin
password=$pw
note=首启自动生成，登录后必须先改密（系统会强制）。删掉本文件不影响账号。
EOF
chmod 600 "$OUT"
log "初始管理员口令已保存到 $OUT（600）"

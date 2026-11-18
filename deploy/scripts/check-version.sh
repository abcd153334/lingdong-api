#!/usr/bin/env bash
# G4：版本口径门禁。
#
# 三处必须自洽，否则"打出来的镜像"和"记录的版本"会对不上：
#   1. go.mod 的 go 指令 ≤ 构建镜像的 Go 版本（用 1.22 编 1.23 模块会直接失败）；
#   2. controller.Version 必须是 var（const 会让 -ldflags -X 静默失效）；
#   3. 若存在 deploy/.release，它的 version 必须等于源码里的 Version。
#
# 用法：bash deploy/scripts/check-version.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
GOMOD="$ROOT_DIR/backend/go.mod"
DOCKERFILE="$ROOT_DIR/deploy/backend.Dockerfile"
HEALTH="$ROOT_DIR/backend/internal/controller/health.go"
RELEASE="$ROOT_DIR/deploy/.release"
fail=0

# ---------- 1. Go 版本 ----------
go_directive="$(grep -E '^go [0-9]+\.[0-9]+' "$GOMOD" | awk '{print $2}')"
builder="$(grep -E '^FROM golang:[0-9]+\.[0-9]+' "$DOCKERFILE" | head -1 | sed -E 's/^FROM golang:([0-9]+\.[0-9]+).*/\1/')"

if [ -z "$go_directive" ] || [ -z "$builder" ]; then
  echo "FAIL 读不到 Go 版本（go.mod=$go_directive, Dockerfile=$builder）"
  fail=1
else
  # 逐段比大小：1.23 > 1.22
  IFS=. read -r gmaj gmin <<< "$go_directive"
  IFS=. read -r bmaj bmin <<< "$builder"
  if [ "$bmaj" -lt "$gmaj" ] || { [ "$bmaj" -eq "$gmaj" ] && [ "$bmin" -lt "$gmin" ]; }; then
    echo "FAIL 构建镜像 Go $builder < go.mod 要求 $go_directive（会直接编译失败）"
    fail=1
  else
    echo "PASS Go 版本：go.mod=$go_directive ≤ builder=$builder"
  fi
fi

# ---------- 2. Version 必须是 var ----------
if grep -qE '^var Version = "' "$HEALTH"; then
  echo "PASS Version 是 var（-ldflags -X 可注入）"
elif grep -qE '^const Version = "' "$HEALTH"; then
  echo "FAIL Version 是 const：-X 会静默失效，镜像版本永远是源码字面量"
  fail=1
else
  echo "FAIL 找不到 Version 声明"
  fail=1
fi

# ---------- 3. .release 一致性 ----------
if [ -f "$RELEASE" ]; then
  rel="$(grep -E '^version=' "$RELEASE" | cut -d= -f2- || true)"
  src="$(grep -E '^var Version = ' "$HEALTH" | sed -E 's/.*"(.*)".*/\1/')"
  if [ -n "$rel" ] && [ "$rel" != "$src" ]; then
    echo "WARN .release 记录的版本（$rel）与源码（$src）不同：确认这是有意为之（回滚场景）"
  else
    echo "PASS .release 与源码版本一致（$src）"
  fi
else
  echo "SKIP 还没有 .release（首次发布前正常）"
fi

exit "$fail"

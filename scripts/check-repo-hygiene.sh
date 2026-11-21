#!/usr/bin/env bash
# 公开仓卫生门禁（G0）。
#
# 背景：仓库 github.com/abcd153334/lingdong-api 是公开的，push 不可撤销。
# 本脚本是「哪些文件能进仓库」的唯一判据——不接受「我看过了没问题」。
#
# 查五件事：
#   1. 顶层只允许代码目录（.github/backend/web/deploy/scripts + 四个元信息文件）
#   2. 明确的禁止路径（交付文档、厂商资料、原型、备份、第三方副本、内部基础设施）
#   3. .md 只允许两类：
#      a) 编译/测试必需（go:embed 的 skill、apidoc 夹具、根 README）；
#      b) **部署文档**（deploy/README.md、deploy/edge/**、deploy/agent/**）——
#         目标是「克隆即可自助部署」：部署手册必须跟代码同版本、同一次 clone。
#         这类文档只写操作步骤与模板，不写生产域名/IP/凭据（第 4 节逐条扫）。
#   4. 敏感值：私钥块、云厂商 AK、预签名 URL 凭据、生产域名、个人邮箱、已知口令
#   5. 误产物：无扩展名散落文件、%XX 编码文件名
#
# 用法：bash scripts/check-repo-hygiene.sh
# 退出：0 = 干净；1 = 有违规（逐条打印，可直接照着删）

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR" || exit 1

violations=0
report() { printf '  \033[31m✗\033[0m %s\n' "$1"; violations=$((violations + 1)); }
section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# ---------- 1. 顶层白名单 ----------
section "1/5 顶层范围"
ALLOWED_TOP='^(\.github|backend|web|deploy|scripts|testdata)/'
ALLOWED_ROOT='^(README\.md|\.gitignore|\.gitattributes|\.dockerignore)$'
bad_top=$(git ls-files | grep -vE "$ALLOWED_TOP" | grep -vE "$ALLOWED_ROOT")
if [ -n "$bad_top" ]; then
  while IFS= read -r f; do report "不允许的顶层路径: $f"; done <<< "$(echo "$bad_top" | head -20)"
  n=$(echo "$bad_top" | wc -l)
  [ "$n" -gt 20 ] && printf '  ... 另有 %d 个同类文件\n' "$((n - 20))"
else
  printf '  \033[32m✓\033[0m 顶层只含代码目录与元信息文件\n'
fi

# ---------- 2. 禁止路径 ----------
section "2/5 禁止路径"
DENY='^(docs/|API文档/|prototype/|lab/|data/|evidence/|outputs/|xunhupay-master/|new-api/|源代码|\.trae/|\.workbuddy/|deploy/backups/|deploy/\.release|scripts/dev[^/]*\.ps1$|scripts/setup-pg-user\.ps1$|scripts/live-rebuild\.py$|scripts/reset-doc-groups\.ps1$|scripts/duomi-nano-protocol\.sql$|scripts/gen-docs-fixtures\.mjs$|deploy/scripts/check-docs\.sh$|deploy/scripts/gen-env-doc\.sh$)'
bad_path=$(git ls-files | grep -E "$DENY")
if [ -n "$bad_path" ]; then
  while IFS= read -r f; do report "禁止入库: $f"; done <<< "$(echo "$bad_path" | head -20)"
  n=$(echo "$bad_path" | wc -l)
  [ "$n" -gt 20 ] && printf '  ... 另有 %d 个同类文件\n' "$((n - 20))"
else
  printf '  \033[32m✓\033[0m 无禁止路径\n'
fi

# ---------- 3. .md 白名单 ----------
section "3/5 文档白名单"
MD_ALLOWED='^(README\.md|deploy/README\.md|deploy/edge/[^/]*\.md|deploy/edge/[^/]+/[^/]*\.md|deploy/optional/[^/]+/[^/]*\.md|backend/internal/service/agent/skill/[^/]+\.md|backend/internal/apidoc/testdata/[^/]+\.md)$'
bad_md=$(git ls-files '*.md' | grep -vE "$MD_ALLOWED")
if [ -n "$bad_md" ]; then
  while IFS= read -r f; do report "多余文档: $f"; done <<< "$(echo "$bad_md" | head -20)"
  n=$(echo "$bad_md" | wc -l)
  [ "$n" -gt 20 ] && printf '  ... 另有 %d 个 .md\n' "$((n - 20))"
else
  printf '  \033[32m✓\033[0m .md 仅剩编译/测试必需的几个\n'
fi

# ---------- 4. 敏感值 ----------
section "4/5 敏感值"
# 硬禁止：私钥、云厂商 AK、预签名凭据、生产域名与备案号、个人邮箱。
# 早期版本曾给 `example.com` 开过「只允许出现在 seed.go」的口子，理由是
# 「产品对外身份本来就公开」——后来发现同样的值在 e2e 夹具、注释、placeholder 里
# 各有一份，逐个开口子等于没有规则。现在一律换成 example.com，口径回到零容忍。
#
# ⚠️ 铁则：本脚本**不得出现它要守的实际值**。曾经把个人邮箱和本地库口令原样写进
# 这里，结果脚本自己成了仓库里唯一泄漏它们的地方（真实发生过，被外部 clone 扫出来）。
# 一律用「形态正则」而不是字面量：抓的是 QQ 邮箱这个类别，不是某个人的地址。
SECRET_HARD='-----BEGIN [A-Z ]*PRIVATE KEY-----|AKLT[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|LTAI[A-Za-z0-9]{12,}|AKID[A-Za-z0-9]{13,}|AIza[A-Za-z0-9_-]{35}|gh[pousr]_[A-Za-z0-9]{36,}|xox[baprs]-[A-Za-z0-9-]{10,}|X-Tos-Credential=|X-Amz-Credential=|X-Tos-Signature=|lingdongai\.vip|lingdong-api\.cn|lingdong\.cn|京ICP备[0-9]{8}号|[0-9]{5,12}@qq\.com'

hits=$(git grep -nIE "$SECRET_HARD" -- . 2>/dev/null | grep -vE 'testdata|_test\.(go|ts|tsx)|check-repo-hygiene\.sh|example|EXAMPLE|placeholder')
if [ -n "$hits" ]; then
  while IFS= read -r line; do report "敏感值: ${line:0:150}"; done <<< "$(echo "$hits" | head -15)"
  n=$(echo "$hits" | wc -l)
  [ "$n" -gt 15 ] && printf '  ... 另有 %d 处\n' "$((n - 15))"
else
  printf '  \033[32m✓\033[0m 未发现密钥/AK/生产域名/备案号/个人邮箱\n'
fi

# ---------- 5. 误产物 ----------
section "5/5 误产物"
NOEXT=$(git ls-files | awk -F/ '{n=$NF; if (n !~ /\./ && n != "Makefile" && n != "LICENSE" && n != "NOTICE") print}')
if [ -n "$NOEXT" ]; then
  while IFS= read -r f; do report "无扩展名散落文件: $f"; done <<< "$NOEXT"
else
  printf '  \033[32m✓\033[0m 无无扩展名散落文件\n'
fi
encoded=$(git ls-files | grep -E '%[0-9A-Fa-f]{2}')
if [ -n "$encoded" ]; then
  while IFS= read -r f; do report "文件名含 %XX 编码: $f"; done <<< "$encoded"
else
  printf '  \033[32m✓\033[0m 无 %%XX 编码文件名\n'
fi

# ---------- 结论 ----------
printf '\n'
if [ "$violations" -eq 0 ]; then
  printf '\033[32m门禁通过：%d 个跟踪文件全部符合公开仓白名单\033[0m\n' "$(git ls-files | wc -l)"
  exit 0
fi
printf '\033[31m门禁未通过：%d 处违规\033[0m\n' "$violations"
printf '规则见本脚本头部注释（五节检查）与 .gitattributes。\n'
exit 1

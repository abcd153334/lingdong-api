#!/usr/bin/env bash
# 部署前体检：只查「查出来能立刻改」的项。
#
# 设计原则：一条检查只回答一个可执行的问题，失败必须给出**怎么改**。
# 刻意不做的三类（以前做过，全是噪音）：
#   - 端口占用：`docker compose up` 会报 `address already in use`，信息一样清楚；
#   - 磁盘余量：只报 FAIL 不给动作，`df -h` 本来就在手边；
#   - 编排语法：`docker compose config -q` 一条命令的事，不需要包一层。
#
# 用法：bash deploy/scripts/preflight.sh
#       SKIP_DOCKER=1 时跳过编排校验（只改文档/脚本的机器用）。

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

FAILED=0
pass() { ok "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILED=$((FAILED + 1)); }

[ -f "$ENV_FILE" ] || die "找不到 $ENV_FILE —— 先执行：bash deploy/ld.sh init"

log "部署前体检：$ENV_FILE"

# ---------- 1. 文件权限 ----------
# Windows（Git Bash / MSYS）不支持 POSIX 权限位：chmod 600 之后 stat 仍报 644。
# 在那种环境下这检查只会制造假红灯，因此只告警；Linux 生产机必须通过。
case "$(uname -s 2>/dev/null || echo unknown)" in
  MINGW*|MSYS*|CYGWIN*)
    warn "Windows 文件系统不支持 POSIX 权限位，跳过 .env 权限检查（Linux 生产机仍会强制）"
    ;;
  *)
    if stat -c '%a' "$ENV_FILE" >/dev/null 2>&1; then
      perm="$(stat -c '%a' "$ENV_FILE")"
      case "$perm" in
        600|400) pass ".env 权限 $perm" ;;
        *) fail ".env 权限是 $perm，应为 600（它含库内密文主密钥）：chmod 600 $ENV_FILE" ;;
      esac
    else
      warn "无法读取 .env 权限，跳过"
    fi
    ;;
esac

# ---------- 2. 必填项 ----------
for k in APP_ENV POSTGRES_PASSWORD REDIS_PASSWORD APP_ENCRYPTION_KEY; do
  v="$(read_env "$k" '')"
  if [ -z "$v" ]; then
    fail "必填项 $k 为空 —— 缺它 compose 会直接拒绝启动（重跑 bash deploy/ld.sh init 可补齐）"
  else
    pass "$k 已配置"
  fi
done

# ---------- 3. 取值合法性 ----------
app_env="$(read_env APP_ENV '')"
case "$app_env" in
  prod) pass "APP_ENV=prod" ;;
  dev|test) warn "APP_ENV=$app_env：弱密钥只告警不拒绝，且会打印路由表。生产必须是 prod" ;;
  "") : ;;
  *) fail "APP_ENV=$app_env 非法（只接受 prod/dev/test）" ;;
esac

key="$(read_env APP_ENCRYPTION_KEY '')"
if [ -n "$key" ]; then
  # 公开示例值必须与 backend/internal/conf/secrets.go 的 publicSecretValues 同步。
  # 历史事故：两处字面量不同，于是"用公开示例值部署"体检显示 pass，
  # 而后端 ValidateSecrets 在 prod 直接拒绝启动 —— 体检绿灯、服务起不来。
  if [ "$key" = "0123456789abcdef0123456789abcdef" ]; then
    fail "APP_ENCRYPTION_KEY 是公开示例值 —— 等于把库内密文交给任何读过源码的人"
  elif [ "${#key}" -lt 16 ]; then
    # 硬下限 16 与后端一致（服务端 SHA-256 派生，任意长度可用，门槛只防"没有熵"）。
    fail "APP_ENCRYPTION_KEY 只有 ${#key} 位，至少 16 位（bash deploy/ld.sh init 会生成 64 位）"
  elif [ "${#key}" -lt 32 ]; then
    warn "APP_ENCRYPTION_KEY 为 ${#key} 位：能正常派生，但建议至少 32 位"
  else
    pass "APP_ENCRYPTION_KEY 长度 ${#key}"
  fi
fi

pgpw="$(read_env POSTGRES_PASSWORD '')"
if [ -n "$pgpw" ]; then
  case "$pgpw" in
    *[!A-Za-z0-9_-]*) fail "POSTGRES_PASSWORD 含 URL 保留字符（只允许 A-Za-z0-9_-）：@ : / ? # 会把 DATABASE_URL 拼坏" ;;
    *) pass "POSTGRES_PASSWORD 字符集合法" ;;
  esac
  [ "${#pgpw}" -ge 16 ] || fail "POSTGRES_PASSWORD 只有 ${#pgpw} 位，建议 ≥16"
fi

redispw="$(read_env REDIS_PASSWORD '')"
if [ -n "$redispw" ] && [ "${#redispw}" -lt 12 ]; then
  fail "REDIS_PASSWORD 只有 ${#redispw} 位，建议 ≥12（它保护限流键与 leader 锁）"
fi

# ---------- 4. 口令单一真相源 ----------
dsn_explicit="$(read_env DATABASE_URL '')"
if [ -n "$dsn_explicit" ] && [ -n "$pgpw" ]; then
  case "$dsn_explicit" in
    *"@postgres:5432"*)
      case "$dsn_explicit" in
        *":${pgpw}@"*) pass "DATABASE_URL 与本机 POSTGRES_PASSWORD 一致" ;;
        *) fail "DATABASE_URL 里的口令与 POSTGRES_PASSWORD 不一致 —— 两个真相源，后端会连库失败。注释掉 DATABASE_URL 让它自动派生" ;;
      esac
      ;;
    *) pass "DATABASE_URL 指向外部库（跳过一致性检查）" ;;
  esac
fi

# ---------- 5. 反代与入口 ----------
#
# .env 不写 LD_TRUSTED_PROXIES 是**推荐**姿势：编排给了与 subnet 配对的默认值。
# 所以这里按「容器实际拿到的值」判断（先 .env，再 compose 解析结果）——
# 只读 .env 会让每次默认部署都报一条假警告，假警告多了真警告就没人看。
tp="$(read_env LD_TRUSTED_PROXIES '')"
tp_src=".env"
if [ -z "$tp" ] && command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  tp="$( ( cd "$DEPLOY_DIR" && docker compose -f "$(basename "$COMPOSE_FILE")" config ) 2>/dev/null \
        | sed -n 's/^ *LD_TRUSTED_PROXIES: *//p' | head -n1 | tr -d "\"'" | tr -d '\r')"
  tp_src="编排默认值"
fi
if [ -n "$tp" ]; then
  bad=0
  IFS=',' read -ra parts <<< "$tp"
  for p in "${parts[@]}"; do
    p="$(echo "$p" | tr -d ' ')"
    case "$p" in
      0.0.0.0/0|::/0) fail "LD_TRUSTED_PROXIES 含 $p：等于信任任何来源，任何人都能伪造 IP"; bad=1 ;;
      */*)
        ones="${p#*/}"
        case "$p" in
          *:*) [ "$ones" -ge 48 ] || { fail "LD_TRUSTED_PROXIES 网段过宽：$p（v6 需 ≥ /48）"; bad=1; } ;;
          *)   [ "$ones" -ge 16 ] || { fail "LD_TRUSTED_PROXIES 网段过宽：$p（v4 需 ≥ /16）"; bad=1; } ;;
        esac
        ;;
    esac
  done
  # 回环必须在列表里：入口（宝塔/宿主 nginx）转到发布的 127.0.0.1:8088 时，
  # 容器看到的是 docker-proxy 的回环地址并由它追加 XFF；不信任回环，
  # 每个公网用户都会被当成同一个 IP（限流、验证码升级、IP 白名单、审计全废）。
  case ",$tp," in
    *,127.0.0.1/32,*|*,127.0.0.1,*) : ;;
    *) warn "LD_TRUSTED_PROXIES 未含 127.0.0.1/32：同机入口部署下，所有用户会塌成同一个 IP" ;;
  esac
  if [ "$bad" -eq 0 ]; then pass "LD_TRUSTED_PROXIES 合法（$tp_src）：$tp"; fi
else
  warn "LD_TRUSTED_PROXIES 为空：只适用于「直连」部署；经反代时所有用户会塌进同一个限流桶"
fi

# Cookie 的 Secure 由 LD_COOKIE_SECURE 决定（默认 auto：按请求实际协议）。
# 这条提示只在「prod + 强制 secure」时才有意义，因为那才真的要求 TLS。
cookie_mode="$(read_env LD_COOKIE_SECURE 'auto')"
case "$cookie_mode" in
  auto) pass "LD_COOKIE_SECURE=auto（https 自动加 Secure，明文入口不加）" ;;
  true) [ "$(port_listening 443 >/dev/null 2>&1 && echo 1 || echo 0)" = "1" ] \
          && pass "LD_COOKIE_SECURE=true 且 443 有监听" \
          || warn "LD_COOKIE_SECURE=true 但 443 没有监听：浏览器不会在 http:// 上回传会话 Cookie，登录会「成功但立刻掉线」" ;;
  false) warn "LD_COOKIE_SECURE=false：会话 Cookie 不带 Secure，仅用于临时验证，生产不要这么配" ;;
  *) fail "LD_COOKIE_SECURE=$cookie_mode 非法（auto|true|false）" ;;
esac

mode="$(read_env LD_NODE_MODE 'full')"
case "$mode" in full|api) pass "LD_NODE_MODE=$mode" ;; *) fail "LD_NODE_MODE=$mode 非法（full|api）" ;; esac

# ---------- 6. 编排可解析 ----------
if [ "${SKIP_DOCKER:-0}" = "1" ]; then
  warn "SKIP_DOCKER=1：跳过 docker/compose 校验"
elif ! command -v docker >/dev/null 2>&1; then
  fail "找不到 docker —— 只改文档/脚本的机器用 SKIP_DOCKER=1；目标机必须装 Docker ≥24"
elif docker compose version >/dev/null 2>&1; then
  pass "docker compose v2 可用"
  if ( cd "$DEPLOY_DIR" && docker compose -f "$(basename "$COMPOSE_FILE")" config -q ) 2>/tmp/ld-compose.err; then
    pass "编排文件可解析（docker compose config）"
  else
    fail "编排文件无法解析：$(head -3 /tmp/ld-compose.err | tr '\n' ' ')"
  fi
else
  fail "docker compose v2 不可用"
fi

echo
if [ "$FAILED" -eq 0 ]; then
  log "体检通过（0 项失败）"
else
  die "体检未通过：$FAILED 项失败，按上面提示逐条修正后再启动"
fi

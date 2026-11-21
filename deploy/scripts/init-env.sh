#!/usr/bin/env bash
# 生成 deploy/.env —— 让「一条命令部署」成立的那一步（v2.2.0）。
#
# 设计只有两条，都是踩过的：
#   1. **幂等，且绝不覆盖已有值**。覆盖 APP_ENCRYPTION_KEY 等于让库里的
#      渠道密钥、托管密钥、/v1/media 直链签名永久解不开 —— 数据还在，但都用不了。
#      所以这里只做「缺哪个补哪个」，已存在的键一个字节都不动。
#   2. **不打印任何密钥**。终端可能被录屏/贴进聊天，密钥只落 0600 的文件。
#
# 生成的口令只用 [0-9a-f]：POSTGRES_PASSWORD / REDIS_PASSWORD 会参与拼 DSN
# 与 redis-cli 参数，`@ : / ? #` 会把连接串拼坏（那是"体检绿灯、服务起不来"的经典成因）。
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

ENV_FILE="$DEPLOY_DIR/.env"

rand_hex() {
  # openssl 缺失时退到 /dev/urandom：只要 32 个十六进制字符，不值得为此装包。
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$1"
  else
    head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

# 键是否已存在（含空值也算存在：空值由后端 ValidateSecrets 明确拒绝并说清原因，
# 我们不去猜用户为什么留空）。
has_key() { grep -qE "^[[:space:]]*$1=" "$ENV_FILE" 2>/dev/null; }

value_of() { read_env "$1" ''; }

ensure_key() {
  local key="$1" value="$2"
  if has_key "$key"; then
    if [ -z "$(value_of "$key")" ]; then
      warn "$key 存在但为空 —— 保留原样（后端会明确拒绝启动并说明原因）"
    fi
    return 0
  fi
  printf '%s=%s\n' "$key" "$value" >> "$ENV_FILE"
  created="${created}${key} "
}

created=""
fresh=0
if [ ! -f "$ENV_FILE" ]; then
  fresh=1
  umask 077
  cat > "$ENV_FILE" <<'EOF'
# 由 `bash deploy/ld.sh init` 生成（v2.2.0）。
# 这四个值缺一不可，且不要手改：改了口令要重建库，改了加密密钥解不开库内密文。
# 更多可选项（时区、跨域、指标、留痕）见 deploy/.env.example，按需追加即可。
APP_ENV=prod
EOF
fi

ensure_key POSTGRES_PASSWORD "$(rand_hex 16)"
ensure_key REDIS_PASSWORD     "$(rand_hex 16)"
ensure_key APP_ENCRYPTION_KEY "$(rand_hex 32)"
# auto：按请求实际协议决定 Cookie 的 Secure（https 才加）。
# 好处是「本机 http 先验一遍」与「生产上 TLS」共用同一份配置，不需要改环境变量。
ensure_key LD_COOKIE_SECURE   "auto"
# 唯一对外端口。改这里即可，脚本与文档都读同一个键。
ensure_key APP_PORT           "8088"

chmod 600 "$ENV_FILE"

if [ "$fresh" = "1" ]; then
  ok "已生成 $ENV_FILE（权限 600，含 3 个随机密钥）"
elif [ -n "$created" ]; then
  ok "已补全 $ENV_FILE 的缺失键：$created（已有值未改动）"
else
  log "$ENV_FILE 已就绪（未改动）"
fi

#!/usr/bin/env bash
# 部署统一入口（v2.2.0）：日常只有一条命令 —— `bash deploy/ld.sh up`。
#
# 只做转发：任何"智能判断"都应该在具体脚本里，否则排障要读两处。

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 编排文件：默认 docker-compose.yml。
# 同一台机器要跑多套、或端口/网段要换时，用 COMPOSE_FILE 指到自己的文件；
# scripts/*.sh 与 common.sh 都认这个变量（以前只有部分脚本认，最常用的 up/down 反而不认）。
COMPOSE_FILE="${COMPOSE_FILE:-$DIR/docker-compose.yml}"

usage() {
  cat <<'EOF'
用法：bash deploy/ld.sh <命令> [参数]

  up [--pull]       启动（自动生成 .env；--pull 优先拉发布镜像，否则本地构建）
  init              只生成/补全 deploy/.env（幂等，不覆盖已有值）
  down              停止（保留卷与数据）
  restart           重启应用容器（不重建、不改配置）
  ps                容器状态
  logs [服务]       跟踪日志（默认 backend）
  health            浅探 + 深探
  preflight         部署前体检
  backup [参数]     备份（转给 scripts/backup.sh）
  restore <参数>    恢复（转给 scripts/restore.sh）
  upgrade <TAG>     切换版本（转给 scripts/upgrade.sh）
  tag <TAG>         构建并记录发布（转给 scripts/tag-release.sh）
  config            校验编排文件
EOF
}

case "${1-}" in
  up)
    shift
    pull=0; args=()
    for a in "$@"; do
      case "$a" in
        --pull) pull=1 ;;
        *) args+=("$a") ;;
      esac
    done
    # 没有 .env 就先造一个：3 个密钥自动生成，用户不需要手填任何值。
    bash "$DIR/scripts/init-env.sh"
    if [ "$pull" = "1" ]; then
      ( cd "$DIR" && docker compose -f "$COMPOSE_FILE" pull )
      ( cd "$DIR" && docker compose -f "$COMPOSE_FILE" up -d "${args[@]}" )
    else
      # 默认构建：验的是"这份 checkout 能不能跑"。镜像已存在时 Docker 会复用层缓存。
      ( cd "$DIR" && docker compose -f "$COMPOSE_FILE" up -d --build "${args[@]}" )
    fi
    bash "$DIR/scripts/healthcheck.sh" --wait 180
    ;;
  init)        bash "$DIR/scripts/init-env.sh" ;;
  down)
    shift
    # 不加 -t：让 compose 使用各服务的 stop_grace_period（默认 10s 会硬切在途请求）
    ( cd "$DIR" && docker compose -f "$COMPOSE_FILE" down "$@" )
    ;;
  restart)     ( cd "$DIR" && docker compose -f "$COMPOSE_FILE" restart backend ) ;;
  ps)          ( cd "$DIR" && docker compose -f "$COMPOSE_FILE" ps ) ;;
  logs)
    svc="${2:-backend}"
    ( cd "$DIR" && docker compose -f "$COMPOSE_FILE" logs -f --tail=200 "$svc" )
    ;;
  health)      bash "$DIR/scripts/healthcheck.sh" "${@:2}" ;;
  preflight)   bash "$DIR/scripts/preflight.sh" ;;
  backup)      bash "$DIR/scripts/backup.sh" "${@:2}" ;;
  restore)     bash "$DIR/scripts/restore.sh" "${@:2}" ;;
  upgrade)     bash "$DIR/scripts/upgrade.sh" "${@:2}" ;;
  tag)         bash "$DIR/scripts/tag-release.sh" "${@:2}" ;;
  config)
    ( cd "$DIR" && docker compose -f "$COMPOSE_FILE" config -q && echo "compose OK" )
    ;;
  ""|-h|--help) usage ;;
  *) usage; exit 1 ;;
esac

#!/usr/bin/env bash
# 部署脚本统一入口：记住六条命令，不必记路径与参数顺序。
#
# 只做转发：任何"智能判断"都应该在具体脚本里，否则排障要读两处。

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<'EOF'
用法：bash deploy/ld.sh <命令> [参数]

  up                首次启动 / 应用改动（构建 + 启动，TAG 取自 .env 或 local）
  down              停止（保留卷与数据）
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
    ( cd "$DIR" && docker compose -f docker-compose.yml up -d --build "$@" )
    bash "$DIR/scripts/healthcheck.sh" --wait 120
    ;;
  down)
    shift
    # 不加 -t：让 compose 使用各服务的 stop_grace_period（默认 10s 会硬切在途请求）
    ( cd "$DIR" && docker compose -f docker-compose.yml down "$@" )
    ;;
  ps)
    ( cd "$DIR" && docker compose -f docker-compose.yml ps )
    ;;
  logs)
    svc="${2:-backend}"
    ( cd "$DIR" && docker compose -f docker-compose.yml logs -f --tail=200 "$svc" )
    ;;
  health)      bash "$DIR/scripts/healthcheck.sh" "${@:2}" ;;
  preflight)   bash "$DIR/scripts/preflight.sh" ;;
  backup)      bash "$DIR/scripts/backup.sh" "${@:2}" ;;
  restore)     bash "$DIR/scripts/restore.sh" "${@:2}" ;;
  upgrade)     bash "$DIR/scripts/upgrade.sh" "${@:2}" ;;
  tag)         bash "$DIR/scripts/tag-release.sh" "${@:2}" ;;
  config)
    ( cd "$DIR" && docker compose -f docker-compose.yml config -q && echo "compose OK" )
    ;;
  ""|-h|--help) usage ;;
  *) usage; exit 1 ;;
esac

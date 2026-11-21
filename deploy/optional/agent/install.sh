#!/usr/bin/env bash
# 安装 / 升级宿主机更新程序（ld-updater）。**必须用 root 执行**。
#
# 为什么装到 /opt/lingdong/updater 而不是仓库里：一次更新会 `git checkout` 整个工作区，
# 而 bash 是边读边执行的 —— 正在运行的脚本文件被改写会读到半截语句。
# 仓库里的 deploy/optional/agent/ 只是发布源，运行副本必须在仓库之外。
#
# 用法：
#   bash deploy/optional/agent/install.sh --repo /opt/lingdong/app
#   bash deploy/optional/agent/install.sh --repo /opt/lingdong/app --interval 10
#   bash deploy/optional/agent/install.sh --uninstall

set -euo pipefail

APP_DIR="/opt/lingdong/updater"
CONF_DIR="/etc/lingdong"
CONF="$CONF_DIR/updater.conf"
UNIT="/etc/systemd/system/ld-updater.service"
REPO_PATH="/opt/lingdong/app"
INTERVAL=5
UNINSTALL=0

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO_PATH="${2-}"; shift 2 ;;
    --interval) INTERVAL="${2-}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" = "0" ] || { echo "必须用 root 执行（要写 /etc 与 /opt，并管理 systemd）" >&2; exit 1; }

if [ "$UNINSTALL" = "1" ]; then
  systemctl disable --now ld-updater 2>/dev/null || true
  rm -f "$UNIT" "$CONF"
  rm -rf "$APP_DIR"
  systemctl daemon-reload
  echo "已卸载 ld-updater（任务表与备份不受影响）"
  exit 0
fi

command -v docker >/dev/null 2>&1 || { echo "缺少 docker：先按 docs/deployment/installation/docker.md 装好" >&2; exit 1; }
command -v git >/dev/null 2>&1 || { echo "缺少 git" >&2; exit 1; }
[ -d "$REPO_PATH/.git" ] || { echo "$REPO_PATH 不是 git 工作区：生产目录必须是独立 clone" >&2; exit 1; }
[ -f "$REPO_PATH/deploy/.env" ] || { echo "缺 $REPO_PATH/deploy/.env：先按 deploy/.env.example 生成" >&2; exit 1; }
docker compose -f "$REPO_PATH/deploy/docker-compose.yml" ps --status running --services \
  | grep -qx postgres || { echo "postgres 容器未运行：先 docker compose up -d" >&2; exit 1; }

mkdir -p "$APP_DIR" "$CONF_DIR"
install -m 0755 "$(dirname "$0")/ld-updater.sh" "$APP_DIR/ld-updater.sh"

cat > "$CONF" <<EOF
# ld-updater 安装配置（由 install.sh 生成；改完 systemctl restart ld-updater）
REPO_PATH=$REPO_PATH
INTERVAL=$INTERVAL
LOCK_FILE=/var/lock/ld-updater.lock
EOF
chmod 600 "$CONF"

cat > "$UNIT" <<'EOF'
[Unit]
Description=lingdong updater (one-click release executor)
Documentation=file:/opt/lingdong/updater/ld-updater.sh
After=docker.service network-online.target
Requires=docker.service

[Service]
Type=simple
ExecStart=/opt/lingdong/updater/ld-updater.sh
# 进程意外退出 = 上一次更新状态未知：重启后它会继续领取新任务，
# 而被中断的那一笔由后端按心跳判死（不会有两个进程抢同一次切换）。
Restart=always
RestartSec=5
# 它要调 docker，必须能拿到 /var/run/docker.sock
User=root
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now ld-updater
sleep 2
systemctl --no-pager --lines=8 status ld-updater || true

cat <<EOF

安装完成：
  运行副本  $APP_DIR/ld-updater.sh   （仓库里的改动需要重新跑本脚本才生效）
  配置      $CONF
  日志      journalctl -u ld-updater -f
  自检      bash $APP_DIR/ld-updater.sh --dry-run --tag v0.0.0

自检要点：管理台「系统配置 → 通用 → 版本与更新」填好清单地址后，
侧栏底部版本号旁应出现更新提示，且更新按钮不再提示「更新程序未响应」。
EOF

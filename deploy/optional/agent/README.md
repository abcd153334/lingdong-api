# ld-updater（宿主机更新程序）

一键更新的**执行者**。后端只写一条任务行，本程序领取后按固定顺序执行：
拉代码 → 体检 → 构建 → 备份 → 切换 → 校验，失败自动回滚。

## 为什么它不是后端的一部分

后端容器里没有 Docker 控制权，也不该有：把 `docker.sock` 挂进一个对外提供 API 的进程，
等于用一个 RCE 换一个按钮。所以执行者必须独立、在宿主机上、且只认本地白名单脚本。

## 为什么装到 `/opt/lingdong/updater`

一次更新会 `git checkout` 整个工作区。若 agent 脚本本身在仓库里，checkout 会在它
**执行途中**改写文件 —— bash 按字节偏移继续读，结果是 "unexpected EOF" 或执行到半截语句。
仓库里的 `deploy/optional/agent/` 只是发布源；运行副本必须在仓库之外，且不 `source` 仓库里任何文件
（`lib/common.sh` 会随版本变，而正在跑的 agent 是旧版本）。

## 安装

```bash
sudo bash deploy/ld.sh updater              # 推荐：自动带上当前部署目录
sudo bash deploy/ld.sh updater uninstall    # 卸载（任务表与备份不受影响）
```

等价的底层命令（要自己确认路径时）：

```bash
sudo bash deploy/optional/agent/install.sh --repo /opt/lingdong-api/app
```

注意 `install.sh` 的 `--repo` 默认值是 `/opt/lingdong/app`，而本书的部署目录是
`/opt/lingdong-api/app` —— 路径写错的表现是"装了但一直 `agent_offline`"，
所以优先用 `ld.sh updater`（它按当前脚本位置推导，不会错）。

前置（脚本会检查并在缺项时拒绝）：

| 项 | 要求 |
|---|---|
| Docker | Engine ≥ 24 + compose v2 |
| git | 能 `fetch` 远端（只读 deploy key 即可） |
| 生产目录 | **独立 clone**，不是开发目录 |
| `.env` | 已按 `deploy/.env.example` 生成（4 必填） |
| postgres | 容器已运行 |

## 工作方式

- 每 `INTERVAL`（默认 5 秒）查一次 `release_update_jobs` 里有没有 `queued` 任务。
- 准备镜像时**优先拉 GHCR 上的预构建镜像**（几十秒），拉不到才本地构建（3–8 分钟）。
  两条路都只经 `tag-release.sh` 一个入口 —— 它同时写 `.release`（version/commit/
  image_id/data_epoch/previous），另写一份就是两套真相。- 领取用一条 `UPDATE ... WHERE id=(SELECT ... FOR UPDATE SKIP LOCKED) RETURNING`，
  不靠"先查再写"，也不依赖 flock 来保证唯一（flock 只防同机两个进程）。
- 领取后启一个**独立子进程**每 10 秒刷新 `last_heartbeat_at`。
  必须是独立进程：构建是 3–8 分钟的阻塞调用，主循环在这期间刷新不了心跳，
  否则后端会在第 90 秒把任务判成"失联"，而机器其实在正常构建。
- 每 15 秒 upsert `release_agent_state`（后台据此回答"这台机器装没装更新程序"）。

## 状态与失败

| 状态 | 含义 | 谁来收尾 |
|---|---|---|
| `queued` | 已提交，等 agent 领取 | 60s 无人领 → 后端置 `failed(agent_offline)` |
| `fetching` / `building` / `backing_up` | 切换前 | 心跳停 90s → `failed(agent_lost)` |
| `switching` / `verifying` | 切换与校验 | 心跳停 90s–180s → `manual_required` |
| `success` / `failed` / `rolled_back` / `manual_required` | 终态 | — |

**没有"续跑"**：agent 重启后只继续轮询新任务。被中断的那一笔由后端按心跳判死。
理由是切换中断不可安全续跑（不知道容器停在新旧之间），猜一个比停下来更危险。

失败码：`tag_invalid`、`tag_not_found`、`commit_mismatch`、`dirty_worktree`、
`fetch_failed`、`checkout_failed`、`preflight_failed`、`build_failed`、`backup_failed`、
`disk_low`、`switch_failed`、`verify_failed`、`rollback_unavailable`、`rollback_failed`、
`data_epoch_changed`、`interrupted_during_switch`、`agent_offline`、`agent_lost`。

## 排障

```bash
journalctl -u ld-updater -f                    # 实时日志
systemctl status ld-updater                    # 最近一次退出原因
bash /opt/lingdong/updater/ld-updater.sh --dry-run --tag v2.2.0   # 只打印命令序列
```

| 现象 | 原因 | 处置 |
|---|---|---|
| 管理台一直"已提交，等待更新程序领取"，60s 后失败 | 没装 / 没启 / 连不上库 | `systemctl status ld-updater`，看 `journalctl` |
| `dirty_worktree` | 生产目录有未提交改动 | 查明来源（热修？手工改配置？）后再更新；**不要**用 `checkout -f` |
| `tag_not_found` | 远端没有这个 tag | 先 `git push --tags` |
| `commit_mismatch` | tag 被移动过 | 重新发布一个 tag；核对清单里的 `commit` |
| `rollback_unavailable` | 上一版镜像已被清理 | 恢复镜像保留策略（默认保留当前+上一版+2 个） |
| `manual_required(data_epoch_changed)` | 期间跑过数据迁移 | 按 `docs/deployment/upgrade.md` 做"镜像 + 数据"双回滚 |

## 升级 agent 自身

agent 的运行副本在 `/opt/lingdong/updater`，而仓库里的那份（`deploy/optional/agent/`）
会随每次应用升级一起更新。两者不一致时，agent **自己会同步**：

- 启动时发现不一致 → `WARN 更新程序与仓库版本不一致（旧 → 新），自动同步` → 退出，
  systemd 按 `Restart=always` 拉起新的一份；
- 一条更新任务跑完之后（刚才的 `checkout` 已经把工作区换成新 tag）→ 同样自愈。

所以正常情况下**什么都不用做**。需要人工介入的只有一种情况：新脚本语法检查没过
（`bash -n`），这时会打 `WARN 仓库里的更新程序语法不通过，保留当前版本` —— 旧版本
继续跑，问题留给人处理，不会把服务带进重启循环。

想强制重装一次（比如怀疑运行副本被改过）：

```bash
sudo bash deploy/ld.sh updater
```

它会重写运行副本、重写 unit、`daemon-reload`，然后 **restart**（不是 `enable --now`：
服务已在跑时 `start` 是 no-op，文件换了进程还是旧代码，症状是"重装了没效果"）。

判断有没有生效：

```bash
sha256sum deploy/optional/agent/ld-updater.sh /opt/lingdong/updater/ld-updater.sh  # 应相同
systemctl show -p ActiveEnterTimestamp ld-updater --value                          # 应是刚才
```

**刻意不做的事**：agent 不会在"正在执行一次更新"的过程中替换自己 —— 那段时间脚本
正在被读，换文件等于把脚下的地板抽掉。自愈只发生在启动瞬间和任务结束之后。

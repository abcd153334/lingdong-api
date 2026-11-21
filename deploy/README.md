# 灵动 API · 部署

> **三个数字**：3 个容器、1 个端口、0 个必填配置。
> 页面与 API 在同一个进程里（前端产物 `go:embed` 进二进制），所以没有独立的 nginx 容器，
> 也没有第二层转发头可配错。

```bash
git clone https://github.com/abcd153334/lingdong-api.git lingdong-api
cd lingdong-api
bash deploy/ld.sh up
```

看到 `PASS 深探通过` 就完成了。打开 `http://127.0.0.1:8088`，用**出厂账号 `admin` / `admin123`** 登录。

这个口令是写死在代码里的常量（`db.defaultBootstrapPassword`），不需要翻日志、不需要找文件。
首个登录会被强制改密 —— 出厂口令只负责让你进门：

> 想跳过强制改密（CI、自动化建环境）：部署前 `export LD_BOOTSTRAP_ADMIN_PASSWORD='...'`，
> 只在**首次建号**时生效。

**改过之后忘了怎么办**：库里存的是 bcrypt，读不出原文。库还在就新建一个管理员重置；
库也没了就用备份恢复（见 §6）。

版本号不用手写：`ld.sh up` 会读当前 checkout 的 tag 注入二进制，`/api/health` 的
`version` 与 tag 一致；不带 tag 的 checkout（如直接下压缩包）才会落在 `dev`。

> 生产环境请务必在它前面加一层 TLS（见 §4）。`APP_ENV=prod` 下会话 Cookie 默认按请求协议
> 决定是否带 `Secure`，所以明文入口**能**登录；但会话票据会以明文过链路，只适合首次验证。

---

## 1. 它长什么样

| 容器 | 作用 | 对外 |
|---|---|---|
| `lingdong-backend` | Go 单进程：页面 + `/api` + `/v1` + `/f/` | `127.0.0.1:8088` |
| `lingdong-postgres` | 业务库（命名卷 `pgdata`） | 仅 compose 内网 |
| `lingdong-redis` | 限流桶、验证码、worker leader 锁（命名卷 `redisdata`） | 仅 compose 内网 |

- **只有一个对外端口** `127.0.0.1:8088`（`.env` 的 `APP_PORT` 可改），且只绑回环 ——
  对外服务必须在它前面放一层终止 TLS 的入口。
- 上传/产物落 `storagedata` 卷（容器内 `/app/data`）。
- 配置只有一个文件：`deploy/.env`，由 `ld.sh init` 生成（3 个随机密钥，权限 600）。
  **它一旦丢失，库内密文（渠道密钥、托管密钥、直链签名）就解不开了** —— 请与数据库备份一起保管。

---

## 2. 日常命令

```bash
bash deploy/ld.sh up            # 启动 / 应用改动（自动生成 .env、构建、等健康）
bash deploy/ld.sh up --pull     # 用发布镜像启动（不打 tag 时拉 :latest，几十秒；
                                #  拉不到会自动退回本地构建，国内网络不必先祈祷）
bash deploy/ld.sh ps            # 容器状态
bash deploy/ld.sh logs          # 跟踪 backend 日志（默认）
bash deploy/ld.sh health        # 浅探 + 深探（DB/Redis 真可用）
bash deploy/ld.sh restart       # 重启应用容器（不重建）
bash deploy/ld.sh down          # 停止（保留卷与数据）
bash deploy/ld.sh preflight     # 部署前体检（可选）
```

底层就是 `docker compose -f deploy/docker-compose.yml`，没有额外守护进程。

---

## 3. 配置

`ld.sh init` 生成的 `.env` 只有 6 行，全部自动填好：

| 键 | 说明 |
|---|---|
| `APP_ENV` | `prod`（默认）/`dev`/`test`。缺省会拒绝启动 |
| `POSTGRES_PASSWORD` | 库口令，同时用于派生 `DATABASE_URL`；只允许 `A-Za-z0-9_-` |
| `REDIS_PASSWORD` | Redis 口令（限流键与 leader 锁都在里面） |
| `APP_ENCRYPTION_KEY` | 库内密文主密钥，**全节点同值、与备份分开保管** |
| `LD_COOKIE_SECURE` | `auto`（默认）/`true`/`false`：`auto` = https 才给 Cookie 加 `Secure` |
| `APP_PORT` | 对外端口，默认 8088 |

其余开关（时区、可信代理、跨域、指标、留存、并发）都是可选，键名与默认值见
[`.env.example`](.env.example) —— 它是字典，不需要复制。

三条容易踩的：

1. **`LD_TRUSTED_PROXIES` 必须含 `127.0.0.1/32`**（编排默认已含）。入口在宿主机上时，
   容器看到的来源是回环；不信任它，所有公网用户会被当成同一个 IP，限流与登录锁定互相误伤。
2. **`APP_ENCRYPTION_KEY` 不要改**。改了口令只需重建库；改了密钥，库还在但密文全废。
3. **数据库不发布宿主端口**。排障用 `docker compose exec postgres psql -U lingdong -d lingdong`。

---

## 4. 对外提供服务（二选一）

两条路的共同点：**在 443 上终止 TLS，然后把请求原样转给 `http://127.0.0.1:8088`**。
gzip、静态缓存头、安全响应头都由应用自己下发，入口层不需要再配一遍
（重复配置只会出现两套真相源）。

### A. 宝塔面板反代（已有面板的机器）

1. 网站 → 添加站点（纯静态即可，PHP 选「静态」）→ 申请 Let's Encrypt 证书 → 开强制 HTTPS。
2. 网站 → 设置 → 反向代理 → 目标 `http://127.0.0.1:8088`，发送域名 `$http_host`。
3. 面板生成的 `/www/server/panel/vhost/nginx/proxy/<域名>/proxy.conf` 会被面板改写，
   建议直接用仓里的模板整体覆盖（含超时、请求体上限、`X-Forwarded-Host`）：

```bash
F=/www/server/panel/vhost/nginx/proxy/<域名>/proxy.conf
cp "$F" "$F.bak.$(date +%s)"
sed -e 's|__UPSTREAM__|http://127.0.0.1:8088|g' deploy/edge/bt/proxy.conf.template > "$F"
/www/server/nginx/sbin/nginx -t && /www/server/nginx/sbin/nginx -s reload
```

细节（为什么单独给 `/api/icons/` 一段、为什么要 `$http_host`）见
[`deploy/edge/bt/README.md`](edge/bt/README.md)。

### B. 独立 nginx（干净机器）

```bash
apt install -y nginx certbot
sed -e 's|__DOMAIN__|api.example.com|g' \
    -e 's|__WEBROOT__|/var/www/letsencrypt|g' \
    -e 's|__UPSTREAM__|http://127.0.0.1:8088|g' \
    deploy/edge/nginx/lingdong.tls.conf.template > /etc/nginx/conf.d/lingdong.conf
mkdir -p /var/www/letsencrypt
cp deploy/edge/nginx/snippets/*.conf /etc/nginx/snippets/
nginx -t && nginx -s reload
certbot certonly --webroot -w /var/www/letsencrypt -d api.example.com
nginx -s reload
curl -sS https://api.example.com/api/health
```

HSTS 建议先不加、跑通后再加（浏览器记住一年，撤回只能 `max-age=0`）。细节见
[`deploy/edge/README.md`](edge/README.md)。

> **不要把 8088 直接开放到公网**。它是明文口；正确姿势永远是在它前面放 TLS。

---

## 5. 备份与恢复

```bash
bash deploy/ld.sh backup
# → deploy/backups/<时间戳>/ 下：dump、sha256、指纹校验单、本地产物包

bash deploy/scripts/restore.sh deploy/backups/<时间戳>/xxx.dump \
     --target-dsn 'postgres://lingdong:<口令>@postgres:5432/lingdong_drill?sslmode=disable' --i-know
```

恢复脚本 **必须**给 `--target-dsn`（默认不碰生产库）和 `--i-know`（承认会清空目标库），
并在恢复后逐项比对 9 项账务指纹（用户数/余额/冻结/流水条数/流水金额/消耗……）。

> 为什么非要这 9 个数：`sha256` 只能证明"文件没坏"，证明不了"钱是对的"。
> 这是一个计费系统，恢复后余额与流水对不上，比恢复失败更难发现。

`APP_ENCRYPTION_KEY` 不在数据库里：备份文件 + `.env` 才是完整的一套。

---

## 6. 升级与回滚

镜像标签即版本。`/api/health` 的 `version` 必须等于标签（构建时用 `-X` 注入），
否则「回滚后跑的是哪一版」又变成靠猜。

### A. 命令行（适合脚本与无人值守）

```bash
# 用发布镜像升级 / 回滚（推荐）：本地没有该 tag 会自动拉，拉不到会明确报错且不做任何变更
bash deploy/scripts/upgrade.sh <TAG> --app-only

# 自己构建（改了代码 / 离线）
bash deploy/scripts/tag-release.sh <TAG>       # 构建 + 记账到 deploy/.release
bash deploy/scripts/upgrade.sh <TAG> --app-only
```

`upgrade.sh` 的顺序是：体检 → 备份（失败即中止）→ 只切应用容器 → 等健康并**核对版本 + 深探 DB**。
失败会直接打印可执行的回滚命令。

回滚：`bash deploy/scripts/upgrade.sh <上一版> --app-only`。
若这一步提示 `data_epoch` 变了（跑过数据迁移），只回退镜像不够，必须连带恢复备份。

### B. 管理台一键更新（先装一次执行器）

装一次就够了，之后在管理台点按钮：

```bash
sudo bash deploy/ld.sh updater              # 装（自动带上当前部署目录，不用记路径）
sudo bash deploy/ld.sh updater uninstall    # 卸（任务表与备份不受影响）
```

侧栏底部点版本号 → 「版本与更新」。「立即更新」能不能点由后端判定，
灰着时弹窗会直接写原因：清单没配 / 执行器离线 / 已是最新 / 有任务在跑 / 版本号不可比。

执行器的执行序列（失败自动回滚）：

```
fetch → 校验 tag → 查脏工作区 → 记回退镜像 → 磁盘检查
→ checkout → 体检 → 准备镜像（优先拉预构建，拉不到才本地构建）
→ 备份 → 只切应用容器 → 核对版本 + 深探 DB → 比对 data_epoch
```

**发布清单**是"有没有新版本"的唯一来源。出厂默认指向官方清单，所以克隆部署的实例
开箱即可检查更新；fork 后想跟自己的发布，在「系统配置 → 版本与更新」里换地址，
填 `off` 则彻底关闭（连自动检查也不出站）。

### C. 发一个版本（开发者）

```bash
git tag v2.2.8 && git push origin v2.2.8
```

推 `v*` 标签会触发一次构建，同时产出两样东西：

| 产物 | 位置 |
|---|---|
| 镜像 | `ghcr.io/abcd153334/lingdong-api:v2.2.8` + `:latest`（amd64/arm64） |
| 清单 | `raw.githubusercontent.com/abcd153334/lingdong-api/release-manifest/release.json` |

**不要再手工跑 `gen-release-manifest.sh` 上传**：那是离线 / 自建发布渠道才需要的。
清单里 `commit` 与镜像的 `VERSION` 都取自同一个 tag，所以"清单说有新版"与
"拉到的镜像版本号"天然一致。

---

## 7. 常见问题

**登录接口 200，之后所有请求 401？**
会话 Cookie 带了 `Secure` 而你在用 http 访问。把 `LD_COOKIE_SECURE` 设回 `auto`（默认），
或给入口配上 TLS。`bash deploy/ld.sh preflight` 会把这种情况指出来。

**打开页面 502 / 连不上？**
先分清是入口错还是栈没起：

```bash
curl -sS http://127.0.0.1:8088/api/health?deep=1
```

本机通、域名不通 → 入口的上游写错了（应为 `http://127.0.0.1:8088`）。

**8088 被占用？**
改 `deploy/.env` 的 `APP_PORT`，再 `ld.sh up`。脚本和体检都读同一个键。

**所有用户的 IP 都是 127.0.0.1？**
`LD_TRUSTED_PROXIES` 少了 `127.0.0.1/32`（入口与容器同机时必须信任回环）。

**初始口令是什么？**
`admin` / `admin123`，写死在代码里（`backend/internal/db/seed.go` 的 `defaultBootstrapPassword`）。
只在**首次建号**时生效：账号已存在时 seed 走 `ON CONFLICT DO NOTHING`，改这个常量不会重置
任何已有账号的口令。

**改过之后忘了怎么办？**
口令存的是 bcrypt，从库里读不出原文。库还在就新建一个管理员重置（或从上一次备份恢复）；
`down -v` 重建过库的话，下个启动会重新建 admin，又是 `admin123`。

**「立即更新」按钮灰着？**
按钮能不能点由后端判定，弹窗会写原因，按原因处理：

| `blocked_reason` | 怎么办 |
|---|---|
| `not_configured` | 清单地址被设成了 `off`（留空即用官方清单） |
| `agent_offline` | 执行器没装或没起：`sudo bash deploy/ld.sh updater`，再 `systemctl status ld-updater` |
| `no_update` | 真的是最新版（`update_available=false`） |
| `manifest_error` | 抓不到清单：看 `error_kind`（`blocked` = 非 https/内网、`http_status` = 404 或重定向、`timeout` = 出站被拦） |
| `already_running` | 有一个任务在跑；排队中的可以取消，执行中的不行 |

**一键更新中途失败，站点是好的吗？**
是。更新只在最后一步切容器，之前任何一步失败都会把检出恢复原样、容器与库都没动。
切换后校验不过会自动回滚到上一版镜像；若期间跑过数据迁移（`data_epoch` 变了）则不自动回滚，
而是转 `manual_required` 并提示必须连带恢复备份 —— 因为那是"旧代码配新数据"。

**能不能多机部署？**
可以，但那是可选组件：[`deploy/optional/cluster/`](optional/cluster/docker-compose.node.yml)
（多节点共用一个库与一个 Redis，见 §3 关于 `APP_ENCRYPTION_KEY` 必须同值的说明）。

---

## 8. 目录说明

```
deploy/
  ld.sh                    入口（up/down/logs/health/backup/upgrade…）
  docker-compose.yml       唯一编排：3 个容器、1 个端口
  backend.Dockerfile       前端构建 → Go 构建（内嵌 dist）→ 运行镜像
  .env.example             变量字典（不是模板，别复制）
  scripts/
    init-env.sh            生成/补全 .env（幂等，不覆盖已有值）
    healthcheck.sh         浅探 + 深探 + 版本核对
    backup.sh restore.sh   备份与恢复（9 项账务指纹）
    upgrade.sh             体检 → 备份 → 切版本 → 核对
    tag-release.sh         准备镜像（--pull 优先拉）+ 记账到 .release
    preflight.sh           部署前体检（可选）
    gen-release-manifest.sh  手工生成发布清单（只在离线 / 自建发布渠道时需要）
  edge/                    两种正式入口的模板（宝塔 / 独立 nginx）
  optional/                agent（管理台一键更新）、cluster（多机）—— 单机不需要
```

发布镜像由 GitHub Actions 在打 tag 时构建并推送到
`ghcr.io/abcd153334/lingdong-api`（amd64 + arm64），**同一次构建还会把发布清单
推到 `release-manifest` 分支**，见 [`.github/workflows/docker.yml`](../.github/workflows/docker.yml)。

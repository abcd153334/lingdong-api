# 宝塔面板「反向代理」配置模板（正式入口 A）

把本文件内容替换进宝塔生成的 `proxy.conf`，即可让面板反代到本系统的 8088 端口。

## 为什么不能直接用面板默认值

面板「反向代理」的四个默认值，在本系统上都会造成**用户可见的故障**，而且都不报错：

| 配置 | 面板默认 | 必须改成 | 不改的后果 |
|---|---|---|---|
| `proxy_read_timeout` | 60s | `960s` | 同步等图的请求固定第 60 秒收到 nginx 的 504 HTML，后端日志里什么都没有（钱已扣、图已出） |
| `proxy_send_timeout` | 60s | `960s` | 大响应/流式回程被分段掐断 |
| `proxy_buffering` | on | `off` | 图片与 SSE 被缓冲，首字节延迟变成"整包延迟" |
| `client_max_body_size` | 1m | `256m` | 参考图超过 1 MiB 直接被 413，后端看不到请求 |

另外两处不是"性能"，是**功能错误**：

- `X-Forwarded-Host` 必须传 `$http_host`（不是 `$host`）：面板默认不传这个头，
  后端拼出来的 `icon_url` / 产物直链 / 文档示例会退回配置里的第一条地址，
  多线路部署下就是跨线路拉图、跨线路下载。用 `$http_host` 是因为 `$host` 会砍掉端口，
  非 443 端口访问时 CSRF 的完整 origin 比对必然失败（登录 403）。
- `/api/icons/` 必须单独一段且**不加任何 `add_header`**：图标路由没有扩展名，
  会被面板那条「按扩展名判静态」的规则追加 `Cache-Control: no-cache`，
  覆盖后端下发的一年强缓存 —— 每开一次弹窗几十张图全部回源。

## 占位符

| 占位符 | 填什么 | 示例 |
|---|---|---|
| `__UPSTREAM__` | 本系统入口地址 | `http://127.0.0.1:8088` |
| `__DOMAIN__` | 对外域名（仅用于注释与日志名） | `api.example.com` |

## 用法

```bash
# 1) 面板：网站 → 添加站点（纯静态，不要 PHP）→ 设置 → 反向代理 → 目标 http://127.0.0.1:8088
# 2) 面板：SSL → Let's Encrypt → 申请 → 开「强制 HTTPS」
# 3) 用本模板覆盖面板生成的文件（路径随域名变化）
F=/www/server/panel/vhost/nginx/proxy/__DOMAIN__/proxy.conf
cp "$F" "$F.bak.$(date +%s)"
sed -e 's|__UPSTREAM__|http://127.0.0.1:8088|g' \
    -e 's|__DOMAIN__|api.example.com|g' deploy/edge/bt/proxy.conf.template > "$F"
# 4) 校验并重载
/www/server/nginx/sbin/nginx -t && /www/server/nginx/sbin/nginx -s reload
```

> 面板会在你每次"点开反向代理再保存"时**重写这个文件**。改完必须 `nginx -t` +
> 一次真实请求复核；升级面板/迁移站点后也要复看一遍。

## 验收

```bash
curl -sSI https://__DOMAIN__ | head -1                  # 200/301，且有 Strict-Transport-Security
curl -sS  https://__DOMAIN__/api/health                 # version 必须等于 deploy/.release 的 version
curl -sS  "https://__DOMAIN__/api/diag/hold?seconds=90" # 需 LD_DIAG_HOLD_TOKEN；90s 不被掐断
```

浏览器再走一遍：登录 → 建 KEY → `GET /v1/models`。登录能进但立刻 401，
说明站点没开强制 HTTPS（prod 的会话 Cookie 带 `Secure`）。

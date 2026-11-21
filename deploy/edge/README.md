# 入口层（TLS / 反代 / 证书 / 超时）

这一层只解决一件事：**把明文 HTTP 变成可信的 HTTPS，并且不在长请求上掐断连接**。
它不在 `docker-compose.yml` 里，因为它取决于你把机器放在哪。

## 先记住两个数字

| 数字 | 是什么 |
|---|---|
| `8088` | 本系统**唯一对外端口**（宿主 `127.0.0.1:8088` → 应用的 8080），`deploy/.env` 的 `APP_PORT` 可改 |
| `443` | 用户真正访问的端口，由下面两种入口之一提供 |

**不要再给入口层配静态缓存、gzip、安全响应头**：这些由应用自己下发（`internal/webui`），
入口层只做 TLS 与转发。配两遍只会出现两套真相源 —— 以前那套 nginx 配置就因此
同时存在"双份 Cache-Control"和"nosniff 被 `add_header` 中断"两个 bug。

## 两种正式入口，选一种

| | **A. 宝塔面板反代** | **B. 独立 nginx（本仓模板）** |
|---|---|---|
| 适用 | 机器上已经装了宝塔，站点点几下就能建 | 干净的云主机，不想装面板 |
| 配置 | 面板「网站 → 反向代理」→ `http://127.0.0.1:8088`，再用本仓模板覆盖面板生成的文件 | `deploy/edge/nginx/*.template` + `certbot` |
| 文件 | [`bt/`](bt/README.md)、[`bt/proxy.conf.template`](bt/proxy.conf.template) | [`nginx/lingdong.tls.conf.template`](nginx/lingdong.tls.conf.template)、[`nginx/snippets/`](nginx/snippets/) |
| 证书 | 面板「SSL → Let's Encrypt」一键 | `certbot certonly --webroot` |
| 谁管配置 | 面板会重写，改完要复核 | 你自己，副本即真相 |

**A 的关键动作只有一个**：把面板默认的四个值改掉（超时 960s、关 buffering、
请求体 256m、透传 `X-Forwarded-Host $http_host`）。细节见 [`bt/README.md`](bt/README.md)，
那里逐条写了"不改的后果"。

**B 的关键动作是替换三个占位符**：

```bash
# __DOMAIN__   对外域名
# __WEBROOT__  ACME webroot 目录（如 /var/www/letsencrypt）
# __UPSTREAM__ http://127.0.0.1:8088        ← 就是本系统的入口
sed -e 's|__DOMAIN__|api.example.com|g' \
    -e 's|__WEBROOT__|/var/www/letsencrypt|g' \
    -e 's|__UPSTREAM__|http://127.0.0.1:8088|g' \
    deploy/edge/nginx/lingdong.tls.conf.template > /etc/nginx/conf.d/lingdong.conf
cp deploy/edge/nginx/snippets/*.conf /etc/nginx/snippets/
nginx -t && nginx -s reload
```

> 两种入口都**不要**把 8088 直接对公网放开。它是明文口：没有证书，会话票据会明文过链路。
> `127.0.0.1` 绑定已经保证了只有本机入口能访问它。

## 文件说明

```
bt/                                 形态 A：宝塔面板（操作步骤 + proxy.conf 模板）
nginx/snippets/timeouts.conf        超时与体积的唯一来源
nginx/snippets/proxy-common.conf    反代头的唯一来源
nginx/lingdong.tls.conf.template   完整节点（页面 + API）
nginx/api.tls.conf.template        纯 API 节点（api 模式，域名根 404）
```

## 三条必须理解的规则

### 1. 超时是一个阶梯，不是一堆数字

```
客户端 SDK ≥ 960s  >  本层 proxy_read_timeout 960s  >  应用同步等待 780s（上限 900s）  >  单次上游 60s
```

- 外层必须**严格大于**内层（相等都不行）：相等时两边同时超时，谁先写响应是不确定的。
- 只改一层等于没改：外层 60s、内层 780s，用户仍然在第 60 秒收到一个 HTML 504。
- 每层改哪里：SDK = 调用方代码；本层 = `snippets/timeouts.conf`（宝塔见 `bt/proxy.conf.template`）；
  应用 = `LD_DUOMI_SYNC_IMAGE_WAIT_SEC`；上游 = 适配器。

应用进程本身**不设** `WriteTimeout`（`main.go` 里写明了原因：同步等图要 780s）。

### 2. `Host` 与 `X-Forwarded-Host` 各管一件事

- `Host`：告诉上游"请求发给谁"。直连本系统时传 `$http_host`。
- `X-Forwarded-Host`：告诉后端"用户实际访问的是什么"。**一律 `$http_host`**，不能是 `$host`：
  `$host` 会砍掉端口，非 443 端口访问时会话/CSRF 的完整 origin 比对必然失败（登录 403），
  多线路部署下图标与产物直链也会指错线路。
- `X-Forwarded-Proto` 必须传：`LD_COOKIE_SECURE=auto` 靠它判断是否给 Cookie 加 `Secure`；
  对外地址推导也靠它判 scheme（不传就一律算成 http，https 页面里下发 http 图片地址会被浏览器拦）。

### 3. HTTP/3 必须关

nginx 的 QUIC 空闲超时固定 60s，**没有指令可以调大**。
同步等图的 780s 里一个字节都不写，走 h3 的客户端会在第 60 秒被掐断。
API 域名开 h3 零收益：关掉 `listen 443 quic` / `http3 on` / `Alt-Svc` 即可；
已经缓存过 Alt-Svc 的客户端会自动回退 TCP。

## 上线顺序（HSTS 尤其）

1. 先只开 80，确认 ACME 能签发（`/.well-known/acme-challenge/` 可达）。
2. 开 443 + `http2 on`，**先不加 HSTS**，用 `curl -I https://域名` 与浏览器各验一遍。
3. 再加 `Strict-Transport-Security: max-age=31536000; includeSubDomains`。
4. 出问题时只能 `max-age=0` 撤回——浏览器已经记住的那一年无法撤销，所以第 2 步不能省。

## 验证

```bash
curl -sS http://127.0.0.1:8088/api/health        # 本机入口本身通（不经过 TLS 层）
nginx -t                                         # 语法
curl -sSI http://域名 | head -1                  # 301 → https
curl -sSI https://域名 | grep -i strict-transport
curl -sS  https://域名/api/health                # 200，version 与 deploy/.release 一致
# 长请求不被掐断（需要 LD_DIAG_HOLD_TOKEN）
curl -sS "https://域名/api/diag/hold?seconds=120"
```

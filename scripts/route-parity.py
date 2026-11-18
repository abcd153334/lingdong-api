#!/usr/bin/env python3
"""new-api 中继面路由对账（路由级全量差分）。

为什么需要它
------------
「还有没有漏掉的端点」这个问题，如果靠人翻 new-api 源码回答，就永远没有终点：
new-api 是活跃项目，每次翻都会看到新条目，而其中大部分是我们**不应该**实现的
（Midjourney 专用协议、WebSocket 通道、它自己的 Web 测试台）。人工对比没有
"完成"这个状态。

这个脚本把问题变成一次可复现的集合运算，并把**已判定**的差异固化成基线：

  new-api 全部路由  −  new-api 自己返回 501 的  =  真正要对齐的集合（reference）
  reference  −  本平台已注册路由              =  缺口（gap）
  gap  −  基线里已判定条目                     =  **未判定差异**（退出码 1）

于是"兼容做完了吗"有了客观答案：未判定差异为 0。新增差异不会静默存在，
它要么被实现，要么在基线里写清类别与理由。

用法
----
    python scripts/route-parity.py
    python scripts/route-parity.py --new-api /path/to/new-api --verbose

退出码：0 = 全部差异都已判定；1 = 存在未判定差异；2 = 参考源码不可读。

只读：本脚本不修改任何业务代码。
"""

import argparse
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

# new-api 的 gin 组变量 -> URL 前缀。
# 必须手工维护而不是从 Group(...) 推导：`relayV1Router.Group("")` 这种空前缀
# 推导出来是 ""，会把 httpRouter 下所有路由错算成不带 /v1 —— 第一版脚本就
# 因此把 `POST /chat/completions` 报成了缺口（假阳性会让整个对账失去可信度）。
GROUP_PREFIX = {
    "router": "",
    "modelsRouter": "/v1/models",
    "geminiRouter": "/v1beta/models",
    "geminiCompatibleRouter": "/v1beta/openai/models",
    "playgroundRouter": "/pg",
    "relayV1Router": "/v1",
    "wsRouter": "/v1",
    "httpRouter": "/v1",
    "relayMjRouter": "/mj",
    "relayMjModeRouter": "/:mode/mj",
    "relayGeminiRouter": "/v1beta",
}

ROUTE_RE = re.compile(r'(\w+)\.(GET|POST|PUT|DELETE|PATCH|Any)\("([^"]*)"\s*,\s*(.*)')


def norm(path):
    """把契约写法与 gin 写法统一成可比较形态。

    `*path` / `:model` / `{model}` 都是"一个占位段"，统一成 `:p`；
    否则 `/v1/models/*path` 与 `/v1/models/{model}` 会被算成两条不同的路由。
    """
    path = re.sub(r"\*\w+", ":p", path)
    path = re.sub(r":\w+", ":p", path)
    path = re.sub(r"\{[^}]+\}", ":p", path)
    return path.rstrip("/") or "/"


def newapi_routes(src_path):
    """解析 new-api 的 relay-router.go，返回 (全部, 真实实现)。

    "真实实现"= handler 不是 RelayNotImplemented。这一层过滤是整份对账的
    前提：new-api 自己也把 files / fine-tunes / variations / DELETE models
    挂在 `controller.RelayNotImplemented`（回 501）。把那些算成"我们要补的
    缺口"，就是拿一个别人也没做的功能要求自己。
    """
    with open(src_path, encoding="utf-8", errors="replace") as f:
        lines = f.read().split("\n")

    rows, cur = [], None
    for ln in lines:
        m = ROUTE_RE.search(ln)
        if m:
            cur = (m.group(1), m.group(2), m.group(3), m.group(4))
            if m.group(4).strip():
                rows.append(cur)
                cur = None
            continue
        if cur is not None:
            cur = cur[:3] + (cur[3] + " " + ln.strip(),)
            if re.search(r"\}\)|\)\s*$", ln):
                rows.append(cur)
                cur = None
    if cur:
        rows.append(cur)

    all_routes, impl = {}, {}
    for recv, method, path, handler in rows:
        prefix = GROUP_PREFIX.get(recv)
        if prefix is None:  # 未登记的组：宁可报错也不猜前缀
            raise SystemExit("未知的路由组变量 %r（请补 GROUP_PREFIX）" % recv)
        key = (method.upper(), norm(prefix + path))
        all_routes[key] = handler
        if "RelayNotImplemented" not in handler:
            impl[key] = handler
    return all_routes, impl


def our_routes(router_go):
    """解析本平台的对外路由（/v1、/v1beta、/dashboard 三个前缀）。"""
    with open(router_go, encoding="utf-8") as f:
        src = f.read()
    out = set()
    prefixes = {"v1": "/v1", "beta": "/v1beta", "dash": "/dashboard"}
    for m in re.finditer(r'\b(v1|beta|dash)\.(GET|POST|PUT|DELETE|PATCH|Any)\("([^"]*)"', src):
        out.add((m.group(2).upper(), norm(prefixes[m.group(1)] + m.group(3))))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--new-api", default=os.path.join(ROOT, "new-api"),
                    help="new-api 仓库根目录")
    ap.add_argument("--router", default=os.path.join(ROOT, "backend", "internal", "router", "router.go"))
    ap.add_argument("--baseline", default=os.path.join(HERE, "route-parity-baseline.json"))
    ap.add_argument("--verbose", action="store_true", help="打印超集与 new-api 自身 501 的条目")
    args = ap.parse_args()

    src = os.path.join(args.new_api, "router", "relay-router.go")
    if not os.path.isfile(src):
        print("参考源码不存在: %s" % src, file=sys.stderr)
        print("（new-api 是外部参考仓库，不在本仓库内；需要它才能对账）", file=sys.stderr)
        return 2

    na_all, na_impl = newapi_routes(src)
    ours = our_routes(args.router)

    gap = sorted(set(na_impl) - ours)
    extra = sorted(r for r in (ours - set(na_all)) if not r[1].startswith("/api/"))

    baseline = {}
    if os.path.isfile(args.baseline):
        with open(args.baseline, encoding="utf-8") as f:
            baseline = json.load(f).get("gaps", {})

    print("new-api 中继路由 %d 条（其中自身返回 501 的 %d 条，真实实现 %d 条）"
          % (len(na_all), len(na_all) - len(na_impl), len(na_impl)))
    print("本平台对外注册 %d 条" % len(ours))
    print("缺口 %d 条" % len(gap))
    print()

    unjudged = 0
    by_cat = {}
    for method, path in gap:
        key = "%s %s" % (method, path)
        b = baseline.get(key)
        if b:
            by_cat.setdefault(b["category"], []).append((key, b["reason"]))
            print("  [已判定:%-18s] %s" % (b["category"], key))
        else:
            unjudged += 1
            print("  [未判定]  %s" % key)

    if by_cat:
        print("\n按类别汇总：")
        for cat in sorted(by_cat):
            print("  %-20s %d 条" % (cat, len(by_cat[cat])))
            for key, reason in by_cat[cat]:
                print("      %s —— %s" % (key, reason))

    if args.verbose:
        print("\n本平台独有（new-api 未注册，属平台自有能力）：%d 条" % len(extra))
        for m, p in extra:
            print("   %-6s %s" % (m, p))
        skipped = sorted(set(na_all) - set(na_impl))
        print("\nnew-api 自己返回 501（对齐=不做，做了反而超出）：%d 条" % len(skipped))
        for m, p in skipped:
            mark = "  ← 本平台已实现（超集）" if (m, p) in ours else ""
            print("   %-6s %s%s" % (m, p, mark))

    print()
    if unjudged:
        print("结果：%d 条差异未判定。请在 %s 里登记 category 与 reason，"
              "或实现它。" % (unjudged, os.path.basename(args.baseline)))
        return 1
    print("结果：全部差异已判定（缺口 %d 条，均有类别与理由）。" % len(gap))
    return 0


if __name__ == "__main__":
    sys.exit(main())

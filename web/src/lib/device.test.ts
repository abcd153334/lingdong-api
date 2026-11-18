import { describe, expect, it } from "vitest";
import { timeAgo, uaLabel } from "./device";

describe("uaLabel", () => {
  const CHROME_WIN =
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0 Safari/537.36";
  const SAFARI_MAC =
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15";
  const EDGE_WIN =
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0 Safari/537.36 Edg/120.0";
  const CHROME_ANDROID =
    "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/122.0 Mobile Safari/537.36";
  const CHROME_IOS =
    "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 CriOS/122.0 Mobile/15E148 Safari/604.1";
  const FIREFOX_LINUX = "Mozilla/5.0 (X11; Linux x86_64; rv:124.0) Gecko/20100101 Firefox/124.0";

  it("识别常见桌面浏览器与系统", () => {
    expect(uaLabel(CHROME_WIN)).toBe("Chrome · Windows");
    expect(uaLabel(SAFARI_MAC)).toBe("Safari · macOS");
    expect(uaLabel(FIREFOX_LINUX)).toBe("Firefox · Linux");
  });

  // 全部真实 UA 互相包含：Edge 也含 "Chrome/"、Chrome 也含 "Safari/"。
  // 判定顺序一旦写反，全世界都会显示成 Safari —— 不报错，只是永远认错。
  it("包含关系不被误判（Edge 不会显示成 Chrome，Chrome 不会显示成 Safari）", () => {
    expect(uaLabel(EDGE_WIN)).toBe("Edge · Windows");
    expect(uaLabel(CHROME_WIN)).toBe("Chrome · Windows");
  });

  // Android 的 UA 里带 "Linux"、iOS 的带 "Mac OS X"：先判手机系统才不会被认成桌面
  it("手机系统优先于其 UA 里的桌面系统字样", () => {
    expect(uaLabel(CHROME_ANDROID)).toBe("Chrome · Android");
    expect(uaLabel(CHROME_IOS)).toBe("Chrome · iOS");
  });

  it("认不出也不返回空串（空串在列表里是一行空白）", () => {
    expect(uaLabel("")).toBe("未知设备");
    expect(uaLabel("   ")).toBe("未知设备");
    expect(uaLabel("totally-unknown-agent")).toBe("未知浏览器 · 未知系统");
    expect(uaLabel("curl/8.4.0")).toBe("命令行 · 未知系统");
  });
});

describe("timeAgo", () => {
  const now = Date.parse("2026-01-01T12:00:00Z");
  const at = (secondsAgo: number) => new Date(now - secondsAgo * 1000).toISOString();

  it("按区间分段", () => {
    expect(timeAgo(at(10), now)).toBe("刚刚");
    expect(timeAgo(at(3 * 60), now)).toBe("3 分钟前");
    expect(timeAgo(at(5 * 3600), now)).toBe("5 小时前");
    expect(timeAgo(at(3 * 86400), now)).toBe("3 天前");
  });

  // 时钟漂移下 last_seen_at 可能比本地 now 还新，显示「-2 分钟前」比不精确更糟
  it("未来时间（时钟漂移）按刚刚处理", () => {
    expect(timeAgo(at(-120), now)).toBe("刚刚");
  });

  it("超过 30 天回落为日期；非法输入返回占位符", () => {
    expect(timeAgo(at(40 * 86400), now)).not.toMatch(/前$/);
    expect(timeAgo("not-a-date", now)).toBe("—");
  });
});

import { describe, expect, it } from "vitest";
import { initialFavicon, openaiBase } from "@/lib/site";

/**
 * 对外地址与图标的**纯函数**契约。
 *
 * useSite() 的兜底链（配置 → 站点地址 → origin）由后端 `siteurl.DisplayBases`
 * 决定，前端只做展示；这里覆盖的是「拿到 origin 之后怎么变成用户可见的字符串」，
 * 也就是最容易在复制按钮上出错的那两步。
 */
describe("openaiBase", () => {
  it("origin 补 /v1", () => {
    expect(openaiBase("https://api.x.com")).toBe("https://api.x.com/v1");
  });

  it("已带 /v1 时不重复追加（幂等）", () => {
    expect(openaiBase("https://api.x.com/v1")).toBe("https://api.x.com/v1");
    expect(openaiBase("https://api.x.com/v1/")).toBe("https://api.x.com/v1");
  });

  it("尾斜杠/空白先归一", () => {
    expect(openaiBase("  https://api.x.com/  ")).toBe("https://api.x.com/v1");
  });

  it("空值回空串（不产出写死的域名）", () => {
    expect(openaiBase("")).toBe("");
    expect(openaiBase("   ")).toBe("");
  });

  it("带端口的 origin 原样保留", () => {
    expect(openaiBase("http://127.0.0.1:6000")).toBe("http://127.0.0.1:6000/v1");
  });
});

describe("initialFavicon", () => {
  it("中文站名取首字，产出可解析的 svg dataURI", () => {
    const uri = initialFavicon("灵动 API");
    expect(uri.startsWith("data:image/svg+xml,")).toBe(true);
    expect(decodeURIComponent(uri.slice("data:image/svg+xml,".length))).toContain(">灵<");
  });

  it("空站名回落为单个字符而不是空文本节点", () => {
    const svg = decodeURIComponent(initialFavicon("").slice("data:image/svg+xml,".length));
    expect(svg).toContain(">A<");
  });

  it("XML 元字符被转义（否则整段 svg 解析失败，标签页无图标）", () => {
    const svg = decodeURIComponent(initialFavicon("<").slice("data:image/svg+xml,".length));
    expect(svg).toContain("&lt;");
    expect(svg).not.toContain(">>");
  });
});

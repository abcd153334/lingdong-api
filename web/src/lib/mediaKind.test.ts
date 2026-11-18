import { describe, expect, it } from "vitest";

import { canRender, kindOfMime, mimeOfUrl, resolveMediaKind } from "./mediaKind";

/**
 * 媒体判型是「预览显示错」的唯一根因修复点，必须锁死三条规则：
 * MIME 优先、扩展名次之、后端提示最后；SVG 一律按文件（不内联）。
 */
describe("mediaKind", () => {
  it("MIME 优先于扩展名与提示", () => {
    expect(resolveMediaKind("video", "image/png", "https://x/a.mp4")).toBe("image");
  });

  it("没有 MIME 时看扩展名（视频端点回 jpg 封面）", () => {
    expect(resolveMediaKind("video", "", "https://x/cover.jpg")).toBe("image");
  });

  it("都没有才用后端提示", () => {
    expect(resolveMediaKind("audio", "", "https://x/download/1")).toBe("audio");
    expect(resolveMediaKind("", "", "")).toBe("file");
  });

  it("签名链接 query 里的假后缀不算", () => {
    expect(mimeOfUrl("https://x/dl/1?name=a.png&sig=1")).toBe("");
  });

  it("扩展名大小写不敏感", () => {
    expect(mimeOfUrl("https://x/A.PNG")).toBe("image/png");
  });

  it("SVG 与 PDF 不内联", () => {
    expect(kindOfMime("image/svg+xml")).toBe("file");
    expect(resolveMediaKind("image", "image/svg+xml", "https://x/a.svg")).toBe("file");
    // PDF 不在渲染白名单里，落到「文件」→ 只给下载。
    expect(resolveMediaKind("file", "application/pdf", "https://x/a.pdf")).toBe("file");
  });

  it("JSON 按文本预览", () => {
    expect(kindOfMime("application/json")).toBe("text");
  });

  it("带参数的 MIME 能识别", () => {
    expect(kindOfMime("text/plain; charset=utf-8")).toBe("text");
  });

  it("只有图/视频/音频/文本可在页面内渲染", () => {
    expect(canRender("image")).toBe(true);
    expect(canRender("text")).toBe(true);
    expect(canRender("file")).toBe(false);
  });
});

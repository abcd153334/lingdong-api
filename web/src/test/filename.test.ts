import { describe, expect, it } from "vitest";

import { filenameFrom } from "@/lib/apiClient";

// 下载文件名的解析规则（RFC 6266）。这条用例的意义：
// 后端为了兼容只会填 ASCII 的客户端，同时下发
// `filename="plugin-dev-manual.md"; filename*=UTF-8''%E5%BA%94...md`，
// 若不按规范优先取 filename*，中文名会被英文兜底名顶掉（曾经就是这样）。
describe("Content-Disposition 文件名", () => {
  it("同时存在时 filename* 优先", () => {
    const d =
      "attachment; filename=\"plugin-dev-manual.md\"; filename*=UTF-8''%E5%BA%94%E7%94%A8%E6%8F%92%E4%BB%B6%E5%BC%80%E5%8F%91%E6%89%8B%E5%86%8C.md";
    expect(filenameFrom(d)).toBe("应用插件开发手册.md");
  });

  it("只有 filename= 时用它（并去掉引号）", () => {
    expect(filenameFrom('attachment; filename="ticket.csv"')).toBe("ticket.csv");
    expect(filenameFrom("attachment; filename=ticket.csv")).toBe("ticket.csv");
  });

  it("没有头时返回 null", () => {
    expect(filenameFrom(null)).toBeNull();
    expect(filenameFrom("attachment")).toBeNull();
  });

  it("filename* 编码坏了退回 ASCII 名，不让下载整体失败", () => {
    expect(filenameFrom("attachment; filename=\"a.csv\"; filename*=UTF-8''%E5%ZZ")).toBe("a.csv");
  });
});

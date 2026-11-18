import { afterEach, describe, expect, it, vi } from "vitest";

import { formatBytes, prettifyJSON, probeRequest } from "./probe";
import type { BuiltRequest } from "./request";

const req: BuiltRequest = {
  method: "GET",
  url: "/api/user/keys?page=1",
  headers: { Accept: "application/json", Authorization: "Bearer <你的密钥>" },
  missing: [],
};

/** 造一个最小 Response；只需要头、体、状态。 */
function res(body: string, init: { status?: number; type?: string; bytes?: Uint8Array } = {}) {
  // `.slice()` 返回 `Uint8Array<ArrayBuffer>`（而不是 `<ArrayBufferLike>`）：
  // Response 的 BodyInit 在 DOM lib 里不收可能基于 SharedArrayBuffer 的视图。
  // 这只是测试夹具的类型妥协，不影响被测逻辑。
  const payload: BodyInit = init.bytes ? init.bytes.slice().buffer : body;
  return new Response(payload, {
    status: init.status ?? 200,
    statusText: init.status === 403 ? "Forbidden" : "OK",
    headers: { "content-type": init.type ?? "application/json" },
  });
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("probeRequest", () => {
  it("不带 Cookie（representative 接入方调用，不是管理员会话）", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(res('{"code":0}'));
    await probeRequest(req, "sk-1");
    const init = spy.mock.calls[0][1]!;
    expect(init.credentials).toBe("omit");
    expect(init.cache).toBe("no-store");
    expect(init.method).toBe("GET");
  });

  it("Authorization 由 apiKey 决定，契约里的占位符不得泄漏到真实请求", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(res("{}"));
    await probeRequest(req, "  sk-real  ");
    const headers = spy.mock.calls[0][1]!.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer sk-real");
    expect(JSON.stringify(headers)).not.toContain("你的密钥");
  });

  it("apiKey 为空时不带 Authorization（免鉴权端点）", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(res("{}"));
    await probeRequest({ ...req, headers: { Accept: "application/json" } }, "");
    const headers = spy.mock.calls[0][1]!.headers as Record<string, string>;
    expect(headers.Authorization).toBeUndefined();
  });

  it("4xx 不抛错：状态码是结果的一部分", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(res('{"code":403}', { status: 403 }));
    const r = await probeRequest(req, "sk-bad");
    expect(r.ok).toBe(false);
    expect(r.status).toBe(403);
    expect(r.statusText).toBe("Forbidden");
    expect(r.body).toContain("403");
  });

  it("JSON 响应被美化，纯文本错误原样保留", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(res('{"a":1}'));
    expect((await probeRequest(req, "")).body).toBe('{\n  "a": 1\n}');

    vi.spyOn(globalThis, "fetch").mockResolvedValue(res("upstream unavailable", { type: "text/plain" }));
    expect((await probeRequest(req, "")).body).toBe("upstream unavailable");
  });

  it("二进制响应只报告大小，不塞进 DOM", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      res("", { type: "application/octet-stream", bytes: new Uint8Array([0x50, 0x4b, 0x03, 0x04]) }),
    );
    const r = await probeRequest(req, "");
    expect(r.bytes).toBe(4);
    expect(r.body).toContain("二进制内容");
    expect(r.body).toContain("4 B");
  });

  it("超大响应只给前 64KB 预览并注明总大小", async () => {
    const big = new Uint8Array(200 * 1024).fill(0x61);
    vi.spyOn(globalThis, "fetch").mockResolvedValue(res("", { type: "text/plain", bytes: big }));
    const r = await probeRequest(req, "");
    expect(r.bytes).toBe(200 * 1024);
    expect(r.body).toContain("200.0 KB");
    // 预览长度受控（含提示行），不会把 200KB 全塞进来
    expect(r.body.length).toBeLessThan(70 * 1024);
  });

  it("网络层失败向上抛（只有这种情况才抛）", async () => {
    vi.spyOn(globalThis, "fetch").mockRejectedValue(new TypeError("Failed to fetch"));
    await expect(probeRequest(req, "")).rejects.toThrow("Failed to fetch");
  });
});

describe("prettifyJSON", () => {
  it("合法 JSON 美化，非 JSON 原样", () => {
    expect(prettifyJSON('{"a":1}')).toBe('{\n  "a": 1\n}');
    expect(prettifyJSON("not json")).toBe("not json");
    expect(prettifyJSON("")).toBe("");
    // 半截 JSON（流被截断）：原样返回，不能吞掉
    expect(prettifyJSON('{"a":')).toBe('{"a":');
  });
});

describe("formatBytes", () => {
  it("按量级切换单位", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(2048)).toBe("2.0 KB");
    expect(formatBytes(5 * 1024 * 1024)).toBe("5.0 MB");
  });
});

describe("probeRequest · 鉴权头形态", () => {
  it("authHeader=x-api-key 时写 x-api-key，不写 Bearer", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(res("{}"));
    await probeRequest(
      { ...req, headers: { "x-api-key": "<你的密钥>" } },
      "sk-real",
      undefined,
      { authHeader: "x-api-key" },
    );
    const headers = spy.mock.calls[0][1]!.headers as Record<string, string>;
    expect(headers["x-api-key"]).toBe("sk-real");
    expect(headers.Authorization).toBeUndefined();
  });

  it("契约里的两个鉴权占位符都被剔除，真实请求只留一个", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(res("{}"));
    await probeRequest(
      { ...req, headers: { Authorization: "Bearer <你的密钥>", "x-api-key": "<你的密钥>" } },
      "sk-real",
    );
    const headers = spy.mock.calls[0][1]!.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer sk-real");
    expect(headers["x-api-key"]).toBeUndefined();
  });
});

describe("probeRequest · 二进制", () => {
  it("wantBinary 时媒体响应产出 blobUrl，不再把字节塞进文本", async () => {
    const bytes = new Uint8Array([0x89, 0x50, 0x4e, 0x47]);
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      res("", { type: "image/png", bytes }),
    );
    // jsdom 没有 createObjectURL：给出一个可断言的替身
    const spy = vi.fn(() => "blob:mock");
    Object.assign(URL, { createObjectURL: spy });

    const got = await probeRequest(req, "sk", undefined, { wantBinary: true });
    expect(got.blobUrl).toBe("blob:mock");
    expect(got.body).toContain("二进制内容");
    expect(spy).toHaveBeenCalledOnce();
  });

  it("不给 wantBinary 时维持文本行为（不产生 blobUrl）", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      res("", { type: "image/png", bytes: new Uint8Array([1, 2]) }),
    );
    const got = await probeRequest(req, "sk");
    expect(got.blobUrl).toBeUndefined();
    expect(got.body).toContain("二进制内容");
  });
});

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError, apiClient, upload } from "@/lib/apiClient";

/**
 * multipart 上传通道的契约。
 *
 * 这里守的是三件**看起来对、跑起来错**的事：
 *  1. 不能带 `Content-Type: application/json` —— 手写了它，服务端会按 JSON 解析表单，
 *     `c.FormFile` 直接失败，而报错只有一句「缺少上传文件」；
 *  2. 不能 `JSON.stringify` 请求体 —— FormData 会被序列化成 `{}`，上传静默变成空文件；
 *  3. 错误体解析必须与 JSON 通道**完全一致** —— 各写一份的结果是
 *     「上传超限时没有提示」或「上传时 401 不登出」。
 */
const fetchMock = vi.fn();

beforeEach(() => {
  vi.stubGlobal("fetch", fetchMock);
  // CSRF 双提交令牌：服务端种的非 HttpOnly Cookie，前端原样放进头。
  document.cookie = "ld_csrf=tok-123";
  fetchMock.mockReset();
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.cookie = "ld_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT";
});

function okResponse(data: unknown): Response {
  return new Response(JSON.stringify({ code: 0, message: "ok", data }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function errResponse(status: number, code: string, message: string): Response {
  return new Response(JSON.stringify({ code: status, message, error_code: code }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("upload", () => {
  it("发 FormData：不设 Content-Type、不序列化请求体、带 CSRF 头与凭证", async () => {
    fetchMock.mockResolvedValue(
      okResponse({ id: "01j9x8k2m4n6p8r0t2v4w6y8z0", icon: "upload:01j9x8k2m4n6p8r0t2v4w6y8z0" }),
    );
    const form = new FormData();
    form.append("file", new Blob([new Uint8Array([0x89, 0x50, 0x4e, 0x47])]), "a.png");

    const got = await upload<{ id: string; icon: string }>("/admin/icons", form);
    expect(got.icon).toBe("upload:01j9x8k2m4n6p8r0t2v4w6y8z0");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    // resolveURL 会补成绝对地址（基于 window.location.origin），只断言路径部分。
    expect(url).toContain("/api/admin/icons");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("include");
    // 关键：body 就是 FormData 本身（不是字符串）
    expect(init.body).toBe(form);
    const headers = init.headers as Record<string, string>;
    expect(headers["Content-Type"]).toBeUndefined();
    // 边界串由浏览器生成，前端无法拼；一旦手写 Content-Type 就会丢掉 boundary
    expect(headers["X-CSRF-Token"]).toBe("tok-123");
  });

  it("业务失败按统一错误体抛 ApiError（含 error_code）", async () => {
    fetchMock.mockResolvedValue(errResponse(415, "file_type_unsupported", "只支持 PNG / JPEG / WebP"));
    const form = new FormData();
    await expect(upload("/admin/icons", form)).rejects.toMatchObject({
      code: 415,
      errorCode: "file_type_unsupported",
      message: "只支持 PNG / JPEG / WebP",
    });
  });

  it("413 与 503 同样是普通业务错误，不触发登出", async () => {
    fetchMock.mockResolvedValue(errResponse(413, "file_too_large", "图标不能超过 512KB"));
    const err = await upload("/admin/icons", new FormData()).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe(413);

    // 存储故障是 503（502 的语义是"上游网关回了坏响应"，与服务端自身存储不可用不同）。
    fetchMock.mockResolvedValue(errResponse(503, "storage_unavailable", "存储服务不可用"));
    const err2 = await upload("/admin/icons", new FormData()).catch((e: unknown) => e);
    expect((err2 as ApiError).errorCode).toBe("storage_unavailable");
  });

  it("网络异常归一成 -1（不把 fetch 的 TypeError 泄漏给页面）", async () => {
    fetchMock.mockRejectedValue(new TypeError("Failed to fetch"));
    const err = await upload("/admin/icons", new FormData()).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe(-1);
  });
});

describe("apiClient 仍走 JSON 通道", () => {
  it("普通 POST 依旧设 Content-Type 并序列化", async () => {
    fetchMock.mockResolvedValue(okResponse({ item: { id: "v1" } }));
    await apiClient.post("/admin/vendors", { name: "测试分组" });

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["Content-Type"]).toBe("application/json");
    expect(init.body).toBe(JSON.stringify({ name: "测试分组" }));
  });
});

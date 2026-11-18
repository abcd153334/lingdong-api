import { describe, expect, it } from "vitest";
import { EMPTY_ADAPTER_FORM, buildConfig, configToForm, normalizePrefix } from "@/lib/adapterConfig";

describe("adapterConfig 表单适配", () => {
  it("全空时返回 null（新建不携带该字段，保持旧请求体不变）", () => {
    const r = buildConfig({ ...EMPTY_ADAPTER_FORM });
    expect(r.config).toBeNull();
    expect(r.errors).toEqual({});
  });

  it("非法 JSON 就地报错且不产出配置", () => {
    const r = buildConfig({ ...EMPTY_ADAPTER_FORM, requestPatch: "{" });
    expect(r.config).toBeNull();
    expect(r.errors.requestPatch).toContain("不是合法 JSON");
  });

  it("非对象（数组/标量）被拒绝", () => {
    const r = buildConfig({ ...EMPTY_ADAPTER_FORM, pathOverrides: "[1,2]" });
    expect(r.errors.pathOverrides).toContain("必须是 JSON 对象");
  });

  it("路径覆盖的值必须以 / 开头", () => {
    const r = buildConfig({
      ...EMPTY_ADAPTER_FORM,
      pathOverrides: '{"\\/v1\\/chat\\/completions": "v4/chat"}',
    });
    expect(r.errors.pathOverrides).toContain("必须以 / 开头");
  });

  it("response_path 必须含 content", () => {
    const r = buildConfig({
      ...EMPTY_ADAPTER_FORM,
      responsePath: '{"prompt_tokens": "a.b"}',
    });
    expect(r.errors.responsePath).toContain("content");
  });

  it("鉴权前缀非空且无尾随空格时自动补一个（后端与插件都不补，防错只能放前端）", () => {
    expect(normalizePrefix("Bearer")).toBe("Bearer ");
    expect(normalizePrefix("Bearer ")).toBe("Bearer ");
    expect(normalizePrefix("none")).toBe("none");
    expect(normalizePrefix("NONE")).toBe("none");
    expect(normalizePrefix("  ")).toBe("");
  });

  it("配置往返不丢字段", () => {
    const cfg = {
      auth_header: "x-api-key",
      auth_prefix: "Bearer ",
      path_overrides: { "/v1/chat/completions": "/v4/chat" },
      request_patch: { thinking: { type: "enabled" }, temperature: null },
      response_path: { content: "data.answer", prompt_tokens: "data.usage.in" },
    };
    const form = configToForm(cfg);
    const back = buildConfig(form);
    expect(back.errors).toEqual({});
    expect(back.config).toEqual(cfg);
  });

  it("后端下发 {} 时表单全空", () => {
    const form = configToForm({});
    expect(form).toEqual(EMPTY_ADAPTER_FORM);
  });

  it("后端未下发该字段时也能容错（旧接口/桩件）", () => {
    expect(() => configToForm(undefined)).not.toThrow();
    expect(configToForm(undefined)).toEqual(EMPTY_ADAPTER_FORM);
  });
});

import { describe, expect, it } from "vitest";

import { builtinIconToken, isBuiltinIconToken, isUploadIconToken } from "@/lib/entityIcon";
import { initialOf } from "@/lib/utils";

/**
 * 令牌构造与判定（**不含**「令牌 → 地址」的解析）。
 *
 * 地址映射的唯一实现在后端 `service/iconref`，前端只消费 `icon_url`。
 * 这里守着的是写路径的那一半：表单提交前拼出来的 token 必须能被后端接受。
 */
describe("token 构造", () => {
  it("builtinIconToken 只接受合法 key", () => {
    expect(builtinIconToken("openai-gpt")).toBe("builtin:openai-gpt");
    expect(builtinIconToken("")).toBe("");
    expect(builtinIconToken("../x")).toBe("");
    // 大写不在 catalog 的形态里：拼出来必然被后端 icon_invalid 拒掉
    expect(builtinIconToken("OpenAI")).toBe("");
  });
});

describe("token 判定", () => {
  it("区分内置与上传", () => {
    expect(isBuiltinIconToken("builtin:openai-gpt")).toBe(true);
    expect(isBuiltinIconToken("upload:01j9x8k2m4n6p8r0t2v4w6y8z0")).toBe(false);
    expect(isUploadIconToken("upload:01j9x8k2m4n6p8r0t2v4w6y8z0")).toBe(true);
    expect(isUploadIconToken("builtin:openai-gpt")).toBe(false);
    expect(isBuiltinIconToken("")).toBe(false);
  });
});

/**
 * 首字占位：三处（实体图标 / 站点 logo / 用户头像）共用。
 * 用错实现的表现是 emoji 被截成半个码元（渲染成「�」）或大小写规则不一致。
 */
describe("initialOf", () => {
  it("英文名取首字母并大写", () => {
    expect(initialOf("openai")).toBe("O");
    expect(initialOf("Kling")).toBe("K");
  });

  it("中文名取第一个字", () => {
    expect(initialOf("智谱")).toBe("智");
    expect(initialOf("阿里通义")).toBe("阿");
  });

  it("代理对（emoji）不会被劈开", () => {
    expect(initialOf("🚀rocket")).toBe("🚀");
  });

  it("空值回落为 ?，也可自定义", () => {
    expect(initialOf("")).toBe("?");
    expect(initialOf("   ")).toBe("?");
    expect(initialOf(undefined)).toBe("?");
    expect(initialOf(null, "·")).toBe("·");
  });

  it("只对 ASCII 小写字母做大写（避免 ß→SS 撑宽）", () => {
    expect(initialOf("ß")).toBe("ß");
    expect(initialOf("1password")).toBe("1");
  });
});

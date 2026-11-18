import { describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";

import type { AuthConfig, ContactChannel } from "@/types";

/**
 * 联系方式渠道解析的契约。
 *
 * 锁四件事（都是真实存在过的问题）：
 *   1. 默认渠道取「下发数组的首项」，前端**不重排**（优先级只在后端一处定义）；
 *   2. 用户手动选择只在仍合法时保留，集合变化要收敛（否则发出策略不允许的渠道）；
 *   3. 配置尚未返回时不得假设任何渠道（否则会先画一个邮箱框）；
 *   4. blocked 只在「要求验证 + 无可用渠道」时为真。
 */

type MockConfig = { auth: AuthConfig; email_enabled: boolean; sms_enabled: boolean };

const portal: { data?: MockConfig; fetched: boolean } = { fetched: false };

vi.mock("@/lib/api", () => ({
  usePortalConfigQuery: () => ({ data: portal.data, isFetched: portal.fetched }),
}));

import {
  bindTargetError,
  channelFormatError,
  channelMeta,
  CHANNEL_META,
  contactOf,
  CONTACT_CHANNELS,
  resolveChannel,
  useAvailableChannels,
  useChannelAvailability,
  useContactChannelPicker,
} from "@/lib/contactChannel";

function authCfg(overrides: Partial<AuthConfig> = {}): AuthConfig {
  return {
    register_mode: "open",
    contact_verify: "off",
    contact_channels: ["email"],
    available_contact_channels: ["email"],
    captcha_register: "always",
    captcha_login: "after_fails",
    captcha_reset: "always",
    login_fail_threshold: 5,
    ...overrides,
  };
}

function setConfig(
  channels: ContactChannel[],
  opts: { required?: boolean; email?: boolean; sms?: boolean } = {},
) {
  portal.fetched = true;
  portal.data = {
    auth: authCfg({
      contact_verify: opts.required ? "required" : "off",
      available_contact_channels: channels,
    }),
    email_enabled: opts.email ?? true,
    sms_enabled: opts.sms ?? true,
  };
}

describe("resolveChannel", () => {
  it("空集合返回 null（页面据此不渲染任何渠道）", () => {
    expect(resolveChannel([], null)).toBeNull();
  });

  it("没有手动选择时取首项（首项 = 后端给的默认，当前是手机）", () => {
    expect(resolveChannel(["sms", "email"], null)).toBe("sms");
  });

  it("手动选择仍合法时保留用户的选择", () => {
    expect(resolveChannel(["sms", "email"], "email")).toBe("email");
  });

  it("手动选择不再合法时收敛到首项（防发出策略不允许的渠道）", () => {
    expect(resolveChannel(["sms"], "email")).toBe("sms");
  });

  it("不重排：顺序完全由后端决定，前端不许把 sms 提到前面", () => {
    expect(resolveChannel(["email", "sms"], null)).toBe("email");
  });
});

describe("CHANNEL_META", () => {
  it("两个渠道的文案与输入属性齐全且互不串味", () => {
    const email = channelMeta("email");
    const sms = channelMeta("sms");
    expect([email.label, email.shortLabel, email.inputType, email.autoComplete]).toEqual([
      "邮箱",
      "邮箱",
      "email",
      "email",
    ]);
    expect([sms.label, sms.shortLabel, sms.inputType, sms.autoComplete]).toEqual([
      "手机号",
      "短信",
      "tel",
      "tel",
    ]);
    // 用户中心行标题与找回抽屉字段名是两个场景，不能混用
    expect(email.rowLabel).toBe("绑定邮箱");
    expect(sms.rowLabel).toBe("绑定手机");
    expect(sms.drawerLabel).toBe("绑定手机号");
  });

  it("正则各管各的：手机号不接受邮箱，邮箱不接受手机号", () => {
    expect(channelMeta("email").pattern.test("a@b.com")).toBe(true);
    expect(channelMeta("email").pattern.test("13800138000")).toBe(false);
    expect(channelMeta("sms").pattern.test("13800138000")).toBe(true);
    expect(channelMeta("sms").pattern.test("a@b.com")).toBe(false);
    expect(channelMeta("sms").pattern.test("12345")).toBe(false);
  });

  it("脱敏走各渠道自己的规则（页面不再自己分支）", () => {
    expect(channelMeta("email").mask("alice@example.com")).toBe("a***@example.com");
    expect(channelMeta("sms").mask("13800138000")).toBe("138****8000");
  });
});

describe("格式校验", () => {
  it("channelFormatError 返回该渠道自己的文案", () => {
    expect(channelFormatError("email", "a@b.com")).toBeNull();
    expect(channelFormatError("email", "13800138000")).toBe("邮箱格式不正确");
    expect(channelFormatError("sms", "a@b.com")).toBe("手机号格式不正确");
  });

  it("bindTargetError 额外拦「与当前一致」", () => {
    expect(bindTargetError("email", "", "bad")).toBe("邮箱格式不正确");
    expect(bindTargetError("email", "a@b.com", "a@b.com")).toBe("新邮箱与当前一致");
    expect(bindTargetError("sms", "", "13800001234")).toBe("");
    expect(bindTargetError("sms", "13800001234", "13800001234")).toBe("新手机号与当前一致");
  });

  it("contactOf 按渠道取对应的资料字段", () => {
    expect(contactOf("email", "a@b.com", "13800138000")).toBe("a@b.com");
    expect(contactOf("sms", "a@b.com", "13800138000")).toBe("13800138000");
  });
});

describe("useContactChannelPicker", () => {
  it("配置未返回：channels 与 meta 都是 null（不得假设任何渠道）", () => {
    portal.fetched = false;
    portal.data = undefined;
    const { result } = renderHook(() => useContactChannelPicker());
    expect(result.current.channels).toBeNull();
    expect(result.current.meta).toBeNull();
    expect(result.current.blocked).toBe(false);
  });

  it("配置到达：默认取首项（手机优先由后端数组顺序体现）", () => {
    setConfig(["sms", "email"], { required: true });
    const { result } = renderHook(() => useContactChannelPicker());
    expect(result.current.channels).toEqual(["sms", "email"]);
    expect(result.current.channel).toBe("sms");
    expect(result.current.meta?.inputType).toBe("tel");
    expect(result.current.blocked).toBe(false);
  });

  it("手动切到邮箱后保留；集合收缩时自动收敛回手机", () => {
    setConfig(["sms", "email"]);
    const { result, rerender } = renderHook(() => useContactChannelPicker());
    act(() => result.current.select("email"));
    expect(result.current.channel).toBe("email");

    // 运营在另一个标签页只留了短信 → 用户此前的选择不再合法
    setConfig(["sms"]);
    rerender();
    expect(result.current.channel).toBe("sms");
    expect(result.current.meta?.inputType).toBe("tel");
  });

  it("blocked 只在「要求验证 + 无可用渠道」时为真", () => {
    setConfig([], { required: true });
    const required = renderHook(() => useContactChannelPicker());
    expect(required.result.current.blocked).toBe(true);
    expect(required.result.current.meta).toBeNull();

    setConfig([], { required: false });
    const off = renderHook(() => useContactChannelPicker());
    expect(off.result.current.blocked).toBe(false);
    expect(off.result.current.channels).toEqual([]);
  });
});

describe("两个可用性 hook 的口径差异", () => {
  it("注册/找回用生效集合（策略∩就绪）；换绑只看就绪", () => {
    // 策略只勾了邮箱，但两个通道都配好了
    portal.fetched = true;
    portal.data = {
      auth: authCfg({
        contact_channels: ["email"],
        available_contact_channels: ["email"],
      }),
      email_enabled: true,
      sms_enabled: true,
    };

    const available = renderHook(() => useAvailableChannels());
    expect(available.result.current).toEqual(["email"]);

    // 换绑不受策略影响：手机通道就绪就可用（与服务端 BindContact 一致）
    const bind = renderHook(() => useChannelAvailability());
    expect(bind.result.current.channels).toEqual(["email", "sms"]);
    expect(bind.result.current.isAvailable("sms")).toBe(true);
  });

  it("通道未就绪时换绑入口不可用", () => {
    portal.fetched = true;
    portal.data = {
      auth: authCfg(),
      email_enabled: false,
      sms_enabled: true,
    };
    const { result } = renderHook(() => useChannelAvailability());
    expect(result.current.isAvailable("email")).toBe(false);
    expect(result.current.isAvailable("sms")).toBe(true);
  });
});

describe("渠道集合的单一来源", () => {
  it("CONTACT_CHANNELS 与 CHANNEL_META 严格同源", () => {
    // 账户弹窗「账户安全」两行按 CONTACT_CHANNELS 的顺序渲染，文案/校验按 CHANNEL_META 取。
    // 新增渠道只改一边会得到「渲染出第三个渠道但没有文案」或「有文案却永远不显示」。
    expect([...CONTACT_CHANNELS].sort()).toEqual(Object.keys(CHANNEL_META).sort());
  });
});

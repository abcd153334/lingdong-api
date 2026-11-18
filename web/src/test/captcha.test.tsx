import { describe, expect, it, vi } from "vitest";
import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AuthConfig } from "@/types";

/**
 * 图形验证码的前端契约。
 *
 * 这里锁四件事（都是上一版真出过问题的）：
 *   1. 只有策略要求时才取图（off 不白请求）；
 *   2. after_fails 首屏不亮，服务端说「要验证」时能亮出来并取图；
 *   3. refresh 一定换新 id —— 答案是一次性的，复用 id 必失败；
 *   4. 图挂了要能看见「重试」，不是空白。
 */

const portal: { data?: { auth: AuthConfig }; fetched: boolean } = { fetched: false };
let calls = 0;
let failNext = false;

function auth(overrides: Partial<Record<string, unknown>> = {}): AuthConfig {
  return {
    register_mode: "open",
    contact_verify: "off",
    contact_channels: [],
    available_contact_channels: [],
    captcha_register: "always",
    captcha_login: "after_fails",
    captcha_reset: "always",
    login_fail_threshold: 5,
    ...overrides,
  } as AuthConfig;
}

vi.mock("@/lib/api", () => ({
  usePortalConfigQuery: () => ({ data: portal.data, isFetched: portal.fetched }),
  fetchCaptcha: async () => {
    calls += 1;
    if (failNext) throw new Error("boom");
    return { id: `id-${calls}`, image: `data:image/png;base64,AAA${calls}` };
  },
}));

import { CaptchaField, useCaptcha } from "@/components/auth/AuthFields";

function ready(authCfg: AuthConfig) {
  portal.data = { auth: authCfg };
  portal.fetched = true;
}

describe("useCaptcha", () => {
  it("配置未就绪时不取图（避免白请求）", () => {
    portal.data = undefined;
    portal.fetched = false;
    calls = 0;
    const { result } = renderHook(() => useCaptcha("register"));
    expect(result.current.visible).toBe(false);
    expect(result.current.id).toBe("");
    expect(calls).toBe(0);
  });

  it("always：挂载即取图，id 与 image 一起返回", async () => {
    ready(auth());
    calls = 0;
    const { result } = renderHook(() => useCaptcha("register"));
    await waitFor(() => expect(result.current.id).toBe("id-1"));
    expect(result.current.visible).toBe(true);
    expect(result.current.image).toContain("data:image/png;base64,");
  });

  it("off：不渲染也不取图", async () => {
    ready(auth({ captcha_login: "off" }));
    calls = 0;
    const { result } = renderHook(() => useCaptcha("login"));
    await waitFor(() => expect(result.current.mode).toBe("off"));
    expect(result.current.visible).toBe(false);
    expect(calls).toBe(0);
  });

  it("after_fails：首屏不渲染，require() 后才取图", async () => {
    ready(auth());
    calls = 0;
    const { result } = renderHook(() => useCaptcha("login"));
    await waitFor(() => expect(result.current.mode).toBe("after_fails"));
    expect(result.current.visible).toBe(false);
    expect(calls).toBe(0);

    act(() => result.current.require());
    await waitFor(() => expect(result.current.id).toBe("id-1"));
    expect(result.current.visible).toBe(true);
  });

  it("refresh 一定换新 id，并清空输入", async () => {
    ready(auth());
    calls = 0;
    const { result } = renderHook(() => useCaptcha("register"));
    await waitFor(() => expect(result.current.id).toBe("id-1"));

    act(() => result.current.setAnswer("12345"));
    expect(result.current.answer).toBe("12345");

    act(() => result.current.refresh());
    await waitFor(() => expect(result.current.id).toBe("id-2"));
    expect(result.current.answer).toBe("");
  });

  it("取图失败时把 id 清掉（不能拿旧 id 配新答案）", async () => {
    ready(auth());
    calls = 0;
    failNext = true;
    const { result } = renderHook(() => useCaptcha("register"));
    await waitFor(() => expect(result.current.failed).toBe(true));
    expect(result.current.id).toBe("");
    failNext = false;
  });
});

describe("CaptchaField", () => {
  it("图内嵌在输入框右端，点图即换一张（没有独立的换一张按钮）", async () => {
    ready(auth());
    calls = 0;
    const Probe = () => {
      const state = useCaptcha("register");
      return <CaptchaField id="cap" state={state} />;
    };
    const { container } = render(<Probe />);
    await waitFor(() => expect(container.querySelector("img")).not.toBeNull());

    const img = container.querySelector("img")!;
    expect(img.getAttribute("src")).toContain("data:image/png;base64,");

    // 结构契约：图必须与输入框同属 `.captcha-box`，且图在按钮内（热区就是图本身）。
    const box = container.querySelector(".captcha-box")!;
    expect(box).not.toBeNull();
    expect(box.querySelector("input")).not.toBeNull();
    expect(box.querySelector(".captcha-swap")!.contains(img)).toBe(true);

    // 只有一块可点热区：全控件里除「图/重试」外不该再有按钮，
    // 这一条防的是"又加回一个换一张按钮"。
    expect(container.querySelectorAll(".captcha-box button").length).toBe(1);

    await userEvent.click(img);
    await waitFor(() => expect(calls).toBe(2));
  });

  it("只接受数字，最多 5 位", async () => {
    ready(auth());
    const Probe = () => {
      const state = useCaptcha("register");
      return <CaptchaField id="cap" state={state} />;
    };
    render(<Probe />);
    const input = await screen.findByPlaceholderText("图中 5 位数字");
    await userEvent.type(input, "a1b2c3d4e5f6");
    expect((input as HTMLInputElement).value).toBe("12345");
  });

  it("图加载失败时同一热区显示「重试」", async () => {
    ready(auth());
    failNext = true;
    const Probe = () => {
      const state = useCaptcha("register");
      return <CaptchaField id="cap" state={state} />;
    };
    render(<Probe />);
    const retry = await screen.findByText("重试");
    failNext = false;
    calls = 0;
    await userEvent.click(retry);
    // 点重试会重新取图（并成功），失败态消失
    await waitFor(() => expect(calls).toBe(1));
  });

  it("off 时不渲染任何东西", async () => {
    ready(auth({ captcha_login: "off" }));
    const Probe = () => {
      const state = useCaptcha("login");
      return <CaptchaField id="cap" state={state} />;
    };
    const { container } = render(<Probe />);
    await waitFor(() => expect(container.innerHTML).toBe(""));
  });
});

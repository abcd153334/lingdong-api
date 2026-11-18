import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { sendCodeText } from "@/lib/sendCode";
import { useCountdown } from "@/lib/useCountdown";

/**
 * 发码按钮的两个共享件：文案（`sendCodeText`）与倒计时（`useCountdown`）。
 *
 * 这两样原本在注册 / 找回 / 换绑三处各写一份，行为已经漂移
 * （文案一个写"重新发送"、一个写"重新发送(60s)"，换绑干脆没有发送中态）。
 * 收敛后这里锁三件事：
 *   1. 三态互斥且 pending 优先；
 *   2. 文案宽度不超过按钮的 min-width（否则输入区会跟着跳）；
 *   3. 倒计时到 0 自己停表，不会 0 之后继续跑成负数。
 */

describe("sendCodeText", () => {
  it("空闲 / 倒计时 / 发送中三态", () => {
    expect(sendCodeText(0, false)).toBe("发送验证码");
    expect(sendCodeText(59, false)).toBe("重新发送(59s)");
    expect(sendCodeText(0, true)).toBe("发送中…");
  });

  it("pending 优先于倒计时（发送途中先告诉用户在发）", () => {
    expect(sendCodeText(60, true)).toBe("发送中…");
  });

  it("idle 文案可覆盖（找回是「重新发送」、管理台是「测试发送」）", () => {
    expect(sendCodeText(0, false, "重新发送")).toBe("重新发送");
    expect(sendCodeText(0, false, "测试发送")).toBe("测试发送");
  });

  it("三种文案都装得进 120px 的按钮（宽高固定，长了会挤掉输入区）", () => {
    // 估算：中文 13px、ASCII 7px（按钮 font-size: 13px + padding 0 14px）
    const width = (s: string) =>
      [...s].reduce((w, ch) => w + (/[\u4e00-\u9fa5]/.test(ch) ? 13 : 7), 0);
    const cases = [
      sendCodeText(0, false),
      sendCodeText(60, false),
      sendCodeText(0, true),
      sendCodeText(0, false, "重新发送"),
      sendCodeText(0, false, "测试发送"),
    ];
    for (const c of cases) {
      expect(width(c), `「${c}」按 13px/7px 估算 ${width(c)}px，超过 120 − 28 padding`)
        .toBeLessThanOrEqual(92);
    }
  });
});

describe("useCountdown", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("每秒递减，到 0 停表（不会变负数）", () => {
    const { result } = renderHook(() => useCountdown());
    expect(result.current.left).toBe(0);

    act(() => result.current.setLeft(3));
    expect(result.current.left).toBe(3);

    act(() => void vi.advanceTimersByTime(1000));
    expect(result.current.left).toBe(2);

    act(() => void vi.advanceTimersByTime(3000));
    expect(result.current.left).toBe(0);

    // 停表：再走 5 秒也不会动
    act(() => void vi.advanceTimersByTime(5000));
    expect(result.current.left).toBe(0);
  });

  it("倒计时中途可以重置（再次发码）", () => {
    const { result } = renderHook(() => useCountdown());
    act(() => result.current.setLeft(10));
    act(() => void vi.advanceTimersByTime(2000));
    expect(result.current.left).toBe(8);

    act(() => result.current.setLeft(60));
    act(() => void vi.advanceTimersByTime(1000));
    expect(result.current.left).toBe(59);
  });
});

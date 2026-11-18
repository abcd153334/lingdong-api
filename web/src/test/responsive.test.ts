import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MOBILE_QUERY, useIsMobile, useMediaQuery } from "@/lib/responsive";

/**
 * `useMediaQuery` 的取值必须是**首帧就是对的**。
 *
 * 曾经的写法是 `useState(false) + useEffect(mql.addEventListener)`：移动端首帧
 * 恒为 false（按桌面渲染），下一帧才切回来——侧栏会闪一下常驻态再变抽屉。
 * 那条回归靠「初始快照直接读 matchMedia().matches」守住，所以这里第一例
 * 就让 matches=true 并断言首次渲染即为 true。
 */
type Listener = () => void;

function stubMatchMedia(initial: boolean) {
  const listeners = new Set<Listener>();
  const mql = {
    get matches() {
      return state.value;
    },
    media: MOBILE_QUERY,
    onchange: null,
    addEventListener: (_: string, fn: Listener) => void listeners.add(fn),
    removeEventListener: (_: string, fn: Listener) => void listeners.delete(fn),
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  };
  const state = { value: initial };
  vi.stubGlobal("matchMedia", () => mql as unknown as MediaQueryList);
  return {
    set(next: boolean) {
      state.value = next;
      listeners.forEach((fn) => fn());
    },
    listenerCount: () => listeners.size,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("responsive", () => {
  it("断点与 components.css 的 @media (max-width: 767px) 一致", () => {
    // 两边错开过一次：遮罩用 Tailwind 的 lg(1024)、抽屉化用 767，
    // 768~1023 之间遮罩盖住整页。改这个常量前先改 CSS，反之亦然。
    expect(MOBILE_QUERY).toBe("(max-width: 767px)");
  });

  it("首帧即取真实值，不从 false 起步", () => {
    stubMatchMedia(true);
    const { result } = renderHook(() => useIsMobile());
    expect(result.current).toBe(true);
  });

  it("媒体查询变化时跟随更新，卸载后不再订阅", () => {
    const mq = stubMatchMedia(false);
    const { result, unmount } = renderHook(() => useMediaQuery(MOBILE_QUERY));
    expect(result.current).toBe(false);

    act(() => mq.set(true));
    expect(result.current).toBe(true);

    unmount();
    expect(mq.listenerCount()).toBe(0);
  });
});

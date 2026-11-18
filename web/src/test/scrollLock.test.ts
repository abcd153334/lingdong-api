import { afterEach, describe, expect, it, vi } from "vitest";

import { lockScroll } from "@/lib/scrollLock";

/**
 * 锁滚动的三条不变式，都是「改坏了不会报错、只会让页面跳一下」的类型：
 *  1. 引用计数：叠层只加锁一次，最后一层释放才还原（与关闭顺序无关）；
 *  2. 补偿量 = **body 的实测布局增量**（锁前锁后宽度相同就不补，绝不能凭空加）；
 *  3. 补偿只在原有内边距上累加，不覆盖样式表/内联已有的 padding。
 */

/**
 * 造一个「锁前 / 锁后 body 布局宽度」可区分的视口。
 *
 * 真实浏览器里这两个数由布局决定：页面有滚动条时，锁定（滚动条消失）后多出一个
 * 滚动条宽；页面本来就没有滚动条（内容不足一屏）时两者相等。实现只认这两个数的
 * 差，所以这里直接给定它们，而不是去 mock `clientWidth` —— 那个值不是布局。
 */
function mockBodyWidth(before: number, after: number): void {
  vi.spyOn(document.body, "getBoundingClientRect").mockImplementation(() => {
    const width = document.body.style.overflow === "hidden" ? after : before;
    return {
      width,
      height: 0,
      x: 0,
      y: 0,
      top: 0,
      left: 0,
      right: width,
      bottom: 0,
      toJSON: () => ({}),
    } as DOMRect;
  });
}

afterEach(() => {
  vi.restoreAllMocks();
  document.body.style.overflow = "";
  document.body.style.paddingRight = "";
});

describe("lockScroll", () => {
  it("页面有滚动条：补回滚动条吃掉的宽度", () => {
    mockBodyWidth(1185, 1200); // 锁后多出 15
    document.body.style.paddingRight = "4px";

    const release = lockScroll();
    expect(document.body.style.overflow).toBe("hidden");
    // 4（原有内联）+ 15（实际变宽）—— 覆盖会丢掉原有内边距
    expect(document.body.style.paddingRight).toBe("19px");

    release();
    expect(document.body.style.overflow).toBe("");
    expect(document.body.style.paddingRight).toBe("4px");
  });

  it("页面本来没有滚动条：不补内边距（锁定前后布局宽度不变）", () => {
    // 这就是曾经的 bug：锁前按 `innerWidth - clientWidth` 算出 10，而布局其实
    // 没变（页面本来就没有滚动条），补一次就是内容凭空内缩 10px。
    mockBodyWidth(1185, 1185);

    const release = lockScroll();
    expect(document.body.style.overflow).toBe("hidden");
    expect(document.body.style.paddingRight).toBe("");

    release();
    expect(document.body.style.overflow).toBe("");
  });

  it("叠层只加锁一次，最后一层释放才还原（与关闭顺序无关）", () => {
    mockBodyWidth(1185, 1200);

    const first = lockScroll();
    const second = lockScroll();
    expect(document.body.style.paddingRight).toBe("15px");

    // 先关「上层」：底下那层还开着，页面必须保持锁定
    second();
    expect(document.body.style.overflow).toBe("hidden");
    expect(document.body.style.paddingRight).toBe("15px");

    first();
    expect(document.body.style.overflow).toBe("");
    expect(document.body.style.paddingRight).toBe("");
  });

  it("重复调用同一个释放函数只生效一次（React effect 双跑不会把计数减穿）", () => {
    mockBodyWidth(1200, 1200);

    const a = lockScroll();
    const b = lockScroll();
    a();
    a(); // 幂等
    expect(document.body.style.overflow).toBe("hidden");
    b();
    expect(document.body.style.overflow).toBe("");
  });
});

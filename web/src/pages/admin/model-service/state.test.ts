import { describe, expect, it } from "vitest";

import type { ChannelItem } from "@/types";

import { adapterInView, flyoutAdapterId, nextState, ORPHANS } from "./state";

const ch = (id: number, adapterId: string): ChannelItem =>
  ({ id, adapter_id: adapterId }) as unknown as ChannelItem;

/**
 * 状态机是这次信息架构调整里唯一"会安静地错"的地方：
 * 打开浮层顺手清掉 `c`（右栏突然空白）、选中渠道忘了关浮层（盖住刚切过去的表格）、
 * 回全部模型没清 `c`（URL 说在看渠道、界面是全部模型）—— 三种都不会报错。
 */
describe("模型服务 · 左栏状态机", () => {
  it("打开浮层不动右栏：点开 B 时 c 仍然是 A 的渠道", () => {
    const s = nextState({ open: "", c: 11 }, { kind: "openAdapter", id: "b" });
    expect(s).toEqual({ open: "b", c: 11 });
  });

  it("再点同一张卡即关闭", () => {
    expect(nextState({ open: "a", c: 11 }, { kind: "toggleAdapter", id: "a" })).toEqual({
      open: "",
      c: 11,
    });
  });

  it("点另一张卡是切换而不是关闭", () => {
    expect(nextState({ open: "a", c: 11 }, { kind: "toggleAdapter", id: "b" })).toEqual({
      open: "b",
      c: 11,
    });
  });

  it("选中渠道即关浮层", () => {
    expect(nextState({ open: "a", c: 9 }, { kind: "selectChannel", channel: ch(11, "a") })).toEqual({
      open: "",
      c: 11,
    });
  });

  it("回全部模型：清浮层也清 c", () => {
    expect(nextState({ open: "a", c: 11 }, { kind: "showAll" })).toEqual({ open: "", c: undefined });
  });

  it("删渠道后回到全部模型（URL 指向的行已经不存在）", () => {
    expect(nextState({ open: "", c: 11 }, { kind: "channelDeleted" })).toEqual({
      open: "",
      c: undefined,
    });
  });

  it("孤儿桶用哨兵表示，再点一次关闭", () => {
    expect(nextState({ open: "", c: 11 }, { kind: "openOrphans" })).toEqual({
      open: ORPHANS,
      c: 11,
    });
    expect(nextState({ open: ORPHANS, c: 11 }, { kind: "openOrphans" })).toEqual({
      open: "",
      c: 11,
    });
  });

  it("关浮层不改变右栏视图", () => {
    expect(nextState({ open: "a", c: 11 }, { kind: "closeFlyout" })).toEqual({ open: "", c: 11 });
  });

  it("哨兵不是合法适配器 id（不会被误当成某个适配器）", () => {
    expect(ORPHANS.startsWith("#")).toBe(true);
    expect(flyoutAdapterId(ORPHANS)).toBeNull();
    expect(flyoutAdapterId("a")).toBe("a");
  });

  it("aria-current 只看渠道归属，与浮层无关", () => {
    expect(adapterInView(ch(11, "a"))).toBe("a");
    expect(adapterInView(null)).toBe("");
  });
});

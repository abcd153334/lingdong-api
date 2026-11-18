import { describe, expect, it } from "vitest";

import type { ChannelItem } from "@/types";

import { focusOf, nextSelection } from "./selection";

/** 只造判定用得到的字段：其余列不参与「展开/选中」规则。 */
const ch = (id: number, adapterId: string): ChannelItem =>
  ({ id, name: `渠道 ${id}`, adapter_id: adapterId, status: 1 }) as unknown as ChannelItem;

describe("左栏选中规则", () => {
  it("点组头展开：同组内保留选中，跨组清掉选中", () => {
    const x = ch(11, "spec:my-gw");
    const openA = nextSelection({ open: "", channel: x }, {
      kind: "toggle",
      id: "spec:my-gw",
      channelIds: [11],
    });
    expect(openA).toEqual({ a: "spec:my-gw", c: 11, open: "spec:my-gw" });

    const openB = nextSelection({ open: "spec:my-gw", channel: x }, {
      kind: "toggle",
      id: "openai",
      channelIds: [1],
    });
    // 跨组必须清 c：否则 a=B、选中的行却藏在 A 里，左栏看不见它
    expect(openB).toEqual({ a: "openai", c: undefined, open: "openai" });
  });

  it("收起含选中渠道的组：a 清空、c 保留、右栏不动", () => {
    const x = ch(11, "spec:my-gw");
    const res = nextSelection({ open: "spec:my-gw", channel: x }, {
      kind: "toggle",
      id: "spec:my-gw",
      channelIds: [11],
    });
    expect(res).toEqual({ a: undefined, c: 11, open: "" });
  });

  it("收起不含选中渠道的组：不动 c（c 属于别的组时也不该被牵连）", () => {
    const res = nextSelection({ open: "openai", channel: null }, {
      kind: "toggle",
      id: "openai",
      channelIds: [],
    });
    expect(res).toEqual({ a: undefined, c: undefined, open: "" });
  });

  it("点孤儿渠道：清 a、写 c、不动展开态（不塌其它组）", () => {
    const gone = ch(13, "spec:gone");
    const res = nextSelection({ open: "spec:my-gw", channel: null }, {
      kind: "select",
      channel: gone,
      knownAdapters: ["spec:my-gw", "openai"],
    });
    expect(res).toEqual({ a: undefined, c: 13, open: "spec:my-gw" });
  });

  it("点正常渠道：写 a/c 并展开所属组", () => {
    const x = ch(11, "spec:my-gw");
    const res = nextSelection({ open: "openai", channel: null }, {
      kind: "select",
      channel: x,
      knownAdapters: ["spec:my-gw", "openai"],
    });
    expect(res).toEqual({ a: "spec:my-gw", c: 11, open: "spec:my-gw" });
  });

  it("删除当前渠道：只清 c，保留 a（右栏回到该适配器的总览）", () => {
    const res = nextSelection({ open: "openai", channel: ch(3, "openai") }, {
      kind: "delete",
      adapterId: "openai",
    });
    expect(res).toEqual({ a: "openai", c: undefined, open: "openai" });
  });
});

describe("聚焦适配器", () => {
  const groups = ["spec:my-gw", "openai"];

  it("渠道优先于 ?a（?a=B 但 c 属于 A 时以 A 为准）", () => {
    expect(focusOf("openai", ch(11, "spec:my-gw"), groups)).toBe("spec:my-gw");
  });

  it("无选中渠道时退回 ?a；?a 不在清单里则空", () => {
    expect(focusOf("openai", null, groups)).toBe("openai");
    expect(focusOf("spec:gone", null, groups)).toBe("");
    expect(focusOf(undefined, null, groups)).toBe("");
  });
});

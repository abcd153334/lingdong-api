import { describe, expect, it } from "vitest";

import type { DocsAdminEndpointRef, DocsAdminSection, DocsAdminTree } from "@/types/docsadmin";

import { buildRail, draftCount, hasUnbound, isCurrent, isOpen, openPath, RAIL_CLOSED, reconcile, seedOpen, toggle } from "./railModel";

/**
 * 左栏的纯函数：选中纠偏、选中判据、计数。
 *
 * 这三件事一旦错，症状都是「点了没反应」或「刷新后跳走」，而组件测试很难定位 ——
 * 所以把它们从组件里抽出来单独守。
 */

const ep = (key: string, extra: Partial<DocsAdminEndpointRef> = {}): DocsAdminEndpointRef => ({
  key,
  name: key,
  method: "POST",
  path: `/v1/${key}`,
  badge: "",
  status: "published",
  hidden: false,
  sort: 0,
  version: 1,
  bound: true,
  ...extra,
});

const section = (key: string, nodes: DocsAdminSection["nodes"] = []): DocsAdminSection => ({
  key,
  name: key,
  en: "",
  desc: "",
  color: "",
  sort: 0,
  hidden: false,
  version: 1,
  unknown: false,
  nodes,
});

const tree = (sections: DocsAdminSection[]): DocsAdminTree => ({
  shape: 7,
  contract_version: "6",
  base_url: "",
  sections,
  totals: { nodes: 0, endpoints: 0, draft_nodes: 0, draft_endpoints: 0 },
});

const T = tree([
  section("images", [
    {
      key: "images-gpt",
      kind: "family",
      title: "GPT Image",
      status: "published",
      hidden: false,
      sort: 0,
      version: 1,
      models: 2,
      endpoints: [ep("ep-gen"), ep("ep-edit", { status: "draft" })],
    },
  ]),
  section("videos"),
]);

describe("buildRail", () => {
  it("板块与节点按树里的顺序铺开（顺序来自库里的 sort）", () => {
    const rows = buildRail(T);
    expect(rows.map((r) => r.section.key)).toEqual(["images", "videos"]);
    expect(rows[0].nodes[0].node.key).toBe("images-gpt");
    expect(rows[1].nodes).toEqual([]);
  });

  it("没有树时返回空数组（不抛错、不返回 undefined）", () => {
    expect(buildRail(undefined)).toEqual([]);
  });
});

describe("reconcile", () => {
  it("节点仍在树里：一个字段都不动", () => {
    const r = reconcile(T, { node: "images-gpt", ep: "ep-gen" });
    expect(r.changed).toBe(false);
  });

  it("节点被删：只保留板块", () => {
    const r = reconcile(T, { section: "images", node: "gone", ep: "ep-gen" });
    expect(r.changed).toBe(true);
    expect(r.sel).toEqual({ section: "images" });
  });

  it("接口不在该节点下：只清接口，节点留着", () => {
    const r = reconcile(T, { node: "images-gpt", ep: "gone" });
    expect(r.changed).toBe(true);
    expect(r.sel).toEqual({ node: "images-gpt" });
  });

  it("没选节点时不纠偏（板块选中是合法态）", () => {
    const r = reconcile(T, { section: "videos" });
    expect(r.changed).toBe(false);
  });

  it("只有 ep 没有 node：丢掉 ep（接口必属于节点，留着只会得到空白与坏链接）", () => {
    const r = reconcile(T, { ep: "ep-gen" });
    expect(r.changed).toBe(true);
    expect(r.sel).toEqual({});
  });

  it("只有 ep 且带了板块：收敛到板块", () => {
    const r = reconcile(T, { section: "images", ep: "ep-gen" });
    expect(r.changed).toBe(true);
    expect(r.sel).toEqual({ section: "images" });
  });
});

describe("展开态（两级各自独立）", () => {
  it("首屏没选中：只摊开第一个有节点的板块，节点全收起", () => {
    expect(seedOpen(T, {})).toEqual({ sections: ["images"], nodes: [] });
  });

  it("选中板块：摊开它，节点仍然全收起", () => {
    expect(seedOpen(T, { section: "videos" })).toEqual({ sections: ["videos"], nodes: [] });
  });

  it("选中节点/接口：板块与节点一起摊开（否则选中项在左栏看不见）", () => {
    expect(seedOpen(T, { node: "images-gpt", ep: "ep-gen" })).toEqual({
      sections: ["images"],
      nodes: ["images-gpt"],
    });
  });

  it("树还没到手 / 路径不存在：回落到第一个有节点的板块，不抛错", () => {
    expect(seedOpen(undefined, { node: "x" })).toEqual(RAIL_CLOSED);
    expect(seedOpen(T, { node: "gone" })).toEqual({ sections: ["images"], nodes: [] });
  });

  it("开合板块不动节点，开合节点不动板块（互相独立，不是手风琴）", () => {
    const a = toggle({ sections: ["images"], nodes: ["images-gpt"] }, "section", "videos");
    expect(a).toEqual({ sections: ["images", "videos"], nodes: ["images-gpt"] });
    // 再点一次只收这一个，另一个板块还开着 —— 级联菜单的语义
    expect(toggle(a, "section", "videos")).toEqual({ sections: ["images"], nodes: ["images-gpt"] });
    expect(toggle({ sections: ["images"], nodes: [] }, "node", "images-gpt")).toEqual({
      sections: ["images"],
      nodes: ["images-gpt"],
    });
  });

  it("开合是幂等的取反：同一个 key 两次回到原样", () => {
    const one = toggle(RAIL_CLOSED, "node", "n1");
    expect(isOpen(one, "node", "n1")).toBe(true);
    expect(toggle(one, "node", "n1")).toEqual({ sections: [], nodes: [] });
  });

  it("openPath 只补选中项这条路径，别的行一个都不动", () => {
    const cur = { sections: ["videos"], nodes: [] };
    expect(openPath(cur, T, { node: "images-gpt" })).toEqual({
      sections: ["videos", "images"],
      nodes: ["images-gpt"],
    });
  });

  it("openPath 已可见时返回同一个对象（不触发多余重渲染）", () => {
    const cur = { sections: ["images"], nodes: ["images-gpt"] };
    expect(openPath(cur, T, { node: "images-gpt", ep: "ep-gen" })).toBe(cur);
    // 只选板块时不去动节点的展开位
    expect(openPath(cur, T, { section: "images" })).toBe(cur);
  });

  it("模块列表为空时返回同一个对象，不抛错", () => {
    const cur = { sections: ["images"], nodes: [] };
    expect(openPath(cur, undefined, { node: "x" })).toBe(cur);
  });
});

describe("isCurrent", () => {
  it("三个选中互斥：选了接口时节点行不再高亮", () => {
    const sel = { node: "images-gpt", ep: "ep-gen" };
    expect(isCurrent(sel, "ep", "ep-gen")).toBe(true);
    expect(isCurrent(sel, "node", "images-gpt")).toBe(false);
    expect(isCurrent(sel, "section", "images")).toBe(false);
  });

  it("只选节点时板块行不高亮（否则左栏同时亮两处）", () => {
    const sel = { node: "images-gpt" };
    expect(isCurrent(sel, "node", "images-gpt")).toBe(true);
    expect(isCurrent(sel, "section", "images")).toBe(false);
  });
});

describe("计数与红点", () => {
  it("草稿数把节点与接口都算进去", () => {
    const sec = T.sections[0];
    // 节点是 published，接口 ep-edit 是 draft
    expect(draftCount(sec)).toBe(1);
  });

  it("有未绑定接口时亮红点（发布了也会被拦）", () => {
    const node = T.sections[0].nodes[0];
    expect(hasUnbound(node)).toBe(false);
    node.endpoints.push(ep("ep-new", { bound: false }));
    expect(hasUnbound(node)).toBe(true);
  });
});

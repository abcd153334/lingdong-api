import type {
  DocsAdminEndpointRef,
  DocsAdminSection,
  DocsAdminTreeNode,
  DocsAdminTree,
} from "@/types/docsadmin";

/**
 * 左栏（板块 → 节点 → 接口）的纯数据视图、展开态与选中判据。
 *
 * 单独成文件是为了能单测：级联的展开/选中/计数都是纯函数，
 * 塞进组件里就只能靠 e2e 慢慢试。
 */

export interface RailRow {
  section: DocsAdminSection;
  nodes: RailNodeRow[];
}

export interface RailNodeRow {
  node: DocsAdminTreeNode;
  endpoints: DocsAdminEndpointRef[];
}

/** 目录 → 渲染行；未知板块（库里没有这一行）保持原顺序排在最后并标 unknown。 */
export function buildRail(tree: DocsAdminTree | undefined): RailRow[] {
  if (!tree) return [];
  return tree.sections.map((s) => ({
    section: s,
    nodes: s.nodes.map((n) => ({ node: n, endpoints: n.endpoints })),
  }));
}

/* ------------------------------ 展开态 ------------------------------ */

/**
 * 展开态 = 两级各一份 key 列表，**互相独立**。
 *
 * 为什么不是「选中谁就展开谁」：那是上一版的做法，症状是节点行没有箭头、
 * 也没法「只收起不换选中」—— 展开态被选中态绑死，收起等于换对象。
 * 级联菜单的展开态本来就是独立的 UI 状态：箭头开合，行主体选中，两件事。
 *
 * 为什么不是一级手风琴：级联的语义是「每级自己开合」。手风琴是上层为了
 * 掩盖"首屏七块全摊开"而加的限制，代价是打开第二块就关掉第一块 ——
 * 用户想同时看两个板块的接口列表时永远做不到。
 * 首屏不清爽的问题由 `seedOpen` 解决（只摊开一条路径），不是靠禁止多开。
 */
export interface RailOpen {
  sections: string[];
  nodes: string[];
}

export const RAIL_CLOSED: RailOpen = { sections: [], nodes: [] };

export type RailLevel = "section" | "node";

/** 某一级是否展开。 */
export function isOpen(open: RailOpen, level: RailLevel, key: string): boolean {
  return (level === "section" ? open.sections : open.nodes).includes(key);
}

/**
 * 开合某一级的一行：只动这一级的这一个 key。
 *
 * 另一级原样返回（**引用相同**），这样开合节点不会让板块列表重渲染。
 */
export function toggle(open: RailOpen, level: RailLevel, key: string): RailOpen {
  const cur = level === "section" ? open.sections : open.nodes;
  const next = cur.includes(key) ? cur.filter((k) => k !== key) : [...cur, key];
  return level === "section"
    ? { sections: next, nodes: open.nodes }
    : { sections: open.sections, nodes: next };
}

/** 选中项所在板块（节点/接口都算；未知或不存在返回 `""`）。 */
export function sectionOf(tree: DocsAdminTree | undefined, sel: Selection): string {
  if (!tree) return "";
  if (sel.node) {
    return tree.sections.find((s) => s.nodes.some((n) => n.key === sel.node))?.key ?? "";
  }
  if (sel.section && tree.sections.some((s) => s.key === sel.section)) return sel.section;
  return "";
}

/** 选中项所在节点（只选了板块时返回 `""`）。 */
export function nodeOf(tree: DocsAdminTree | undefined, sel: Selection): string {
  if (!tree || !sel.node) return "";
  return tree.sections.some((s) => s.nodes.some((n) => n.key === sel.node)) ? sel.node : "";
}

/**
 * 首屏种子：**只摊开一条路径**。
 *
 * 有选中 → 选中项所在板块 +（选了节点时）该节点；
 * 没选中 → 第一个有节点的板块，节点全收起（让用户从「节点」这一级开始点）。
 *
 * 这条规则替代了"手风琴"：首屏永远只有一条路径打开，层级一眼看得清；
 * 之后开几个由用户点箭头决定，程序不再替他收起别的。
 */
export function seedOpen(tree: DocsAdminTree | undefined, sel: Selection): RailOpen {
  if (!tree) return RAIL_CLOSED;
  const sec =
    sectionOf(tree, sel) || tree.sections.find((s) => s.nodes.length > 0)?.key || "";
  const node = nodeOf(tree, sel);
  return { sections: sec ? [sec] : [], nodes: node ? [node] : [] };
}

/**
 * 保证选中项「可见」：把它所在板块（和节点）放进展开位。别的行一个都不动。
 *
 * 只在选中变化时调用。用户手动收起当前节点后，`sel` 没变就不会再被撑开 ——
 * 这正是「箭头说了算」，而不是「选中说了算」。
 */
export function openPath(
  cur: RailOpen,
  tree: DocsAdminTree | undefined,
  sel: Selection,
): RailOpen {
  if (!tree) return cur;
  const sec = sectionOf(tree, sel);
  const node = nodeOf(tree, sel);
  const sections = sec && !cur.sections.includes(sec) ? [...cur.sections, sec] : cur.sections;
  const nodes = node && !cur.nodes.includes(node) ? [...cur.nodes, node] : cur.nodes;
  // 引用不变 = 不触发 setState 重渲染。
  return sections === cur.sections && nodes === cur.nodes ? cur : { sections, nodes };
}

/* ------------------------------ 计数与判据 ------------------------------ */

/** 计数：某板块下有多少篇草稿（侧栏上的数字用它）。 */
export function draftCount(sec: DocsAdminSection): number {
  let n = 0;
  for (const node of sec.nodes) {
    if (node.status === "draft") n++;
    for (const ep of node.endpoints) if (ep.status === "draft") n++;
  }
  return n;
}

/** 一个节点下有没有「未绑定平台端点」的接口（红点：发布了也会被拦）。 */
export function hasUnbound(node: DocsAdminTreeNode): boolean {
  return node.endpoints.some((e) => !e.bound);
}

/** 选中判据：URL 里的三个参数只允许一个组合成立。 */
export interface Selection {
  section?: string;
  node?: string;
  ep?: string;
}

/**
 * 纠偏：把非法组合收敛成一个合法选中。
 *
 * 三条互斥（顺序即优先级）：
 *  1. 节点不在树里（被删/被改名）→ 只保留板块；
 *  2. 接口不在该节点下 → 只保留节点；
 *  3. 都不在 → 保留板块本身（它在树里就仍然有效）。
 */
export function reconcile(
  tree: DocsAdminTree | undefined,
  sel: Selection,
): { sel: Selection; changed: boolean } {
  if (!tree) return { sel, changed: false };
  if (!sel.node) {
    // 只有 `?ep=` 没有 `?node=`：非法组合（手工改地址 / 旧链接 / 删除后残留）。
    // 接口属于节点，没有节点就没有"这个接口"——留着只会得到一片空白与坏链接。
    if (sel.ep) {
      return { sel: sel.section ? { section: sel.section } : {}, changed: true };
    }
    return { sel, changed: false };
  }
  // 按节点 key 找所属板块，不依赖 URL 里的 `s`：节点选中时 URL 只该有 `node`，
  // 多带一个 `s` 就变成"两个选中参数同时存在"的非法态。
  const node = tree.sections
    .flatMap((s) => s.nodes)
    .find((n) => n.key === sel.node);
  if (!node) {
    // 节点被删/改名：回到板块（`s` 还在就保留，否则交给页面选第一个）。
    return { sel: sel.section ? { section: sel.section } : {}, changed: true };
  }
  if (sel.ep && !node.endpoints.some((e) => e.key === sel.ep)) {
    return { sel: { node: node.key }, changed: true };
  }
  return { sel, changed: false };
}

/**
 * 侧栏某一行当前是否被选中（`aria-current`）。
 *
 * 三级互斥：选了接口就不再高亮节点，选了节点就不再高亮板块 ——
 * 否则左栏同时亮两处，读屏也会念两个"当前"。
 */
export function isCurrent(sel: Selection, kind: "section" | "node" | "ep", key: string): boolean {
  if (kind === "section") return !sel.node && sel.section === key;
  if (kind === "node") return !sel.ep && sel.node === key;
  return sel.ep === key;
}

/** 方法徽标：GET/POST 用两个字母宽度会跳，统一取全大写。 */
export function methodBadge(method: string): string {
  return (method || "POST").toUpperCase();
}

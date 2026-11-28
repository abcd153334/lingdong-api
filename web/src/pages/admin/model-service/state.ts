import type { ChannelItem } from "@/types";

/**
 * 左栏「浮层开在哪 / 右栏在看哪条渠道」的唯一判定处。
 *
 * 与改造前的 `selection.ts` 的区别（这是本次信息架构调整的核心）：
 *  - **左栏不再有"展开的组"**。适配器是卡片、渠道在浮层里，所以"展开哪个组"
 *    这个状态根本不存在了 —— 只剩"浮层开在哪（open）"和"右栏在看谁（c）"。
 *  - `open` **不进 URL**。它原本是 `?a`（展开的组），组没了之后，把它塞进 URL
 *    会立刻引出三个新问题：`a=B` 而 `c∈A` 时两处高亮谁优先、≤899 关抽屉后 `a`
 *    残留、孤儿桶需要第三个参数（`orphans` 是合法适配器 id，哨兵会撞）。
 *    它是"我正点着哪个"的瞬态，刷新后回到"没开浮层"才是对的。
 *
 * 三条约定：
 *  1. 打开浮层**不动 `c`**：右栏还显示着 A 的渠道时点开 B，右栏不该突然换掉 ——
 *     用户只是在"看看 B 有什么"。
 *  2. 选中渠道**必关浮层**：浮层是"选完就走"的导航，留着会压住刚切过去的表格。
 *  3. 删渠道 / 回全部模型**都清 `c`**：前者指向已不存在的行，后者是明确的"看全量"。
 */

/** 浮层开在哪个适配器上；`ORPHANS` 是孤儿桶的哨兵（不会与真实 id 相同：真实 id 不允许以 `#` 开头）。 */
export const ORPHANS = "#orphans";

export type FlyoutOpen = "" | typeof ORPHANS | string;

export interface MsState {
  /** 浮层开在哪（"" = 没开） */
  open: FlyoutOpen;
  /** 右栏正在看的渠道（undefined = 全部模型） */
  c?: number;
}

export type MsAction =
  | { kind: "openAdapter"; id: string }
  | { kind: "toggleAdapter"; id: string }
  | { kind: "openOrphans" }
  | { kind: "closeFlyout" }
  | { kind: "selectChannel"; channel: ChannelItem }
  | { kind: "showAll" }
  | { kind: "channelDeleted" };

export function nextState(s: MsState, a: MsAction): MsState {
  switch (a.kind) {
    case "openAdapter":
      return { open: a.id, c: s.c };
    case "toggleAdapter":
      return s.open === a.id ? { open: "", c: s.c } : { open: a.id, c: s.c };
    case "openOrphans":
      return s.open === ORPHANS ? { open: "", c: s.c } : { open: ORPHANS, c: s.c };
    case "closeFlyout":
      return { open: "", c: s.c };
    case "selectChannel":
      return { open: "", c: a.channel.id };
    case "showAll":
      return { open: "", c: undefined };
    case "channelDeleted":
      return { open: "", c: undefined };
  }
}

/**
 * 「右栏正在看其渠道」的适配器 id（决定卡片的 `aria-current`）。
 *
 * 与浮层状态**分开**表达是刻意的：同一张卡可能同时是"浮层开着"和"右栏在看它"，
 * 两个状态必须能分别渲染（`data-open` 吃主色底、`aria-current` 只吃主色字），
 * 否则三张卡里两张都是实心主色，一眼分不出哪个是当前视图。
 */
export function adapterInView(channel: ChannelItem | null): string {
  return channel?.adapter_id ?? "";
}

/** 浮层当前的目标适配器（孤儿桶返回 null，调用方据 `open === ORPHANS` 判形态）。 */
export function flyoutAdapterId(open: FlyoutOpen): string | null {
  if (!open || open === ORPHANS) return null;
  return open;
}

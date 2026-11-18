import type { ChannelItem } from "@/types";

/**
 * 左栏「展开哪个组 / 选中哪条渠道」的唯一判定处。
 *
 * 为什么单独一个纯函数模块：这四条规则每一条都有过一个 bug（点适配器清掉 `?c`、
 * 折叠永远不生效、`?a` 与 `?c` 冲突时选中的行看不见、点孤儿渠道让所有组塌掉）。
 * 混在 `useEffect` 里没法单测，只能靠 e2e 慢慢试；抽出来之后这四条各有一条用例。
 *
 * 三条约定（与页面上的注释一致）：
 *  1. `a` = 当前展开/聚焦的适配器；`c` = 选中的渠道。两者都进 URL。
 *  2. 折叠时**清 `a`、留 `c`**：右栏还在显示那条渠道的模型，刷新后按 `c` 重新展开到它。
 *  3. 跨适配器点击必须清 `c` —— 否则 `a=B, c∈A` 就是「选中的行在另一个组里」。
 */
export interface SelectionState {
  /** 当前展开的组（"" = 全部折叠） */
  open: string;
  /** 当前选中的渠道（null = 未选中） */
  channel: ChannelItem | null;
}

export type SelectionAction =
  | {
      /** 点组头：同组再点即收起 */
      kind: "toggle";
      id: string;
      /** 目标组下的渠道 id 集合（判断选中的渠道是否还留在视野里） */
      channelIds: readonly number[];
    }
  | {
      kind: "select";
      channel: ChannelItem;
      /** 已在左栏成组的适配器 id（不在里面 = 孤儿渠道） */
      knownAdapters: readonly string[];
    }
  | {
      /** 删除当前渠道：保留 a（回到该适配器的总览） */
      kind: "delete";
      adapterId: string | null;
    }
  | { kind: "collapseAll" };

export interface SelectionResult {
  /** 写入 URL 的 a（undefined = 删除该参数） */
  a?: string;
  /** 写入 URL 的 c（undefined = 删除该参数） */
  c?: number;
  /** 新的展开组（"" = 全部折叠） */
  open: string;
}

export function nextSelection(state: SelectionState, action: SelectionAction): SelectionResult {
  const selectedId = state.channel?.id;

  switch (action.kind) {
    case "toggle": {
      const collapse = state.open === action.id;
      // 收起的组里如果还留着当前选中的渠道，`c` 必须保留：
      // 右栏正在显示它，清掉等于「只是收起了左边，右边却白了」。
      const keepC = selectedId !== undefined && action.channelIds.includes(selectedId);
      return {
        a: collapse ? undefined : action.id,
        c: keepC ? selectedId : undefined,
        open: collapse ? "" : action.id,
      };
    }

    case "select": {
      const known = action.knownAdapters.includes(action.channel.adapter_id);
      return {
        // 孤儿渠道不写 `a`：写进去会让所有正常组都匹配不上而全部塌掉。
        a: known ? action.channel.adapter_id : undefined,
        c: action.channel.id,
        // 孤儿桶永远展开，不需要靠 openId 表达；保持现状，别把别人的组关了。
        open: known ? action.channel.adapter_id : state.open,
      };
    }

    case "delete":
      return { a: action.adapterId ?? undefined, c: undefined, open: state.open };

    case "collapseAll":
      return { a: undefined, c: selectedId, open: "" };
  }
}

/**
 * 当前应当「聚焦」的适配器：渠道优先于 `?a`。
 *
 * 渠道优先是刻意的：`?a=B&c=x∈A` 时若 `a` 优先，左栏展开 B、选中的那行藏在 A 里，
 * 用户看到的是"右栏在显示一条左栏找不到的渠道"。以 `c` 为准，`a` 由页面纠偏回填。
 */
export function focusOf(
  searchA: string | undefined,
  channel: ChannelItem | null,
  groupIds: readonly string[],
): string {
  if (channel && groupIds.includes(channel.adapter_id)) return channel.adapter_id;
  if (searchA && groupIds.includes(searchA)) return searchA;
  return "";
}

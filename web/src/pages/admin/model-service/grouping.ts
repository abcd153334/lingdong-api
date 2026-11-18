import type { AdapterInfo, ChannelItem } from "@/types";

/** 左栏的一组：一个适配器 + 它旗下的渠道。 */
export interface AdapterGroup {
  /** 适配器 id（`openai` / `spec:<slug>`）：URL 的 `a` 与渠道的 `adapter_id` 都认它 */
  id: string;
  label: string;
  /** builtin = 随进程发布，只读（不可删/不可改 DSL），但仍可挂渠道 */
  builtin: boolean;
  /** 图标可渲染地址（空 = 首字占位）。自建规格恒空 —— 它们没有自己的品牌图 */
  iconUrl: string;
  /**
   * 适配器自身被停用（规格可停用，内置恒为启用）。
   *
   * 与「渠道停用」是两件事：规格停用后它的渠道**依然挂在那里**（列表不过滤），
   * 但调用必然失败。左栏必须把这件事说出去，否则渠道看起来一切正常。
   */
  disabled: boolean;
  channels: ChannelItem[];
}

export interface RailModel {
  groups: AdapterGroup[];
  /**
   * `adapter_id` 已解析不到的渠道。
   *
   * 现实中会出现：规格被停用（停用不检查引用）或规格被删后残留的渠道。
   * 这些渠道在 `/v1` 上必然 503，如果左栏按 adapter_id 分组就直接把它们吞掉了 ——
   * 运营会看到"渠道数对不上"却不知道少的是哪条。单独成桶才能给出可操作入口。
   */
  orphans: ChannelItem[];
}

/**
 * 把「适配器列表 + 全量渠道」折成左栏结构。
 *
 * 排序是**稳定**的（同一输入必然同一输出）：
 *  - 适配器：自建在前（运营刚加的应该看得见），再按渠道数降序，最后按 id；
 *  - 渠道：启用在前 → 优先级升序 → id 升序。
 * 没有最后那个 id 兜底时，同 priority 的两条渠道顺序会随数据库返回顺序抖动，
 * 表现为"刷新一下顺序就变了"。
 */
export function groupChannels(adapters: AdapterInfo[], channels: ChannelItem[]): RailModel {
  const known = new Map<string, AdapterGroup>();
  for (const a of adapters) {
    known.set(a.id, {
      id: a.id,
      // label 的三级回落只在这里做：左栏/右栏/下拉都读 group.label，
      // 各写一遍的结果是某处显示 "openai"、某处显示 "OpenAI 兼容"。
      label: a.label || a.name || a.id,
      builtin: a.source === "builtin",
      iconUrl: a.icon_url ?? "",
      disabled: a.status !== 1,
      channels: [],
    });
  }
  const orphans: ChannelItem[] = [];
  for (const ch of channels) {
    const g = known.get(ch.adapter_id);
    if (g) g.channels.push(ch);
    else orphans.push(ch);
  }

  const groups = [...known.values()];
  for (const g of groups) g.channels.sort(compareChannel);
  groups.sort((a, b) => {
    if (a.builtin !== b.builtin) return a.builtin ? 1 : -1;
    if (a.channels.length !== b.channels.length) return b.channels.length - a.channels.length;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
  orphans.sort(compareChannel);
  return { groups, orphans };
}

/** 渠道排序：启用在前 → 优先级 → id（见 groupChannels 的注释）。 */
function compareChannel(a: ChannelItem, b: ChannelItem): number {
  const ea = a.status === 1 ? 0 : 1;
  const eb = b.status === 1 ? 0 : 1;
  if (ea !== eb) return ea - eb;
  if (a.priority !== b.priority) return a.priority - b.priority;
  return a.id - b.id;
}

/**
 * 渠道状态的唯一文案来源。
 *
 * 渠道条、左栏渠道行、适配器总览三处都要说「这条渠道不能用了」，
 * 各写一遍三元的结果是改文案时漏一处，两处显示不同状态。
 * 返回空串表示启用（调用方据此决定是否渲染 Tag）。
 */
export function channelStatusLabel(status: number): string {
  if (status === 3) return "自动停用";
  if (status !== 1) return "已停用";
  return "";
}

/** 某个渠道下的模型计数展示：`启用/总数`，两者相等时只显示一个数。 */
export function boundCountText(ch: ChannelItem): string {
  const total = ch.bound_models?.length ?? 0;
  const enabled = ch.bound_models?.filter((b) => b.enabled !== false).length ?? 0;
  if (total === 0) return "无模型";
  return enabled === total ? `${total} 个模型` : `${enabled}/${total} 个模型`;
}

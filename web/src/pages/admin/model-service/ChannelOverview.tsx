import { EmptyState } from "@/components/common/EmptyState";
import { EntityIcon } from "@/components/common/EntityIcon";
import { Tag } from "@/components/common/StatusTag";
import type { ChannelItem } from "@/types";

import { boundCountText, channelStatusLabel, type AdapterGroup } from "./grouping";

/**
 * 适配器的渠道总览（右栏在没有选中渠道时显示）。
 *
 * 它存在的唯一理由：点适配器**不能是一页空白**。用户点下去的心智是"看看这个适配器
 * 下面有什么"，所以这里给的是这个适配器旗下渠道的全貌（图标/地址/状态/模型数/密钥数），
 * 点任意一行就进入该渠道的模型列表。
 *
 * 三条刻意的取舍：
 *  1. **不用 DataTable**：数据已经在页面里（`channels` 全量），没有排序/分页需求；
 *     套上表格会把"导航面板"做成"又一张数据表"，还会带进 `.tbl` 的白底与斑马纹。
 *  2. **零行操作**：整行就是一个按钮，按钮里再嵌按钮是非法的；且「改这条渠道」的
 *     写路径只有一条（选中后的渠道条），两条路径迟早行为不一致。
 *  3. **不做搜索/筛选**：搜索只属于模型列表（左栏与总览都不做，这是需求口径）。
 *     顺序已按「启用在前 → priority → id」，100 条渠道是一次能滚完的量级；
 *     单个适配器超过 500 条时才值得引入服务端分页 + 搜索。
 */
export function ChannelOverview({
  group,
  onSelect,
  onCreateChannel,
}: {
  group: AdapterGroup;
  onSelect: (ch: ChannelItem) => void;
  onCreateChannel: (adapterId: string) => void;
}) {
  if (group.channels.length === 0) {
    return (
      <EmptyState
        title="这个适配器下还没有渠道"
        desc="先建一条渠道，再给它挂模型。"
        action={
          <button
            type="button"
            className="btn btn-sm btn-primary"
            onClick={() => onCreateChannel(group.id)}
          >
            新建渠道
          </button>
        }
      />
    );
  }

  return (
    <div className="ms-ov">
      <ul className="ms-ov__list">
        {group.channels.map((ch) => (
          <li key={ch.id}>
            <button type="button" className="ms-ov__row" onClick={() => onSelect(ch)}>
              <EntityIcon size="sm" url={ch.icon_url} name={ch.name} />
              <span className="ms-ov__main">
                <span className="ms-ov__name">{ch.name}</span>
                <span className="ms-ov__url mono">{ch.base_url}</span>
              </span>
              <span className="ms-ov__meta">
                {boundCountText(ch)} · 密钥 {ch.key_enabled_count}/{ch.key_count}
              </span>
              {ch.status === 1 ? null : <Tag tone="warning">{channelStatusLabel(ch.status)}</Tag>}
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

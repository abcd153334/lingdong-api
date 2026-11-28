import type { RefObject } from "react";

import { EmptyState } from "@/components/common/EmptyState";
import { Icon } from "@/components/common/Icon";
import { Popover } from "@/components/common/Popover";
import { Tag } from "@/components/common/StatusTag";
import { cn } from "@/lib/utils";
import type { ChannelItem } from "@/types";

import { channelStatusLabel, boundCountText } from "./grouping";

/**
 * 渠道二级面板：点左栏适配器卡片后，贴着左栏右缘浮出。
 *
 * 三条几何事实（决定了为什么参数长这样）：
 *  1. **水平锚是整栏、纵向锚是卡片**。卡片右缘比栏右缘内缩 10px，拿卡片当水平锚
 *     会让面板压进栏里；而纵向必须跟着被点的那张卡走（尖角要指向它）。
 *  2. `gap=0`：左缘与栏右缘**严丝合缝**，视觉上等同于"从栏里长出来"。
 *  3. `repositionKey` 必须跟着**适配器 id**走（不能用显示名）：从 A 卡切到 B 卡时
 *     `open` 恒为 true，不换键面板会停在 A 的坐标上（内容换了、位置没换）；
 *     而两个适配器完全可能同名（两条自建规格），拿 label 当键就会漏掉这种切换。
 *
 * 关闭语义（选渠道 / Esc / 点面板外 / 点「全部模型」）由调用方统一处理 ——
 * 面板只负责渲染与"选中即回报"。
 */
export function ChannelFlyout({
  open,
  adapterKey,
  label,
  channels,
  activeId,
  orphans,
  onClose,
  onSelect,
  onCreate,
  anchorRef,
  alignRef,
}: {
  open: boolean;
  /** 适配器 id（浮层定位的去重键：同名适配器也必须重新定位） */
  adapterKey: string;
  label: string;
  channels: ChannelItem[];
  /** 当前右栏正在看的渠道（0 = 没有） */
  activeId: number;
  /** 孤儿桶：底部不是「新建渠道」，而是改绑说明 */
  orphans: boolean;
  onClose: () => void;
  onSelect: (ch: ChannelItem) => void;
  onCreate: () => void;
  anchorRef: RefObject<HTMLElement | null>;
  alignRef: RefObject<HTMLElement | null>;
}) {
  return (
    <Popover
      open={open}
      onClose={onClose}
      anchorRef={anchorRef}
      alignRef={alignRef}
      returnFocusRef={alignRef}
      repositionKey={adapterKey}
      prefer="right"
      gap={0}
      arrow
      maxHeight={560}
      role="dialog"
      ariaLabel={`「${label}」的渠道`}
      className="ms-fly"
      tab="trap"
      focusOnOpen
      id="ms-fly"
    >
      <div className="ms-fly__head">
        <span className="ms-fly__title" title={label}>
          {label}
        </span>
        <span className="ms-fly__sub">{channels.length} 条渠道</span>
        <button type="button" className="icon-btn" aria-label="关闭渠道列表" onClick={onClose}>
          <Icon name="close" size={16} />
        </button>
      </div>

      <div className="ms-fly__list">
        {channels.length === 0 ? (
          <EmptyState title="这个适配器下还没有渠道" desc="点下方「新建渠道」接入第一条线路。" />
        ) : (
          channels.map((ch) => (
            <ChannelCard
              key={ch.id}
              ch={ch}
              active={ch.id === activeId}
              onSelect={onSelect}
              orphans={orphans}
            />
          ))
        )}
      </div>

      <div className="ms-fly__foot">
        {orphans ? (
          <p className="ms-fly__note">
            适配器已失效，这些渠道的调用会失败。到右栏「编辑渠道」把适配器改绑到有效项后即可恢复；
            此处不提供新建。
          </p>
        ) : (
          <button type="button" className="btn btn-primary" onClick={onCreate}>
            新建渠道
          </button>
        )}
      </div>
    </Popover>
  );
}

/** 一张渠道卡片：名称 + 状态 / 地址 / 元信息。浮层与抽屉钻取共用。 */
function ChannelCard({
  ch,
  active,
  onSelect,
  orphans,
}: {
  ch: ChannelItem;
  active: boolean;
  onSelect: (ch: ChannelItem) => void;
  orphans: boolean;
}) {
  return (
    <button
      type="button"
      className="ms-flych"
      data-channel={ch.id}
      aria-current={active ? "true" : undefined}
      onClick={() => onSelect(ch)}
    >
      <span className="ms-flych__t">
        <b>{ch.name}</b>
        {ch.status === 1 ? null : <Tag tone="warning">{channelStatusLabel(ch.status)}</Tag>}
      </span>
      <span className="ms-flych__url mono">{ch.base_url}</span>
      <span className="ms-flych__meta">
        {/* 计数走 `boundCountText`：停用的绑定也在 `bound_models` 里，直接数长度会把
            它们算成"可用模型"（"5 个模型"里可能有 2 条已停用，实际只路由 3 条）。 */}
        <span>{boundCountText(ch)}</span>
        <span aria-hidden="true">·</span>
        <span>
          密钥 {ch.key_enabled_count}/{ch.key_count}
        </span>
        {orphans ? null : ch.weight !== 1 ? (
          <>
            <span aria-hidden="true">·</span>
            <span>权重 {ch.weight}</span>
          </>
        ) : null}
        {orphans ? null : ch.priority !== 1 ? (
          <>
            <span aria-hidden="true">·</span>
            <span>优先级 {ch.priority}</span>
          </>
        ) : null}
      </span>
    </button>
  );
}

/**
 * ≤899 抽屉内的钻取视图：抽屉宽度只有 260px，浮层无处可浮，改为"同一块地方换内容"。
 *
 * 不做二级浮层是对的：移动端两层 Sheet 叠在一起，返回路径与手势返回会互相抢。
 */
export function ChannelDrill({
  label,
  channels,
  activeId,
  orphans,
  onBack,
  onSelect,
}: {
  label: string;
  channels: ChannelItem[];
  activeId: number;
  orphans: boolean;
  onBack: () => void;
  onSelect: (ch: ChannelItem) => void;
}) {
  return (
    <>
      <div className="ms-drill__top">
        <button type="button" className="btn btn-sm" onClick={onBack}>
          <span aria-hidden="true">←</span> 适配器
        </button>
        <b>{label}</b>
      </div>
      <div className={cn("ms-drill__list")}>
        {channels.length === 0 ? (
          <EmptyState title="这个适配器下还没有渠道" desc="点下方「新建渠道」接入第一条线路。" />
        ) : (
          channels.map((ch) => (
            <ChannelCard
              key={ch.id}
              ch={ch}
              active={ch.id === activeId}
              onSelect={onSelect}
              orphans={orphans}
            />
          ))
        )}
      </div>
    </>
  );
}

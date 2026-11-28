import { useEffect, useLayoutEffect, useRef } from "react";

import { EmptyState } from "@/components/common/EmptyState";
import { EntityIcon } from "@/components/common/EntityIcon";
import { Icon } from "@/components/common/Icon";
import { useOverlayBehavior } from "@/components/common/useOverlayBehavior";
import { cn } from "@/lib/utils";
import type { ChannelItem } from "@/types";

import { ChannelDrill, ChannelFlyout } from "./ChannelFlyout";
import type { RailModel } from "./grouping";
import { adapterInView, ORPHANS, type FlyoutOpen } from "./state";

/**
 * 左栏：**只有适配器**（200px 的卡片列表）。
 *
 * 与改造前的级联列表相比，这里少了两件事，而它们正是"看不懂"的来源：
 *  - 不再把渠道摊在栏里：两级用同一套 40px 行语言，父子只能靠缩进 22px 区分，
 *    而 200px 下缩进根本不够看；
 *  - 不再有"展开/折叠"：点卡片是**打开二级面板**，不是把下面的行撑开 ——
 *    前者是导航，后者是树，导航比树少一层心智负担。
 *
 * 三条结构约束：
 *  1. **「全部模型」在滚动容器之外**：它是默认视图的入口，适配器接口挂了也必须点得动。
 *  2. 卡片是**两行 grid**：行 1 = 图标 + 名称（ellipsis + title）+ `›`，行 2 = 渠道数 +
 *     内置/已停用。200px 下名称只有约 8 个汉字，任何徽标挤进行 1 都会把名字吃掉。
 *  3. 计数与「内置」对读屏隐藏，`aria-label` 单独给全名 + 条数 —— 否则读屏会念成
 *     "OpenAI 2 条渠道 · 内置 2"，而视觉上这些字是辅助信息。
 */
export function Rail({
  model,
  open,
  activeChannel,
  loading,
  error,
  onRetry,
  drawer,
  drawerOpen,
  onCloseDrawer,
  onToggleAdapter,
  onToggleOrphans,
  onShowAll,
  onSelectChannel,
  onCreateChannel,
  onCreateAdapter,
  onDrillBack,
  onRestorePresets,
}: {
  model: RailModel;
  /** 浮层开在哪（抽屉形态下复用为"钻取到哪个适配器"） */
  open: FlyoutOpen;
  activeChannel: ChannelItem | null;
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  /** ≤899 抽屉形态（页面判定一次，组件内不读 matchMedia） */
  drawer: boolean;
  drawerOpen: boolean;
  onCloseDrawer: () => void;
  /** 点适配器卡片：带上卡片节点，浮层用它做纵向锚点 */
  onToggleAdapter: (adapterId: string, card: HTMLElement | null) => void;
  onToggleOrphans: (card: HTMLElement | null) => void;
  onShowAll: () => void;
  onSelectChannel: (ch: ChannelItem) => void;
  onCreateChannel: (adapterId: string) => void;
  onCreateAdapter: () => void;
  /** 抽屉钻取返回适配器列表 */
  onDrillBack: () => void;
  /** 打开「恢复出厂」（只在存在内置行时渲染入口，见 hasBuiltin） */
  onRestorePresets: () => void;
}) {
  const ref = useRef<HTMLElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  /** 被点的那张卡：浮层的纵向锚点 + Esc 后的焦点落点（`<aside>` 不可聚焦） */
  const anchorCard = useRef<HTMLElement | null>(null);
  useOverlayBehavior(drawer && drawerOpen, onCloseDrawer, ref);

  // 兜底：浮层由非点击路径打开（例如以后加深链）时，也能找到锚点卡片。
  useLayoutEffect(() => {
    if (!open) {
      anchorCard.current = null;
      return;
    }
    const want = anchorCard.current?.dataset.adapter ?? anchorCard.current?.dataset.orphan;
    const id = open === ORPHANS ? "1" : open;
    if (want !== id) {
      anchorCard.current =
        listRef.current?.querySelector<HTMLElement>(
          open === ORPHANS ? "[data-orphan]" : `[data-adapter="${open}"]`,
        ) ?? null;
    }
  }, [open]);

  // 钻取时把当前渠道滚进视野：抽屉只有 260px 高，渠道多的时候选中项常在屏外。
  useEffect(() => {
    if (!drawer || !drawerOpen || !open) return;
    listRef.current
      ?.querySelector(`[data-channel="${activeChannel?.id ?? 0}"]`)
      ?.scrollIntoView({ block: "nearest" });
  }, [drawer, drawerOpen, open, activeChannel?.id]);

  const inView = adapterInView(activeChannel);
  const drilledGroup = open && open !== ORPHANS ? model.groups.find((g) => g.id === open) : null;
  const drilling = drawer && !!open && (open === ORPHANS || !!drilledGroup);

  /**
   * 是否存在「内置」行 —— 恢复出厂入口的唯一可见性判据。
   *
   * 不用一个全局开关决定：关掉内置预设的实例（`LD_BUILTIN_PRESETS=0`）本来就没有
   * 可恢复的行，按钮留着只会点出一个 400。按数据判断比按配置判断可靠，
   * 而且不需要多一个接口。
   */
  const hasBuiltin =
    model.groups.some((g) => g.channels.some((c) => !!c.preset_group)) ||
    model.orphans.some((c) => !!c.preset_group);

  const cards = (
    <>
      {model.groups.map((g) => (
        <button
          key={g.id}
          type="button"
          className="ms-adp"
          data-adapter={g.id}
          data-open={open === g.id ? "" : undefined}
          aria-current={g.id === inView ? "true" : undefined}
          aria-label={`${g.label}，${g.channels.length} 条渠道`}
          title={g.label}
          onClick={(e) => {
            anchorCard.current = e.currentTarget;
            onToggleAdapter(g.id, e.currentTarget);
          }}
        >
          <span className="ms-adp__ico">
            <EntityIcon size="xs" url={g.iconUrl} name={g.label} />
          </span>
          <span className="ms-adp__name">{g.label}</span>
          <Icon name="chevronRight" size={14} className="ms-adp__chev" />
          <span className="ms-adp__meta" aria-hidden="true">
            <span>{g.channels.length} 条渠道</span>
            <span>·</span>
            {g.disabled ? (
              <span className="is-off">已停用</span>
            ) : (
              <span>{g.builtin ? "内置" : "自建"}</span>
            )}
          </span>
        </button>
      ))}

      {model.orphans.length > 0 ? (
        <button
          type="button"
          className="ms-adp ms-adp--orphan"
          data-orphan="1"
          data-open={open === ORPHANS ? "" : undefined}
          aria-label={`未登记适配器，${model.orphans.length} 条渠道`}
          title="未登记适配器"
          onClick={(e) => {
            anchorCard.current = e.currentTarget;
            onToggleOrphans(e.currentTarget);
          }}
        >
          <span className="ms-adp__ico ms-adp__ico--warn">
            <Icon name="alert" size={15} />
          </span>
          <span className="ms-adp__name">未登记适配器</span>
          <Icon name="chevronRight" size={14} className="ms-adp__chev" />
          <span className="ms-adp__meta" aria-hidden="true">
            <span>{model.orphans.length} 条渠道</span>
            <span>·</span>
            <span className="is-off">已失效</span>
          </span>
        </button>
      ) : null}
    </>
  );

  return (
    <>
      {drawer && drawerOpen ? (
        <div className="ms-scrim" aria-hidden="true" onClick={onCloseDrawer} />
      ) : null}

      <aside
        ref={ref}
        className="ms-rail"
        aria-label="适配器"
        // 布尔，不能写字符串：`inert="false"` 是真值，整栏会点不动。
        inert={drawer && !drawerOpen ? true : undefined}
        role={drawer && drawerOpen ? "dialog" : undefined}
        aria-modal={drawer && drawerOpen ? true : undefined}
      >
        {drawer ? (
          <div className="ms-rail__head">
            <span className="ms-rail__title">适配器</span>
            <button
              type="button"
              className="icon-btn"
              aria-label="关闭适配器列表"
              onClick={onCloseDrawer}
            >
              <Icon name="close" size={18} />
            </button>
          </div>
        ) : null}

        {/* 入口固定在顶部、不随列表滚动：它是默认视图，任何时候都该在同一个位置 */}
        <div className="ms-rail__top">
          <button
            type="button"
            className="ms-all"
            aria-label="全部模型"
            aria-current={activeChannel ? undefined : "true"}
            onClick={onShowAll}
          >
            <Icon name="grid" size={16} />
            <span>全部模型</span>
          </button>
          {/* 钻取时不再画「适配器」小标题：下面那行本身就是「← 适配器 <名字>」，
              两处并排会让"适配器"这个词在同一屏出现三次 */}
          {drilling ? null : <div className="ms-rail__label">适配器</div>}
        </div>

        <div className="ms-rail__list" ref={listRef}>
          {loading ? (
            <EmptyState variant="loading" title="加载中…" />
          ) : error ? (
            <EmptyState
              variant="error"
              title="加载失败"
              action={
                <button type="button" className="btn btn-sm" onClick={onRetry}>
                  重试
                </button>
              }
            />
          ) : drilling ? (
            <ChannelDrill
              label={open === ORPHANS ? "未登记适配器" : (drilledGroup?.label ?? "")}
              channels={open === ORPHANS ? model.orphans : (drilledGroup?.channels ?? [])}
              activeId={activeChannel?.id ?? 0}
              orphans={open === ORPHANS}
              onBack={onDrillBack}
              onSelect={onSelectChannel}
            />
          ) : model.groups.length === 0 && model.orphans.length === 0 ? (
            <EmptyState title="没有可用的适配器" />
          ) : (
            <div className="ms-rail__cards">{cards}</div>
          )}
        </div>

        <div className="ms-rail__foot">
          {drilling ? (
            <button
              type="button"
              className="btn btn-primary ms-rail__new"
              // 孤儿桶没有可用的适配器，建渠道必然失败
              disabled={open === ORPHANS}
              onClick={() => onCreateChannel(open === ORPHANS ? "" : open)}
            >
              新建渠道
            </button>
          ) : (
            <button type="button" className="ms-new" onClick={onCreateAdapter}>
              <Icon name="plus" size={15} />
              <span>新建适配器</span>
            </button>
          )}
          {/* 维护类动作，不是"在这里新建"：做成弱按钮，与上面那个主按钮分组 */}
          {hasBuiltin ? (
            <button type="button" className="ms-reset" onClick={onRestorePresets}>
              恢复出厂…
            </button>
          ) : null}
        </div>
      </aside>

      {/* 浮层只在桌面渲染：≤899 走抽屉钻取，两层 Sheet 会互相抢手势与返回路径 */}
      {!drawer ? (
        <ChannelFlyout
          open={!!open}
          // 用适配器 id 而不是显示名做定位键：同名适配器之间切换也必须重新定位
          adapterKey={open === ORPHANS ? ORPHANS : open}
          label={open === ORPHANS ? "未登记适配器" : (drilledGroup?.label ?? "")}
          channels={open === ORPHANS ? model.orphans : (drilledGroup?.channels ?? [])}
          activeId={activeChannel?.id ?? 0}
          orphans={open === ORPHANS}
          onClose={onDrillBack}
          onSelect={onSelectChannel}
          onCreate={() => onCreateChannel(open === ORPHANS ? "" : open)}
          anchorRef={ref}
          alignRef={anchorCard}
        />
      ) : null}
    </>
  );
}

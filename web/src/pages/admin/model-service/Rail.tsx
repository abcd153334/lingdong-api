import { useEffect, useRef } from "react";

import { EmptyState } from "@/components/common/EmptyState";
import { Icon } from "@/components/common/Icon";
import { EntityIcon } from "@/components/common/EntityIcon";
import { Tag } from "@/components/common/StatusTag";
import { useOverlayBehavior } from "@/components/common/useOverlayBehavior";
import { cn } from "@/lib/utils";
import type { ChannelItem } from "@/types";

import { boundCountText, channelStatusLabel, type RailModel } from "./grouping";

/**
 * 左栏：适配器 → 渠道的级联列表。
 *
 * 视觉规格对标全局侧栏 `.a-side .nav-row`（40px / 13.5px / 圆角 10 / hover 与选中
 * 只有一种色块语言），几何算死在 `model-service.css` 的文件头：
 * 适配器与渠道的图标都落在 x=44、文字都落在 x=70 —— 两级对齐是「级联」与
 * 「两段互不相关的列表」的唯一区别。
 *
 * 四条结构决定（都为了避免「看起来能用、点起来错」）：
 *  1. **不是 `role="tree"`**：treeitem 要求"一个可聚焦元素代表一行"，而适配器行要
 *     额外一个「新建渠道」按钮。用 `aria-expanded` 分组按钮 + 组内按钮单选，
 *     Tab 顺序天然正确，读屏也能念出"已展开/已折叠"。
 *  2. 组头与行尾「+」是**兄弟 button**（不嵌套、不用 stopPropagation）：嵌套按钮
 *     会被浏览器重排 DOM，`getByRole` 的层级关系随之变化。
 *  3. 「+」**常驻且始终可用**：不按展开态卸载（行宽会跳 26px、chevron 位移），
 *     也不用 `visibility:hidden` 冒充禁用（键盘仍能 Tab 进去）。
 *     展开态末尾另有虚线槽，两个入口一个在顶一个在底，长列表里都够得着。
 *  4. 折叠组**条件渲染**（不在 DOM）：100+ 渠道时只有当前那一组进 DOM，
 *     Tab 序与读屏都不会滚过几百个不可见按钮。`aria-controls` 随之只在展开时声明
 *     （指向不存在的节点比不声明更糟）。
 */
export function Rail({
  model,
  openId,
  focusId,
  activeChannel,
  loading,
  error,
  onRetry,
  drawer,
  drawerOpen,
  onCloseDrawer,
  onToggleAdapter,
  onSelectChannel,
  onCreateChannel,
  onCreateAdapter,
}: {
  model: RailModel;
  /** 当前展开的组（"" = 全部折叠） */
  openId: string;
  /** 含当前选中渠道的组：该组被收起时组头转主色，提示「右栏还在看它」 */
  focusId: string;
  /** 当前选中的渠道 id；0 = 未选中 */
  activeChannel: number;
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  /** 是否处于 ≤899 的抽屉形态（由页面判定一次，组件内不读 matchMedia） */
  drawer: boolean;
  drawerOpen: boolean;
  onCloseDrawer: () => void;
  onToggleAdapter: (adapterId: string) => void;
  onSelectChannel: (ch: ChannelItem) => void;
  onCreateChannel: (adapterId: string) => void;
  onCreateAdapter: () => void;
}) {
  const ref = useRef<HTMLElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  // 抽屉是页内弹层：Esc / 焦点环 / 滚动锁 / 关闭后焦点归还全部复用同一实现。
  useOverlayBehavior(drawer && drawerOpen, onCloseDrawer, ref);

  // 选中项滚进可视区。依赖必须带 `model`：新建渠道时先写入 pending 对象、
  // 列表刷新后才换成真实行，两者 id 相同 —— 只依赖 id 不会重跑，新渠道就滚不到。
  useEffect(() => {
    if (!activeChannel) return;
    listRef.current
      ?.querySelector(`[data-channel="${activeChannel}"]`)
      ?.scrollIntoView({ block: "nearest" });
  }, [activeChannel, openId, model]);

  return (
    <>
      {drawer && drawerOpen ? (
        <div className="ms-scrim" aria-hidden="true" onClick={onCloseDrawer} />
      ) : null}

      <aside
        ref={ref}
        className="ms-rail"
        aria-label="适配器与渠道"
        // 布尔，不能写字符串：`inert="false"` 是真值，整栏会点不动。
        inert={drawer && !drawerOpen ? true : undefined}
        role={drawer && drawerOpen ? "dialog" : undefined}
        aria-modal={drawer && drawerOpen ? true : undefined}
      >
        {/* 抽屉形态才需要标题与关闭入口：焦点陷阱里必须有一条可点的退路 */}
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
          ) : model.groups.length === 0 && model.orphans.length === 0 ? (
            <EmptyState title="没有可用的适配器" />
          ) : (
            <>
              {model.groups.map((g, i) => {
                const open = g.id === openId;
                const chsId = `ms-chs-${i}`;
                return (
                  <div key={g.id} className="ms-grp" data-adapter={g.id}>
                    <div className="ms-grp__head">
                      <button
                        type="button"
                        className={cn(
                          "ms-gh",
                          open && "is-open",
                          // 收起时仍标出「选中的渠道在这一组里」，与侧栏 `.nav-group.on` 同一条规则
                          !open && g.id === focusId && "is-focus",
                        )}
                        aria-expanded={open}
                        aria-controls={open ? chsId : undefined}
                        onClick={() => onToggleAdapter(g.id)}
                      >
                        <Icon name="chevronDown" size={14} className="ms-gh__caret" />
                        <EntityIcon size="xs" url={g.iconUrl} name={g.label} />
                        <span className="ms-gh__label">{g.label}</span>
                        {g.disabled ? (
                          <span className="ms-gh__tag is-off">已停用</span>
                        ) : g.builtin ? (
                          <span className="ms-gh__tag">内置</span>
                        ) : null}
                        {/* 计数不进可访问名：读屏应该念实体名，不是「OpenAI 内置 2」 */}
                        <span className="ms-gh__count" aria-hidden="true">
                          {g.channels.length}
                        </span>
                      </button>
                      <button
                        type="button"
                        className="ms-gh__add"
                        // 名称里带适配器名：折叠态下同时有多行各有一个「+」，
                        // 只叫"新建渠道"会让读屏与 e2e 都无法区分是哪一行。
                        aria-label={`在「${g.label}」下新建渠道`}
                        onClick={() => onCreateChannel(g.id)}
                      >
                        <Icon name="plus" size={14} />
                      </button>
                    </div>

                    {open ? (
                      <div className="ms-chs" id={chsId}>
                        {g.channels.map((ch) => (
                          <button
                            key={ch.id}
                            type="button"
                            className="ms-ch"
                            data-channel={ch.id}
                            aria-current={ch.id === activeChannel ? "true" : undefined}
                            onClick={() => onSelectChannel(ch)}
                          >
                            <EntityIcon size="xs" url={ch.icon_url} name={ch.name} />
                            <span className="ms-ch__name">{ch.name}</span>
                            {ch.status === 1 ? (
                              <span className="ms-ch__meta">{boundCountText(ch)}</span>
                            ) : (
                              <Tag tone="warning">{channelStatusLabel(ch.status)}</Tag>
                            )}
                          </button>
                        ))}
                        {/* 固定槽：永远在最后一项，虚线边框表示"这里还能加一个"。
                            aria-label 必须逐字是「新建渠道」—— 全页唯一，e2e 用 exact 定位。 */}
                        <button
                          type="button"
                          className="ms-add-dashed"
                          aria-label="新建渠道"
                          onClick={() => onCreateChannel(g.id)}
                        >
                          <Icon name="plus" size={13} />
                          <span>新建渠道</span>
                        </button>
                      </div>
                    ) : null}
                  </div>
                );
              })}

              {model.orphans.length > 0 ? (
                <div className="ms-grp ms-grp--orphan">
                  <div className="ms-grp__head">
                    {/* 不可点、不可折叠：这里没有可展开的适配器，只有一个需要处理的桶 */}
                    <div className="ms-gh ms-gh--static">
                      <Icon name="alert" size={18} className="ms-gh__warn" />
                      <span className="ms-gh__label">未登记适配器</span>
                      <span className="ms-gh__count" aria-hidden="true">
                        {model.orphans.length}
                      </span>
                    </div>
                  </div>
                  <div className="ms-chs">
                    {model.orphans.map((ch) => (
                      <button
                        key={ch.id}
                        type="button"
                        className="ms-ch"
                        data-channel={ch.id}
                        aria-current={ch.id === activeChannel ? "true" : undefined}
                        onClick={() => onSelectChannel(ch)}
                      >
                        <EntityIcon size="xs" url={ch.icon_url} name={ch.name} />
                        <span className="ms-ch__name">{ch.name}</span>
                        <span className="ms-ch__meta is-off">适配器失效</span>
                      </button>
                    ))}
                  </div>
                  {/* 只写左栏做得到的事：「点开后可改绑」是渠道条上的操作，不在这里承诺 */}
                  <p className="ms-grp__note">适配器已失效，调用会失败。</p>
                </div>
              ) : null}
            </>
          )}
        </div>

        <div className="ms-rail__foot">
          <button type="button" className="ms-new" onClick={onCreateAdapter}>
            <Icon name="plus" size={15} />
            <span>新建适配器</span>
          </button>
        </div>
      </aside>
    </>
  );
}

import { useEffect, useRef } from "react";

import { EmptyState } from "@/components/common/EmptyState";
import { Icon } from "@/components/common/Icon";
import { useOverlayBehavior } from "@/components/common/useOverlayBehavior";
import { cn } from "@/lib/utils";
import type { DocsAdminTree } from "@/types/docsadmin";

import {
  buildRail,
  draftCount,
  hasUnbound,
  isCurrent,
  isOpen,
  methodBadge,
  type RailLevel,
  type RailOpen,
  type Selection,
} from "./railModel";

/**
 * 左栏：板块 → 节点 → 接口的三级级联。
 *
 * ## 一、每一行都是同三个槽（结构，不是样式）
 *
 *   [ 箭头 20 ][ 主体 flex ][ 行尾动作 ]
 *
 * 三级共用这一个结构，区别只有缩进（每级 +18）与主体里的第一个元素：
 *
 *   板块  `箭头 + 色块 + 名称`
 *   节点  `箭头 + 图标 + 标题`
 *   接口  `空位 + 方法 + 名称`   ← 叶子没有子项，仍然占住箭头那一列
 *
 * 箭头列**永远存在**（叶子用空 span 占位）。这就是级联菜单与"三段互不相干的
 * 列表"的唯一区别：不看有没有箭头，只看箭头列与文字列是否成阶梯。
 *
 * ## 二、箭头与主体是两件事
 *
 *   - 箭头按钮：只管展开/收起，**不动选中**（`aria-expanded` 在它身上）
 *   - 主体按钮：只管选中（`aria-current` 在行上），右栏随即显示该对象的表单
 *
 * 上一版把"选中"和"展开"绑在一起（选中节点 = 自动展开、节点行没有箭头），
 * 于是「只收起不换对象」做不到、节点行看着像叶子、层级只能靠缩进猜。
 * 拆开之后：点箭头只影响展开态，点主体只影响选中态，互不牵连。
 *
 * ## 三、为什么不是 `role="tree"`
 *
 * treeitem 要求"一个可聚焦元素代表一行"，而这里一行里有两个按钮（箭头 + 主体），
 * 节点/板块行还有行尾「+」。用 `aria-expanded` 的按钮 + 行内单选，Tab 序天然正确，
 * 读屏也能念出"已展开/已折叠"。
 *
 * ## 四、折叠组条件渲染（不在 DOM）
 *
 * 80+ 接口全进 DOM 会让 Tab 序滚过一屏，读屏也要多念一遍。
 * `aria-controls` 随之只在展开时声明（指向不存在的节点比不声明更糟）。
 */
export function Rail({
  tree,
  loading,
  error,
  onRetry,
  sel,
  open,
  drawer,
  drawerOpen,
  onCloseDrawer,
  onSelect,
  onToggle,
  onCreateEndpoint,
  onCreateNode,
}: {
  tree: DocsAdminTree | undefined;
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  sel: Selection;
  /** 两级各自的展开位（见 railModel 的 RailOpen） */
  open: RailOpen;
  drawer: boolean;
  drawerOpen: boolean;
  onCloseDrawer: () => void;
  onSelect: (next: Selection) => void;
  /** 箭头点击：只开合这一级这一行，不换选中 */
  onToggle: (level: RailLevel, key: string) => void;
  onCreateEndpoint: (nodeKey: string) => void;
  onCreateNode: (sectionKey: string) => void;
}) {
  const ref = useRef<HTMLElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  // 抽屉：Esc / 焦点环 / 滚动锁 / 焦点归还全部复用同一实现。
  useOverlayBehavior(drawer && drawerOpen, onCloseDrawer, ref);

  const rows = buildRail(tree);

  // 选中项滚进可视区。依赖带 tree：新建后列表会换成真实行，只依赖 key 不会重跑。
  useEffect(() => {
    const key = sel.ep ?? sel.node ?? sel.section;
    if (!key) return;
    listRef.current
      ?.querySelector(`[data-doc-row="${key}"]`)
      ?.scrollIntoView({ block: "nearest" });
  }, [sel, tree]);

  return (
    <>
      {drawer && drawerOpen ? (
        <div className="adp-scrim" aria-hidden="true" onClick={onCloseDrawer} />
      ) : null}

      <aside
        ref={ref}
        className="adp-rail"
        aria-label="文档目录"
        data-open={drawerOpen ? "1" : undefined}
        inert={drawer && !drawerOpen ? true : undefined}
        role={drawer && drawerOpen ? "dialog" : undefined}
        aria-modal={drawer && drawerOpen ? true : undefined}
      >
        {drawer ? (
          <div className="adp-rail__head">
            <span className="adp-rail__title">文档目录</span>
            <button type="button" className="icon-btn" aria-label="关闭目录" onClick={onCloseDrawer}>
              <Icon name="close" size={18} />
            </button>
          </div>
        ) : null}

        <div className="adp-rail__list" ref={listRef}>
          {loading ? (
            <EmptyState variant="loading" title="加载中…" />
          ) : error ? (
            <EmptyState
              variant="error"
              title="目录加载失败"
              action={
                <button type="button" className="btn btn-sm" onClick={onRetry}>
                  重试
                </button>
              }
            />
          ) : rows.length === 0 ? (
            <EmptyState title="还没有文档" desc="先新建一个节点，再往里加接口。" />
          ) : (
            rows.map(({ section, nodes }) => {
              const secOpen = isOpen(open, "section", section.key);
              const drafts = draftCount(section);
              return (
                <div className="adp-group" key={section.key}>
                  {/* ① 板块行 */}
                  <div
                    className="adp-row"
                    data-level="1"
                    aria-current={isCurrent(sel, "section", section.key) || undefined}
                  >
                    <button
                      type="button"
                      className="adp-row__caret"
                      aria-expanded={secOpen}
                      aria-label={secOpen ? "收起" : "展开"}
                      title={`${secOpen ? "收起" : "展开"}「${section.name}」`}
                      onClick={() => onToggle("section", section.key)}
                    >
                      <Icon
                        name="chevronRight"
                        size={14}
                        className={cn("adp-caret", secOpen && "is-open")}
                      />
                    </button>
                    <button
                      type="button"
                      className="adp-row__hit"
                      onClick={() => onSelect({ section: section.key })}
                    >
                      <span
                        className="adp-dot"
                        style={section.color ? { background: section.color } : undefined}
                        aria-hidden="true"
                      />
                      <span className="adp-row__label">{section.name}</span>
                      {section.unknown ? (
                        <span className="adp-flag" title="库里没有这个板块，节点归属无效">
                          !
                        </span>
                      ) : null}
                      {section.hidden ? (
                        <span className="adp-flag" title="已隐藏：读者看不到">
                          隐
                        </span>
                      ) : null}
                      {drafts > 0 ? (
                        <span className="adp-count" aria-hidden="true">
                          {drafts}
                        </span>
                      ) : null}
                    </button>
                    <button
                      type="button"
                      className="adp-row__act"
                      aria-label={`在 ${section.name} 下新建节点`}
                      title={`在「${section.name}」下新建节点`}
                      onClick={() => onCreateNode(section.key)}
                    >
                      <Icon name="plus" size={14} />
                    </button>
                  </div>

                  {/* ② 节点行 → ③ 接口行 */}
                  {secOpen
                    ? nodes.map(({ node, endpoints }) => {
                        const nodeOpen = isOpen(open, "node", node.key);
                        const epsId = `adp-eps-${node.key}`;
                        return (
                          <div className="adp-node" key={node.key}>
                            <div
                              className="adp-row"
                              data-level="2"
                              data-doc-row={node.key}
                              aria-current={isCurrent(sel, "node", node.key) || undefined}
                            >
                              <button
                                type="button"
                                className="adp-row__caret"
                                aria-expanded={nodeOpen}
                                aria-controls={nodeOpen ? epsId : undefined}
                                aria-label={nodeOpen ? "收起" : "展开"}
                                title={`${nodeOpen ? "收起" : "展开"}「${node.title}」的接口`}
                                onClick={() => onToggle("node", node.key)}
                              >
                                <Icon
                                  name="chevronRight"
                                  size={14}
                                  className={cn("adp-caret", nodeOpen && "is-open")}
                                />
                              </button>
                              <button
                                type="button"
                                className="adp-row__hit"
                                onClick={() => onSelect({ section: section.key, node: node.key })}
                              >
                                <Icon name="doc" size={14} />
                                <span className="adp-row__label">{node.title}</span>
                                {node.status === "draft" ? (
                                  <span className="adp-dot adp-dot--draft" title="草稿" />
                                ) : null}
                                {hasUnbound(node) ? (
                                  <span
                                    className="adp-dot adp-dot--warn"
                                    title="有接口未绑定平台端点"
                                  />
                                ) : null}
                                {node.hidden ? (
                                  <span className="adp-flag" title="已隐藏">
                                    隐
                                  </span>
                                ) : null}
                              </button>
                              <button
                                type="button"
                                className="adp-row__act"
                                aria-label={`在 ${node.title} 下新建接口`}
                                onClick={() => onCreateEndpoint(node.key)}
                              >
                                <Icon name="plus" size={14} />
                              </button>
                            </div>

                            {nodeOpen ? (
                              <div className="adp-eps" id={epsId}>
                                {endpoints.map((ep) => (
                                  <button
                                    type="button"
                                    key={ep.key}
                                    className="adp-row adp-row--ep"
                                    data-level="3"
                                    data-doc-row={ep.key}
                                    aria-current={isCurrent(sel, "ep", ep.key) || undefined}
                                    onClick={() =>
                                      onSelect({
                                        section: section.key,
                                        node: node.key,
                                        ep: ep.key,
                                      })
                                    }
                                  >
                                    {/* 叶子没有子项，箭头列留空位：三级文字才成阶梯 */}
                                    <span className="adp-row__caret adp-caret--leaf" aria-hidden="true" />
                                    <span className="adp-row__body">
                                      <span
                                        className={cn(
                                          "adp-method",
                                          `m-${methodBadge(ep.method).toLowerCase()}`,
                                        )}
                                      >
                                        {methodBadge(ep.method)}
                                      </span>
                                      <span className="adp-row__label">{ep.name}</span>
                                      {!ep.bound ? (
                                        <span
                                          className="adp-dot adp-dot--danger"
                                          title="未绑定平台端点"
                                        />
                                      ) : ep.status === "draft" ? (
                                        <span className="adp-dot adp-dot--draft" title="草稿" />
                                      ) : null}
                                      {ep.hidden ? (
                                        <span className="adp-flag" title="已隐藏">
                                          隐
                                        </span>
                                      ) : null}
                                    </span>
                                  </button>
                                ))}
                              </div>
                            ) : null}
                          </div>
                        );
                      })
                    : null}
                </div>
              );
            })
          )}
        </div>

        <div className="adp-rail__foot">
          <button
            type="button"
            className="btn btn-ghost btn-sm adp-addnode"
            onClick={() => onCreateNode(rows[0]?.section.key ?? "")}
          >
            <Icon name="plus" size={14} />
            新建节点
          </button>
        </div>
      </aside>
    </>
  );
}

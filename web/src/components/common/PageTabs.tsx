import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Tabs } from "./Tabs";
import type { PageTab } from "@/lib/pageTabs";

export interface PageTabsProps<K extends string> {
  tabs: readonly PageTab<K>[];
  value: K;
  onChange: (k: K) => void;
  /** 页面内唯一常量，必须传字面量：admin-config / portal-usage / admin-logs … */
  idPrefix: string;
  label: string;
  variant?: "line" | "pill";
  /**
   * 需要打「未保存」小圆点的页签（系统配置按页签保存后才需要）。
   *
   * 走这个 prop 而不是让调用方自己拼 tabs：`tabs` 必须是模块级常量，
   * 每次 render 新建数组会让 `usePageTabs` 里的 `setTab` 跟着换身份。
   */
  markedKeys?: readonly K[];
  /**
   * 保活**已访问过**的面板（默认关闭）。
   *
   * 默认只渲染当前面板，切页签会卸载旧面板 —— 组件内的表单草稿随之销毁。
   * 需要"切走再回来内容还在"的页面显式开启；未访问过的页签仍不挂载，
   * 所以不会把五份数据一次全拉下来。
   */
  keepMounted?: boolean;
  /** render-prop：默认只渲染当前面板（不构造全部），开启 keepMounted 后渲染已访问过的那些 */
  children: (tab: K) => ReactNode;
}

/**
 * 页面级页签 + 面板（唯一 tablist 出口，tablist 由 Tabs 渲染，不重复实现）。
 *
 * 硬规矩：
 * - **禁止给面板加 `key={value}`**：会给子树换 key → 整棵卸载，未保存的编辑全丢；
 * - 面板**不加 `tabIndex`**：本项目的面板都含可聚焦控件，加 0 只会多一个空 tab stop；
 * - 卡头内的页签不要用本组件（`role=tabpanel` 塞进 `<h3>` 是非法结构）。
 *
 * 页签条要放进页面自己的顶栏（如 `/admin/docs`）时用 `PageTabPanels`：
 * 面板逻辑与 `PageTabs` 共用同一个 hook，**不要**手写第二套 tabpanel。
 * 第二个用例是「应用管理弹窗」：页签住在 `.dl-head`（`Dialog` 的 `headExtra`），
 * 只有面板留在 body 里 —— 那时页签条不在滚动容器内，"不随内容滚"由结构保证。
 */
export function PageTabs<K extends string>({
  tabs,
  value,
  onChange,
  idPrefix,
  label,
  variant = "line",
  markedKeys,
  keepMounted = false,
  children,
}: PageTabsProps<K>) {
  const panelId = `${idPrefix}-panel-${value}`;
  // 只在有标记时才拷贝：没有 markedKeys 的页面保持传入数组原样（不产生多余对象）
  const items = useMemo(
    () =>
      markedKeys?.length
        ? tabs.map((t) => (markedKeys.includes(t.key) ? { ...t, mark: true } : t))
        : tabs,
    [tabs, markedKeys],
  );
  // 面板集合 = 当前页签集合：页签在运行期消失（如卸载插件）时用来回收面板，
  // 否则 `seen` 只增不减，被移除页签的面板会常驻并继续持有请求。
  const keys = useMemo(() => tabs.map((t) => t.key), [tabs]);

  return (
    <div className="page-tabs">
      {/* 吸附层：滚动时页签条钉在顶栏下沿（样式见 components.css 的 .page-tabs-bar）。
          必须由外层承担 —— 胶囊型页签是 inline-flex，自身背景盖不满整行。 */}
      <div className="page-tabs-bar">
        <Tabs
          items={items}
          value={value}
          onChange={(k) => onChange(k as K)}
          variant={variant}
          idPrefix={idPrefix}
          label={label}
          panelId={panelId}
        />
      </div>
      <PanelStack value={value} idPrefix={idPrefix} keepMounted={keepMounted} keys={keys}>
        {children}
      </PanelStack>
    </div>
  );
}

/**
 * 只有面板、没有页签条的版本：页签条已经渲染在页面顶栏里时用它。
 *
 * 与 `PageTabs` 共用 `PanelStack`（同一份保活逻辑），所以 `keepMounted`
 * 对「切页签不丢草稿」的行为在两处完全一致。
 */
export function PageTabPanels<K extends string>({
  value,
  idPrefix,
  keepMounted = false,
  keys,
  children,
}: Omit<PageTabsProps<K>, "tabs" | "onChange" | "label" | "variant" | "markedKeys"> & {
  /**
   * 当前存在的页签集合（可选）。页签条在页面顶栏里时，本组件看不到 `tabs`，
   * 需要调用方把自己那份页签集合传进来才能回收已消失的面板；不传 = 不回收（旧行为）。
   */
  keys?: readonly K[];
}) {
  return (
    <div className="page-tabs">
      <PanelStack value={value} idPrefix={idPrefix} keepMounted={keepMounted} keys={keys}>
        {children}
      </PanelStack>
    </div>
  );
}

/** 面板栈：`key` 只按页签 key 稳，绝不绑 value（绑了就等于每次切换都卸载）。 */
function PanelStack<K extends string>({
  value,
  idPrefix,
  keepMounted,
  keys,
  children,
}: {
  value: K;
  idPrefix: string;
  keepMounted: boolean;
  keys?: readonly K[];
  children: (tab: K) => ReactNode;
}) {
  // 已访问过的页签（保活用）。初值放 value，避免首帧出现"一个面板都没有"的空壳。
  const [seen, setSeen] = useState<readonly K[]>(() => (keepMounted ? [value] : []));
  useEffect(() => {
    if (!keepMounted) return;
    setSeen((prev) => (prev.includes(value) ? prev : [...prev, value]));
  }, [keepMounted, value]);

  // 本轮要渲染的面板：关掉保活时只有当前一个，与旧行为逐字一致。
  // 传了 keys 就按它过滤：页签在运行期被移除（卸载插件、删最后一篇文档）时，
  // 它的面板必须一起卸载 —— `seen` 只会增，不过滤就是"幽灵面板"。
  const panelKeys = useMemo(() => {
    const base = keepMounted ? (seen.includes(value) ? seen : [...seen, value]) : [value];
    return keys ? base.filter((k) => keys.includes(k)) : base;
  }, [keepMounted, seen, value, keys]);

  return (
    <>
      {/* 非当前面板用 hidden + 内联 display:none 双保险：`.tab-panel` 自带
          `display:flex`，单靠 hidden 属性会被这条 class 规则盖掉。 */}
      {panelKeys.map((k) => {
        const active = k === value;
        return (
          <div
            key={k}
            className="tab-panel"
            role="tabpanel"
            id={`${idPrefix}-panel-${k}`}
            aria-labelledby={`${idPrefix}-tab-${k}`}
            hidden={!active}
            style={active ? undefined : { display: "none" }}
          >
            {children(k)}
          </div>
        );
      })}
    </>
  );
}

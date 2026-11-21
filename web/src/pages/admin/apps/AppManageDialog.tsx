import { useCallback, useMemo, useState } from "react";

import { Dialog } from "@/components/common/Dialog";
import { PageTabPanels } from "@/components/common/PageTabs";
import { Tabs } from "@/components/common/Tabs";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import { useAdminAppPluginInstallsQuery, useSetAppStatusMutation, useUpdateAppMutation } from "@/lib/api";
import { assertNever } from "@/lib/utils";
import type { AdminAppItem } from "@/types";
import type { PageTab } from "@/lib/pageTabs";
import { PluginDataPanel } from "@/components/plugins/PluginDataPanel";
import { AppEditDialog } from "./AppEditDialog";
import { BrandTab } from "./tabs/BrandTab";
import { NoticesTab } from "./tabs/NoticesTab";
import { OverviewTab } from "./tabs/OverviewTab";
import { PluginsTab } from "./tabs/PluginsTab";
import { ReleasesTab } from "./tabs/ReleasesTab";

/** 运行期插件页签的 key 前缀。生成与解析只在本文件两处，必须成对改。 */
const PLUGIN_TAB_PREFIX = "plugin:";

/** 静态页签的 key 联合（显式写死：从常量反推会与常量注解形成循环引用）。 */
type StaticAppTabKey = "overview" | "brand" | "notices" | "releases" | "plugins";

/** 静态页签定义必须是模块级常量：每次 render 新建数组会让页签条的 useMemo 失效。 */
const APP_TABS: readonly PageTab<StaticAppTabKey>[] = [
  { key: "overview", label: "概览" },
  { key: "brand", label: "品牌" },
  { key: "notices", label: "公告" },
  { key: "releases", label: "版本" },
];

/**
 * 「插件」清单页签（安装/卸载入口），**恒在整条页签条的最右**。
 *
 * 为什么不是普通一项：已装插件各占一个页签（见 `pluginTabs`），它们会随安装数量
 * 增长，把管理入口挤到中间去，用户每装一个插件就得重新找它。所以这里两件事一起做：
 *  1. 数组顺序永远最后（`tabs` 的拼装顺序）；
 *  2. `tab--pin` 让它在还有空位时被 `margin-left:auto` 顶到最右
 *     （内容超宽时 auto 退化为 0，紧贴前一项 —— 不会跑到屏幕外）。
 */
const PLUGINS_TAB: PageTab<StaticAppTabKey> = { key: "plugins", label: "插件", className: "tab--pin" };

/** 运行期页签：一个已安装插件一个，卸载即消失。 */
type PluginTabKey = `plugin:${string}`;
type AppTabKey = StaticAppTabKey | PluginTabKey;

const STATIC_TAB_KEYS: readonly string[] = [...APP_TABS.map((t) => t.key), PLUGINS_TAB.key];

/**
 * 页签与面板共用的 id 前缀。
 *
 * 为什么写成常量而不是两处 `idPrefix="app-manage"`：页签条在顶栏、面板在 body，
 * 是同一个 tablist 的两半，前缀必须逐字一致（否则 `aria-labelledby` 断链，
 * `getByRole("tabpanel", { name })` 全部失配）。而 UI 契约门禁 M18 是按
 * **字面量**计数的 —— 同文件出现两次会被判成"两组页签"。常量避开这个误判。
 */
const TAB_ID = "app-manage";

/** 没有任何页签脏态时的共享空对象：避免每帧新建 `{}` 让下游 memo 全失效。 */
const EMPTY_DIRTY: Partial<Record<StaticAppTabKey, boolean>> = {};

type UiState = {
  /** 这份状态属于哪个应用。换应用 / 关闭后重开都靠它判定"新会话"。 */
  appId: number;
  tab: AppTabKey;
  dirty: Partial<Record<StaticAppTabKey, boolean>>;
};

/**
 * 判别运行期页签。
 *
 * 为什么要在 switch 之前分流：静态六键是闭合联合，`default: assertNever(k)`
 * 能在**编译期**抓到"加了页签忘了写 case"（overview 曾落在 default 上显示错内容）；
 * 运行期 key 无法穷举，所以先在这里判别，插件分支不进 switch。
 */
function isStaticTab(k: AppTabKey): k is StaticAppTabKey {
  return STATIC_TAB_KEYS.includes(k);
}

/** 拼运行期页签 key。模板字面量在运行期是 string，这里做**唯一**一次类型收敛。 */
function pluginTabKey(key: string): AppTabKey {
  return `${PLUGIN_TAB_PREFIX}${key}` as PluginTabKey;
}

/**
 * 应用管理弹窗（顶栏页签：静态四项 + 每个已安装插件一项 + 「插件」清单）。
 *
 * 「在线」页签已并入「概览」（2026-11-20）：两页读同一条 `/stats`，分开只会让
 * 运营在两个页签之间来回核对。维护模式与停用/启用的语义分工写在 OverviewTab。
 *
 * 四条结构约定（改之前先读）：
 *  1. **页签住顶栏**（`Dialog` 的 `headExtra`），不再单独占一行。于是
 *     "页签不随内容滚动"由结构保证（它在滚动容器之外），不再依赖 sticky。
 *     **顶栏没有标题**：页签条从左边距起排，`variant="line"`（下划线）贴底，
 *     2px 选中线正好压在顶栏分隔线上。可访问名走 `Dialog.ariaLabel`。
 *  2. **「插件」清单页签恒在最右**（`PLUGINS_TAB` = 最后一项 + `tab--pin`）：
 *     已装插件的数据页签排在它**左边**，装多少个都不会把它挤到中间。
 *  3. **没有底栏**：`Dialog` 只在传了 `actions`/`footer` 时渲染 `.dl-foot`。
 *     关闭路径是右上角 X / Esc / 点遮罩三条；「查看密钥」在概览页签的动作行里。
 *  4. **页签状态挂在最外层**：顶栏由 `Dialog` 产出，状态只能在它之上；
 *     换应用与关闭都不重挂 `Dialog`（那会丢掉关闭过渡），改用「带 appId 的会话状态」。
 *
 * 尺寸（宽/高）唯一来源是 `components.css` 的 `.dialog.app-manage-dialog`：
 * 不传 `size` 也不写内联样式，避免「size 一档、className 一档」两个真相源。
 * 各页签内容高差极大（概览带图表、公告只有一行），外壳必须定高，
 * 否则切页签时整个弹窗会上下各弹半格。
 */
export function AppManageDialog({
  app,
  outOfFilter = false,
  onClose,
  onOpenKeys,
}: {
  app: AdminAppItem | null;
  /** 该行已不符合当前筛选（列表里查不到了），弹窗仍在展示最后一次快照。 */
  outOfFilter?: boolean;
  onClose: () => void;
  onOpenKeys: (app: AdminAppItem) => void;
}) {
  const appId = app?.id ?? 0;
  const [ui, setUi] = useState<UiState | null>(null);
  const [editing, setEditing] = useState(false);

  // 关闭即清空会话状态（渲染期同步 state，React 官方的 "adjusting state when props
  // change"）。为什么必须清：BrandTab 上报脏态时没有 cleanup，卸载不会回调
  // `onDirtyChange(false)`，不清就会把「未保存」圆点带到下一次打开。
  if (app === null && ui !== null) setUi(null);

  // 换应用 = 新会话：渲染期归一，不写 effect（effect 会先渲染一帧旧页签，
  // 那一帧的 aria-controls 指向不存在的面板）。
  const cur = app && ui?.appId === app.id ? ui : null;
  const tab: AppTabKey = cur?.tab ?? "overview";
  const dirty = cur?.dirty ?? EMPTY_DIRTY;

  /** 所有写点都走它：自动带上"当前应用"的会话基线，避免手写 prev 判断。 */
  const patch = useCallback(
    (next: Partial<Pick<UiState, "tab" | "dirty">>) => {
      setUi((prev) => {
        const base =
          prev && prev.appId === appId ? prev : { appId, tab: "overview" as AppTabKey, dirty: EMPTY_DIRTY };
        return { ...base, ...next };
      });
    },
    [appId],
  );

  // 页签集合与「插件」页签共用同一份清单（同 queryKey ⇒ 一次请求、共享缓存）。
  // 各查一次会出现"装完页签不出现 / 卸载后页签还在"。
  // app 为空时传 0：hook 内部 `enabled: id > 0`，不会打请求。
  const installs = useAdminAppPluginInstallsQuery(appId);
  const pluginTabs = useMemo<PageTab<AppTabKey>[]>(
    () =>
      (installs.data?.items ?? [])
        // 3=已卸载：页签消失（数据仍在，「插件」页签里可重新启用或清除）。
        .filter((it) => it.status !== 3)
        .map((it) => ({ key: pluginTabKey(it.plugin_key), label: it.name })),
    [installs.data],
  );
  /** 顺序不变量：静态五项 → 各插件数据页 → 「插件」清单（恒最右，见 PLUGINS_TAB）。 */
  const tabs = useMemo<readonly PageTab<AppTabKey>[]>(
    () => [...APP_TABS, ...pluginTabs, PLUGINS_TAB],
    [pluginTabs],
  );
  const tabKeys = useMemo(() => tabs.map((t) => t.key), [tabs]);

  // 当前页签可能已被移除（刚卸载）：渲染期推导兜底，不写 effect 纠偏 ——
  // effect 会先渲染一帧 aria-controls 指向不存在的面板。
  const activeTab: AppTabKey = tabs.some((t) => t.key === tab) ? tab : "plugins";

  const markedKeys = useMemo(
    () => (Object.keys(dirty) as StaticAppTabKey[]).filter((k) => dirty[k]),
    [dirty],
  );
  // 脏点合并放在这里：`PageTabPanels` 只知道面板，不认识 markedKeys。
  // 用 Set 而不是 includes：页签 key 是 `AppTabKey`（含运行期前缀），
  // 而脏态只可能落在静态键上，两者取交集要按 string 比。
  const marked = useMemo(() => new Set<string>(markedKeys), [markedKeys]);
  const items = useMemo(
    () => (marked.size ? tabs.map((t) => (marked.has(t.key) ? { ...t, mark: true } : t)) : tabs),
    [tabs, marked],
  );

  const setTabDirty = useCallback(
    (k: StaticAppTabKey, d: boolean) => {
      setUi((prev) => {
        const base =
          prev && prev.appId === appId ? prev : { appId, tab: "overview" as AppTabKey, dirty: EMPTY_DIRTY };
        if (base.dirty[k] === d) return base; // 同值不产生新对象（原实现口径）
        return { ...base, dirty: { ...base.dirty, [k]: d } };
      });
    },
    [appId],
  );

  const setStatus = useSetAppStatusMutation();
  const update = useUpdateAppMutation();

  const toggleStatus = () => {
    if (!app) return;
    const next = app.status === 1 ? 2 : 1;
    setStatus.mutate(
      { id: app.id, status: next },
      {
        onSuccess: () => toastOk(next === 1 ? "应用已启用" : "应用已停用"),
        onError: (err) => toastApiError(err, "操作失败"),
      },
    );
  };

  const renderPanel = (k: AppTabKey) => {
    if (!app) return null;
    // 运行期页签：key 里带插件标识，数据面板按 appId + 插件 key 取数。
    if (!isStaticTab(k)) {
      return <PluginDataPanel appId={app.id} pluginKey={k.slice(PLUGIN_TAB_PREFIX.length)} />;
    }
    switch (k) {
      // overview 必须显式列出：此前它落在 default 上，于是"未知 key"会被
      // 静默渲染成概览 —— 加一个页签忘了写 case 也不会报错，只会显示错内容。
      // 现在 default 用 assertNever：漏 case 直接编译失败。
      case "overview":
        return (
          <OverviewTab
            app={app}
            onToggleStatus={toggleStatus}
            onEdit={() => setEditing(true)}
            onOpenKeys={() => onOpenKeys(app)}
            statusBusy={setStatus.isPending}
            onDirtyChange={(d) => setTabDirty("overview", d)}
          />
        );
      case "brand":
        return <BrandTab app={app} onDirtyChange={(d) => setTabDirty("brand", d)} />;
      case "notices":
        return <NoticesTab app={app} />;
      case "releases":
        return <ReleasesTab app={app} />;
      case "plugins":
        return <PluginsTab app={app} onOpenPlugin={(key) => patch({ tab: pluginTabKey(key) })} />;
      default:
        return assertNever(k);
    }
  };

  return (
    <Dialog
      open={app !== null}
      onClose={onClose}
      className="app-manage-dialog"
      closeButton
      /* 顶栏不显示标题（产品决定：页签条自己就是身份）。名字改由 ariaLabel 提供 ——
         没有它，`getByRole("dialog", { name: /应用管理/ })` 与读屏都会退化成「对话框」。 */
      ariaLabel={app ? `${app.display_name || app.name} · 应用管理` : "应用管理"}
      headExtra={
        app ? (
          <Tabs
            items={items}
            value={activeTab}
            onChange={(k) => patch({ tab: k as AppTabKey })}
            variant="line"
            idPrefix={TAB_ID}
            label="应用管理"
            panelId={`${TAB_ID}-panel-${activeTab}`}
          />
        ) : null
      }
    >
      {app ? (
        <>
          {outOfFilter ? (
            <div className="alert-banner info">
              <span>该应用已不符合当前筛选条件，列表不再显示它；此处的数据是最后一次读取的结果。</span>
            </div>
          ) : null}
          {/* key 绑 app.id：换应用时面板栈的「已访问集合」必须一起归零，
              否则会预挂载上一个应用看过、这个应用还没打开过的页签。 */}
          <PageTabPanels
            key={app.id}
            value={activeTab}
            idPrefix={TAB_ID}
            keys={tabKeys}
            // 保活已访问过的页签：品牌九个字段、维护文案都是长表单，
            // 切去看一眼公告再回来内容就没了，用户只会当成 bug 重填一遍；
            // 插件面板同理（筛选/页码/勾选/未提交的表单）。
            // 页签在运行期消失（卸载插件）时，按 keys 把对应面板一起卸载。
            keepMounted
          >
            {renderPanel}
          </PageTabPanels>
          {/* 编辑弹窗挂在页签之外：切页签不会把正在编辑的表单一起卸载。 */}
          <AppEditDialog
            app={app}
            open={editing}
            onClose={() => setEditing(false)}
            submitting={update.isPending}
            onSubmit={(payload) =>
              update.mutate(
                { id: app.id, payload },
                {
                  onSuccess: () => {
                    setEditing(false);
                    toastOk("应用信息已保存");
                  },
                  onError: (err) => toastApiError(err, "保存失败"),
                },
              )
            }
          />
        </>
      ) : null}
    </Dialog>
  );
}

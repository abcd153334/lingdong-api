import { useCallback, useMemo, useState } from "react";

import { Dialog } from "@/components/common/Dialog";
import { PageTabs } from "@/components/common/PageTabs";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import { useSetAppStatusMutation, useUpdateAppMutation } from "@/lib/api";
import type { AdminAppItem } from "@/types";
import type { PageTab } from "@/lib/pageTabs";
import { AppEditDialog } from "./AppEditDialog";
import { BrandTab } from "./tabs/BrandTab";
import { NoticesTab } from "./tabs/NoticesTab";
import { OverviewTab } from "./tabs/OverviewTab";
import { PluginsTab } from "./tabs/PluginsTab";
import { ReleasesTab } from "./tabs/ReleasesTab";
import { RuntimeTab } from "./tabs/RuntimeTab";

/**
 * 页签定义必须是模块级常量：每次 render 新建数组会让 PageTabs 内部的
 * useMemo 失效，切页签时面板会被无谓重建。
 */
const APP_TABS: readonly PageTab<
  "overview" | "brand" | "notices" | "releases" | "runtime" | "plugins"
>[] = [
  { key: "overview", label: "概览" },
  { key: "brand", label: "品牌" },
  { key: "notices", label: "公告" },
  { key: "releases", label: "版本" },
  { key: "runtime", label: "在线" },
  // 插件是**静态**页签：插件清单才是运行期数据，页面本身在 /admin/apps/:id/p/:key。
  // 动态页签会同时踩三处静默失败（switch 回落、seen 不回收、idPrefix 必须字面量）。
  { key: "plugins", label: "插件" },
];
type AppTabKey = (typeof APP_TABS)[number]["key"];

/**
 * 应用管理弹窗（六页签）。
 *
 * 为什么用弹窗而不是独立页面：
 *   1. 应用是"列表里的一行"，配置它属于行内动作，跳页会丢失列表的筛选与页码；
 *   2. 新增页面要同时改 router 与 nav，而 nav 的 7 分区/18 项是全站断言锚点。
 *
 * 尺寸（宽/高）唯一来源是 `components.css` 的 `.dialog.app-manage-dialog`：
 * 不传 `size` 也不写内联样式，避免「size 一档、className 一档」两个真相源。
 * 各页签内容高差极大（概览带图表、公告只有一行），外壳必须定高，
 * 否则切页签时整个弹窗会上下各弹半格。
 *
 * `closeButton` 必须开：弹窗里没有「取消」按钮，右上角是唯一的显式关闭入口。
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
  return (
    <Dialog
      open={app !== null}
      onClose={onClose}
      className="app-manage-dialog"
      closeButton
      title={app ? `${app.display_name || app.name} · 应用管理` : ""}
      actions={[
        { label: "关闭", onClick: onClose },
        {
          label: "查看密钥",
          variant: "primary",
          onClick: () => {
            if (app) onOpenKeys(app);
          },
        },
      ]}
    >
      {app ? (
        <>
          {outOfFilter ? (
            <div className="alert-banner info">
              <span>该应用已不符合当前筛选条件，列表不再显示它；此处的数据是最后一次读取的结果。</span>
            </div>
          ) : null}
          <ManageBody key={app.id} app={app} />
        </>
      ) : null}
    </Dialog>
  );
}

/** 弹窗主体。按 app.id 挂 key：换一个应用时页签状态与各页签表单一起重置。 */
function ManageBody({ app }: { app: AdminAppItem }) {
  const [tab, setTab] = useState<AppTabKey>("overview");
  const [editing, setEditing] = useState(false);
  // 哪些页签有未保存的修改（页签上的圆点）。挂在父级而不是各页签内部：
  // 圆点要在**别的**页签上去看，子组件自己的 state 传不出去。
  const [dirty, setDirty] = useState<Partial<Record<AppTabKey, boolean>>>({});
  const setStatus = useSetAppStatusMutation();
  const update = useUpdateAppMutation();

  const setTabDirty = useCallback((k: AppTabKey, d: boolean) => {
    setDirty((prev) => (prev[k] === d ? prev : { ...prev, [k]: d }));
  }, []);
  const markedKeys = useMemo(
    () => (Object.keys(dirty) as AppTabKey[]).filter((k) => dirty[k]),
    [dirty],
  );

  const toggleStatus = () => {
    const next = app.status === 1 ? 2 : 1;
    setStatus.mutate(
      { id: app.id, status: next },
      {
        onSuccess: () => toastOk(next === 1 ? "应用已启用" : "应用已停用"),
        onError: (err) => toastApiError(err, "操作失败"),
      },
    );
  };

  return (
    <>
      <PageTabs
        tabs={APP_TABS}
        value={tab}
        onChange={setTab}
        idPrefix="app-manage"
        label="应用管理"
        variant="line"
        markedKeys={markedKeys}
        // 保活已访问过的页签：品牌九个字段、维护文案都是长表单，
        // 切去看一眼公告再回来内容就没了，用户只会当成 bug 重填一遍。
        keepMounted
      >
        {(k) => {
          switch (k) {
            // overview 必须显式列出：此前它落在 default 上，于是"未知 key"会被
            // 静默渲染成概览 —— 加一个页签忘了写 case 也不会报错，只会显示错内容。
            case "overview":
              return (
                <OverviewTab
                  app={app}
                  onToggleStatus={toggleStatus}
                  onEdit={() => setEditing(true)}
                  statusBusy={setStatus.isPending}
                />
              );
            case "brand":
              return <BrandTab app={app} onDirtyChange={(d) => setTabDirty("brand", d)} />;
            case "notices":
              return <NoticesTab app={app} />;
            case "releases":
              return <ReleasesTab app={app} />;
            case "runtime":
              return <RuntimeTab app={app} onDirtyChange={(d) => setTabDirty("runtime", d)} />;
            case "plugins":
              return <PluginsTab app={app} />;
            default:
              return null;
          }
        }}
      </PageTabs>
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
  );
}

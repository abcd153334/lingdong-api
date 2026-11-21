import { useEffect, useMemo, useState } from "react";

import { EmptyState } from "@/components/common/EmptyState";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { PageHead } from "@/components/common/PageHead";
import { Pagination } from "@/components/common/Pagination";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import { useAdminAppsQuery, useCreateAppMutation, type AdminAppsParams } from "@/lib/api";
import { DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import type { AdminAppItem } from "@/types";

import { AppCard } from "./apps/AppCard";
import { AppCreateDialog } from "./apps/AppCreateDialog";
import { AppKeysDrawer } from "./apps/AppKeysDrawer";
import { AppManageDialog } from "./apps/AppManageDialog";

/**
 * 应用管理：卡片列表。
 *
 * 这一页存在的理由：对外 API 的归属锚点是应用（`api_keys.app_id` →
 * `usages.key_id` → 消费归因）。卡片回答"这个应用现在是什么状态"，
 * 弹窗回答"怎么配它" —— 列表只放概览信息，配置一律进弹窗。
 *
 * 没有删除按钮是**刻意**的：应用被密钥与订单引用，删掉会留下 join 不到名字的
 * 孤儿（账单里"这笔充值属于哪个应用"直接空白）。下线用停用；要立即断流用
 * 维护模式。两步操作是刻意的，一次点击不该同时影响"新接入"与"正在跑的流量"。
 *
 * 弹窗只持有 **id**，当前行由列表数据派生。持对象引用的后果是：停用成功后列表
 * 重新拉取会生成新对象，弹窗仍指旧对象 —— 界面显示"当前：启用"，再点一次又发
 * 一遍 PATCH。派生失败的场景（筛选条件不再匹配该行）用最后一次快照兜住，
 * 否则"停用应用"会让弹窗自己关掉，看起来像操作被拒绝。
 */
export function AdminAppsPage() {
  const [page, setPage] = useState(1);
  const [values, setValues] = useState<Record<string, string>>({});
  const [creating, setCreating] = useState(false);
  const [managingId, setManagingId] = useState<number | null>(null);
  const [keysId, setKeysId] = useState<number | null>(null);
  // 最后一次已知的列表行：只在"该行已不符合当前筛选"时兜底，不是第二份真相源。
  const [frozenApp, setFrozenApp] = useState<AdminAppItem | null>(null);
  const [frozenKeysApp, setFrozenKeysApp] = useState<AdminAppItem | null>(null);

  const params: AdminAppsParams = useMemo(
    () => ({
      page,
      page_size: DEFAULT_PAGE_SIZE,
      keyword: values.keyword || undefined,
      status: values.status ? Number(values.status) : undefined,
      maintenance: values.maintenance === "1" ? 1 : values.maintenance === "0" ? 0 : undefined,
      sort: (values.sort as AdminAppsParams["sort"]) || undefined,
    }),
    [page, values],
  );

  const apps = useAdminAppsQuery(params);
  const createApp = useCreateAppMutation();

  const items = apps.data?.items ?? [];
  const total = apps.data?.total ?? 0;

  const liveApp = managingId === null ? null : (items.find((i) => i.id === managingId) ?? null);
  const liveKeysApp = keysId === null ? null : (items.find((i) => i.id === keysId) ?? null);
  useEffect(() => {
    if (liveApp) setFrozenApp(liveApp);
  }, [liveApp]);
  useEffect(() => {
    if (liveKeysApp) setFrozenKeysApp(liveKeysApp);
  }, [liveKeysApp]);
  const managing = liveApp ?? frozenApp;
  const keysApp = liveKeysApp ?? frozenKeysApp;

  // 行被筛掉时给一句解释，而不是让弹窗无声消失。
  const manageOutOfFilter = managingId !== null && liveApp === null && frozenApp !== null;

  const fields: FilterField[] = [
    { id: "keyword", t: "search", label: "关键词", placeholder: "应用标识或名称" },
    {
      id: "status",
      t: "select",
      label: "状态",
      allLabel: "全部状态",
      items: [
        ["1", "启用"],
        ["2", "停用"],
      ],
    },
    {
      id: "maintenance",
      t: "select",
      label: "维护",
      allLabel: "全部维护状态",
      items: [
        ["1", "维护中"],
        ["0", "正常"],
      ],
    },
    {
      id: "sort",
      t: "select",
      label: "排序",
      allLabel: "按 ID",
      items: [
        ["created_at", "按创建时间"],
        ["name", "按名称"],
      ],
    },
    { id: "create", t: "btn", label: "新建应用" },
  ];

  return (
    <>
      <PageHead
        title="应用管理"
        desc="对外接口的归属主体：密钥、订单与消费按应用归因"
        actions={[{ type: "btn", label: "新建应用", variant: "primary", onClick: () => setCreating(true) }]}
      />

      <FilterBar
        fields={fields}
        value={values}
        onChange={(id, value) => {
          setPage(1);
          setValues((prev) => ({ ...prev, [id]: value }));
        }}
        onReset={() => {
          setValues({});
          setPage(1);
        }}
        onAction={(id) => {
          if (id === "create") setCreating(true);
        }}
      />

      <div className="count-line">
        共 <b className="mono">{total}</b> 个应用
      </div>

      {apps.isPending ? (
        // 加载中渲染骨架而不是「0 个应用」：后者会让运营以为应用丢了。
        <div className="app-grid" aria-busy="true">
          {[0, 1, 2].map((i) => (
            <div className="app-card app-card--skeleton" key={i}>
              <div className="sk-line w40" />
              <div className="sk-line w70" />
              <div className="sk-line w55" />
            </div>
          ))}
        </div>
      ) : apps.isError ? (
        <EmptyState
          variant="error"
          title="应用列表加载失败"
          desc="请检查网络后重试"
          action={
            <button type="button" className="btn" onClick={() => void apps.refetch()}>
              重试
            </button>
          }
        />
      ) : items.length === 0 ? (
        <EmptyState
          title={Object.keys(values).length ? "没有符合条件的应用" : "还没有应用"}
          desc={
            Object.keys(values).length
              ? "试试清空筛选条件"
              : "新建应用后，接入方才能用应用标识签发托管密钥"
          }
        />
      ) : (
        <>
          <div className="app-grid">
            {items.map((a) => (
              <AppCard
                key={a.id}
                app={a}
                onManage={() => {
                  setFrozenApp(a);
                  setManagingId(a.id);
                }}
                onKeys={() => {
                  setFrozenKeysApp(a);
                  setKeysId(a.id);
                }}
              />
            ))}
          </div>
          <Pagination
            total={total}
            page={page}
            pageSize={DEFAULT_PAGE_SIZE}
            onPageChange={setPage}
          />
        </>
      )}

      {/* key 随 open 变化：Dialog 关闭不卸载子树，靠重建来清空表单。 */}
      <AppCreateDialog
        key={creating ? "create-open" : "create-closed"}
        open={creating}
        onClose={() => setCreating(false)}
        submitting={createApp.isPending}
        onSubmit={(payload) => {
          createApp.mutate(payload, {
            onSuccess: (res) => {
              setCreating(false);
              // 新建行按 id DESC 落在第 1 页；留在第 N 页会让运营以为没建成。
              setPage(1);
              toastOk(`已创建 ${res.slug}`);
            },
            onError: (err) => toastApiError(err, "创建失败"),
          });
        }}
      />

      <AppManageDialog
        app={managing}
        outOfFilter={manageOutOfFilter}
        onClose={() => {
          setManagingId(null);
          setFrozenApp(null);
        }}
        onOpenKeys={(a) => {
          setFrozenKeysApp(a);
          setKeysId(a.id);
        }}
      />

      <AppKeysDrawer
        app={keysApp}
        onClose={() => {
          setKeysId(null);
          setFrozenKeysApp(null);
        }}
      />
    </>
  );
}

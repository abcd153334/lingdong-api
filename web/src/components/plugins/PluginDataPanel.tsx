import { useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { SortingState } from "@tanstack/react-table";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { EmptyState } from "@/components/common/EmptyState";
import { ErrorState, LoadingState } from "@/components/common/QueryStates";
import { Input } from "@/components/common/Input";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Select } from "@/components/common/Select";
import { SwitchRow } from "@/components/common/SwitchRow";
import { copyToast } from "@/lib/clipboard";
import { toastApiError } from "@/lib/apiToast";
import { toastOk } from "@/components/common/Toaster";
import {
  exportAppPluginRecords,
  queryKeys,
  useAdminAppPluginDetailQuery,
  useAdminAppPluginRecordsQuery,
  useBatchDeleteAppPluginRecordsMutation,
  useCreateAppPluginRecordMutation,
  useDeleteAppPluginRecordMutation,
  usePatchAppPluginMutation,
  useUpdateAppPluginRecordMutation,
} from "@/lib/api";
import type { PageQuery } from "@/lib/apiClient";
import type { AdminAppPluginRecord } from "@/types";
import { fieldOf, normalizeEntities } from "@/lib/pluginTypes";
import { RecordDialog } from "@/components/plugins/RecordDialog";
import { RecordTable } from "@/components/plugins/RecordTable";
import { PluginFilters, type FilterValues } from "@/components/plugins/PluginFilters";

const PAGE_SIZES = ["20", "50", "100"];

/**
 * 插件的**数据面板**（应用管理弹窗里「plugin:<key>」页签的内容）。
 *
 * 它以前是一个独立页面（`/admin/apps/$id/p/$key`），迁进弹窗后有三条硬约束：
 *  1. **不渲染 `PageHead`**：页面级 h1 由 `Apps.tsx` 提供，弹窗里再加一个就是
 *     两个 h1 + 重复的 `id="pageTitle"`（「每页唯一 h1」「DOM id 唯一」两条门禁）。
 *  2. **不自带滚动**：唯一滚动体是宿主弹窗的 `.page-tabs > .tab-panel`（直系子代），
 *     这里加 `overflow`/`height:100%` 会形成嵌套滚动，弹窗高度也会跳。
 *  3. **不读 URL**：页签状态不写地址栏（旧路由已删除），实体/筛选/排序都是本地状态。
 *
 * 页面只认后端下发的描述（`plugin.entities`），不解析 manifest。
 */
export function PluginDataPanel({ appId, pluginKey }: { appId: number; pluginKey: string }) {
  const detail = useAdminAppPluginDetailQuery(appId, pluginKey);

  // 描述可能来自一台**还没重启的旧后端**：缺 sort/filters/form 时归一成"能力更少"，
  // 不能直接读 `list.sort[0]`（那会 undefined[0] 崩掉整个弹窗）。
  const entities = useMemo(() => normalizeEntities(detail.data?.plugin.entities), [detail.data]);
  const qc = useQueryClient();

  const [entityKey, setEntityKey] = useState("");
  const [keyword, setKeyword] = useState("");
  const [filters, setFilters] = useState<FilterValues>({});
  const [sortState, setSortState] = useState<SortingState | null>(null);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [creating, setCreating] = useState(false);
  /**
   * 「保存并继续」的清空手段：换 key 重挂 RecordDialog。
   *
   * 不在这里手动 reset 表单值：RecordDialog 的初值来自 toFormValues，重挂即
   * 回到"默认值 + 空"，那是**唯一**一条初始化路径（两套清空逻辑必然漂）。
   */
  const [newSeq, setNewSeq] = useState(0);
  const [editing, setEditing] = useState<AdminAppPluginRecord | null>(null);
  const [viewing, setViewing] = useState<AdminAppPluginRecord | null>(null);
  const [problems, setProblems] = useState<{ path: string; message: string }[]>([]);

  // 实体选择优先取本地选择，非法值回落到第一个（manifest 升级可能删掉当前实体）。
  const activeEntity = entities.find((e) => e.key === entityKey) ?? entities[0];

  /**
   * 排序：没点过表头时用 manifest 的首项（后端也按它排）。
   * 前端必须把同一个默认值喂给表头，否则箭头指向与实际顺序不一致。
   */
  const sort = useMemo<SortingState>(() => {
    if (sortState) return sortState;
    const first = activeEntity?.list.sort[0];
    return first ? [{ id: first.key, desc: first.order === "desc" }] : [];
  }, [sortState, activeEntity]);

  const install = detail.data?.install;
  const keywordHint = activeEntity?.unique
    ? `按${fieldOf(activeEntity, activeEntity.unique)?.label ?? "唯一值"}或编号搜索`
    : "按编号搜索";
  const canRead = !!install && install.status !== 3;
  const canWrite = !!install && install.status === 1 && !detail.data?.catalog_disabled;
  const canBatch = canWrite && !!activeEntity?.list.actions.includes("batch_delete");

  const query = useMemo(() => {
    const q: Record<string, unknown> = {
      entity: activeEntity?.key ?? "",
      keyword,
      page,
      page_size: pageSize,
    };
    if (sort.length) q.sort = `${sort[0].id}:${sort[0].desc ? "desc" : "asc"}`;
    for (const [k, v] of Object.entries(filters)) {
      if (v !== "") q[`f_${k}`] = v;
    }
    return q as PageQuery;
  }, [activeEntity, keyword, page, pageSize, sort, filters]);

  const records = useAdminAppPluginRecordsQuery(appId, pluginKey, query, canRead && !!activeEntity);

  const create = useCreateAppPluginRecordMutation();
  const patchPublic = usePatchAppPluginMutation();
  const update = useUpdateAppPluginRecordMutation();
  const remove = useDeleteAppPluginRecordMutation();
  const batchRemove = useBatchDeleteAppPluginRecordsMutation();

  /** 任何影响结果集的变化：回到第一页并清空勾选（跨页勾选在"删除所选"里最容易被误解）。 */
  const resetTo = (fn: () => void) => {
    fn();
    setPage(1);
    setSelected(new Set());
  };

  const pickEntity = (key: string) => {
    resetTo(() => {
      setKeyword("");
      setFilters({});
      setSortState(null);
      setEntityKey(key);
    });
  };

  const itemKeys = (records.data?.items ?? []).map((r) => r.record_key);
  const allSelected = itemKeys.length > 0 && itemKeys.every((k) => selected.has(k));
  const someSelected = itemKeys.some((k) => selected.has(k));

  const failWith = (err: unknown, fallback: string) => {
    const data = (err as { data?: { problems?: { path: string; message: string }[] } })?.data;
    setProblems(data?.problems ?? []);
    toastApiError(err, fallback);
  };

  const data = detail.data;
  // 标题行在加载/失败/未安装三态都渲染：弹窗里没有别的元素能表示"这是哪个插件"。
  const heading = (
    <div className="plugin-sec__head">
      <span className="blk-title">{data ? data.plugin.name : "插件"}</span>
      <span className="muted small">
        {data
          ? `插件 ${data.install.plugin_version || data.plugin.version} · 共 ${data.record_count} 条`
          : ""}
      </span>
    </div>
  );

  if (detail.isPending) {
    return (
      <>
        {heading}
        <LoadingState title="正在加载插件…" />
      </>
    );
  }
  if (detail.isError) {
    return (
      <>
        {heading}
        <ErrorState
          error={detail.error}
          onRetry={() => {
            void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginDetail(appId, pluginKey) });
          }}
        />
      </>
    );
  }
  if (!data) {
    return (
      <>
        {heading}
        <EmptyState title="插件未安装" desc="回到「插件」页签安装后再打开。" />
      </>
    );
  }

  const entityView = activeEntity;
  const canCreate = canWrite && !!entityView?.list.actions.includes("create");
  const canExport = !!entityView?.list.actions.includes("export");

  const publicURL =
    install?.public_enabled && entityView && entityView.public.length > 0
      ? `${window.location.origin}/api/apps/${data.app.slug}/p/${pluginKey}/${entityView.key}`
      : "";
  const canTogglePublic = !!install && install.status === 1 && !data.catalog_disabled;

  const doBatchDelete = () => {
    if (!entityView || selected.size === 0) return;
    void (async () => {
      const n = selected.size;
      const ok = await confirmDialog({
        title: `删除所选的 ${n} 条${entityView.name}？`,
        body: "删除后不可恢复。不存在的记录会被跳过，不会因此报错。",
        okText: "删除",
        okVariant: "danger",
      });
      if (!ok) return;
      batchRemove.mutate(
        { id: appId, key: pluginKey, entity: entityView.key, recordKeys: [...selected] },
        {
          onSuccess: (res) => {
            toastOk(`已删除 ${res.deleted} 条`);
            setSelected(new Set());
            // 把本页删空时回退一页：否则用户停在"没有数据的第 N 页"以为删错了。
            if (page > 1 && (records.data?.items.length ?? 0) === res.deleted) setPage(page - 1);
          },
          onError: (err) => toastApiError(err, "删除失败"),
        },
      );
    })();
  };

  return (
    <>
      {heading}

      {data.drift ? (
        <div className="alert-banner info">
          插件已升级到 {data.drift.current}，本应用上次安装的是 {data.drift.installed}；已有数据按旧版写入，仍可正常查看。
        </div>
      ) : null}
      {data.catalog_disabled ? (
        <div className="alert-banner info">该插件已下架，暂时不能新增或修改；已有数据仍可查看与导出。</div>
      ) : null}
      {install?.status === 2 ? (
        <div className="alert-banner info">该插件在本应用内已停用，写入已关闭；可在「插件」页签里启用。</div>
      ) : null}
      {install?.status === 3 ? (
        <div className="alert-banner info">
          该插件已卸载，数据仍保留（{data.record_count} 条）。重新启用后即可继续使用。
        </div>
      ) : null}
      {(data.config_missing ?? []).length > 0 ? (
        <div className="alert-banner info">
          有 {(data.config_missing ?? []).length} 项必填配置还没有填写（插件定义里新增的），补齐后行为才与插件说明一致；
          在「插件」页签的「配置」里补。
        </div>
      ) : null}

      <div className="plugin-public">
        <SwitchRow
          className="switch-row--inline"
          label="对外只读"
          ariaLabel="允许终端免凭证读取本插件数据"
          checked={!!install?.public_enabled}
          disabled={!canTogglePublic || patchPublic.isPending}
          onChange={(v) =>
            patchPublic.mutate(
              { id: appId, key: pluginKey, publicEnabled: v, expectedVersion: install?.lock_version ?? 0 },
              {
                onSuccess: () => toastOk(v ? "已开放对外只读" : "已关闭对外只读"),
                onError: (err) => toastApiError(err, "操作失败"),
              },
            )
          }
        />
        {publicURL ? (
          <>
            <code className="plugin-public__url">{publicURL}</code>
            <button type="button" className="btn btn-sm btn-text" onClick={() => void copyToast(publicURL)}>
              复制
            </button>
          </>
        ) : (
          <span className="muted small">
            {entityView && entityView.public.length > 0
              ? "开启后终端可免登录读取白名单字段"
              : "这个数据对象没有声明可公开字段"}
          </span>
        )}
      </div>

      <div className="plugin-toolbar">
        {entities.length > 1 ? (
          <SegmentedChips
            items={entities.map((e) => ({ key: e.key, label: e.name }))}
            value={entityView?.key}
            onChange={pickEntity}
          />
        ) : null}
        <Input
          className="plugin-search"
          type="search"
          value={keyword}
          placeholder={keywordHint}
          aria-label={keywordHint}
          disabled={!canRead}
          onChange={(e) => resetTo(() => setKeyword(e.target.value))}
        />
        <button
          type="button"
          className="btn btn-primary"
          disabled={!canCreate}
          onClick={() => {
            setProblems([]);
            setViewing(null);
            setEditing(null);
            setCreating(true);
          }}
        >
          新建
        </button>
        <button
          type="button"
          className="btn"
          disabled={!canExport || !canRead}
          onClick={() => {
            if (!entityView) return;
            const params: PageQuery & { entity: string } = { entity: entityView.key, keyword };
            for (const [k, v] of Object.entries(filters)) {
              if (v !== "") params[`f_${k}`] = v;
            }
            void exportAppPluginRecords(appId, pluginKey, params).catch((err) =>
              toastApiError(err, "导出失败"),
            );
          }}
        >
          导出 CSV
        </button>
        {/* 每页条数放工具栏而不是分页条：分页条是全站共用组件，
            在里面加 select 会碰 .pager button::after 的 44px 命中区。 */}
        <div className="plugin-pagesize">
          <Select
            aria-label="每页条数"
            value={String(pageSize)}
            disabled={!canRead}
            onChange={(e) => resetTo(() => setPageSize(Number(e.target.value)))}
            items={PAGE_SIZES}
          />
        </div>
        {someSelected ? (
          <button
            type="button"
            className="btn btn-sm btn-text plugin-batch"
            disabled={batchRemove.isPending}
            onClick={doBatchDelete}
          >
            {batchRemove.isPending ? "删除中…" : `删除所选（${selected.size}）`}
          </button>
        ) : null}
      </div>

      {entityView && entityView.list.filters.length > 0 ? (
        <PluginFilters
          entity={entityView}
          values={filters}
          disabled={!canRead}
          onChange={(k, v) => resetTo(() => setFilters((prev) => ({ ...prev, [k]: v })))}
        />
      ) : null}

      {!canRead ? (
        <EmptyState title="暂时看不到记录" desc="插件已卸载或已停用，处理后再回来。" />
      ) : !entityView ? (
        <EmptyState title="这个插件没有数据对象" desc="插件定义里 entities 为空，请重新导入。" />
      ) : (
        <RecordTable
          entity={entityView}
          items={records.data?.items ?? []}
          total={records.data?.total ?? 0}
          page={page}
          pageSize={pageSize}
          loading={records.isPending}
          error={
            records.isError ? (
              <ErrorState error={records.error} onRetry={() => void records.refetch()} />
            ) : undefined
          }
          onPageChange={(p) => {
            setPage(p);
            setSelected(new Set());
          }}
          canWrite={canWrite}
          sort={sort}
          onSortChange={(next) => resetTo(() => setSortState(next))}
          onView={(row) => {
            setProblems([]);
            setCreating(false);
            setEditing(null);
            setViewing(row);
          }}
          selection={
            canBatch
              ? {
                  selected,
                  allSelected,
                  someSelected,
                  toggle: (k) =>
                    setSelected((prev) => {
                      const next = new Set(prev);
                      if (next.has(k)) next.delete(k);
                      else next.add(k);
                      return next;
                    }),
                  toggleAll: () => setSelected(allSelected ? new Set() : new Set(itemKeys)),
                }
              : undefined
          }
          onEdit={(row) => {
            setProblems([]);
            setViewing(null);
            setEditing(row);
          }}
          onDelete={(row) => {
            void (async () => {
              const ok = await confirmDialog({
                title: `删除这条${entityView.name}？`,
                body: `编号 ${row.record_key} 删除后不可恢复。`,
                okText: "删除",
                okVariant: "danger",
              });
              if (!ok) return;
              remove.mutate(
                { id: appId, key: pluginKey, entity: entityView.key, recordKey: row.record_key },
                { onSuccess: () => toastOk("已删除"), onError: (err) => toastApiError(err, "删除失败") },
              );
            })();
          }}
        />
      )}

      {/* key 随目标记录变化：Dialog 关闭不卸载子树，只有重挂才能清空表单。
          idPrefix 由宿主决定：多个插件面板同时保活时，固定字面量会让字段 id 重复。 */}
      <RecordDialog
        key={editing?.record_key ?? viewing?.record_key ?? `new-${newSeq}`}
        idPrefix={`plugin-record-${pluginKey}`}
        entity={
          entityView ?? {
            key: "",
            name: "",
            unique: "",
            fields: [],
            list: { columns: [], actions: [], sort: [], filters: [] },
            form: { sections: [] },
            public: [],
          }
        }
        open={creating || editing !== null || viewing !== null}
        record={editing ?? viewing}
        readOnly={viewing !== null}
        submitting={create.isPending || update.isPending}
        problems={problems}
        onClose={() => {
          setCreating(false);
          setEditing(null);
          setViewing(null);
          setProblems([]);
        }}
        onSubmit={(payload, opts) => {
          if (!entityView) return;
          setProblems([]);
          if (editing) {
            update.mutate(
              {
                id: appId,
                key: pluginKey,
                entity: entityView.key,
                recordKey: editing.record_key,
                data: payload,
                version: editing.version,
              },
              {
                onSuccess: () => {
                  setEditing(null);
                  toastOk("已保存");
                },
                onError: (err) => failWith(err, "保存失败"),
              },
            );
            return;
          }
          create.mutate(
            { id: appId, key: pluginKey, entity: entityView.key, data: payload },
            {
              onSuccess: () => {
                if (opts?.keepOpen) {
                  // 保持打开 + 换 key 重挂 ⇒ 表单回到默认值，接着填下一条。
                  setNewSeq((n) => n + 1);
                  toastOk("已新建，可继续填写下一条");
                  return;
                }
                setCreating(false);
                toastOk("已新建");
              },
              onError: (err) => failWith(err, "新建失败"),
            },
          );
        }}
      />
    </>
  );
}

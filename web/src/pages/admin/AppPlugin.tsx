import { useMemo, useState } from "react";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { EmptyState } from "@/components/common/EmptyState";
import { ErrorState, LoadingState } from "@/components/common/QueryStates";
import { Input } from "@/components/common/Input";
import { PageHead } from "@/components/common/PageHead";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { SwitchRow } from "@/components/common/SwitchRow";
import { copyToast } from "@/lib/clipboard";
import { toastApiError } from "@/lib/apiToast";
import { toastOk } from "@/components/common/Toaster";
import {
  exportAppPluginRecords,
  queryKeys,
  useAdminAppPluginDetailQuery,
  useAdminAppPluginRecordsQuery,
  useCreateAppPluginRecordMutation,
  useDeleteAppPluginRecordMutation,
  usePatchAppPluginMutation,
  useUpdateAppPluginRecordMutation,
} from "@/lib/api";
import type { AdminAppPluginRecord } from "@/types";
import { fieldOf } from "@/lib/pluginTypes";
import { RecordDialog } from "@/components/plugins/RecordDialog";
import { RecordTable } from "@/components/plugins/RecordTable";

const PAGE_SIZE = 20;

/**
 * 插件的泛化页面：`/admin/apps/$id/p/$plugin`。
 *
 * 为什么是独立路由而不是弹窗里的第七个页签：
 *  1. 页签是模块级常量 + `switch`，插件是运行期才知道的，塞进去只能靠动态页签，
 *     而动态页签会撞 `switch default 静默回落`、`PanelStack.seen 只增不减`、
 *     `idPrefix 必须字面量` 三处；
 *  2. 记录列表需要自己的分页、搜索与 URL（深链到某个插件页要能直接打开）。
 *
 * 页面只认后端下发的描述（`plugin.entities`），不解析 manifest。
 */
export function AdminAppPluginPage() {
  const params = useParams({ strict: false }) as { id?: string; plugin?: string };
  const appId = Number(params.id ?? 0);
  const pluginKey = params.plugin ?? "";
  const detail = useAdminAppPluginDetailQuery(appId, pluginKey);

  const entities = useMemo(() => detail.data?.plugin.entities ?? [], [detail.data]);
  const search = useSearch({ strict: false }) as { entity?: string };
  const navigate = useNavigate();
  const qc = useQueryClient();

  // 实体选择优先取 URL（可深链），非法值回落到第一个。
  const activeEntity = entities.find((e) => e.key === search.entity) ?? entities[0];
  const [keyword, setKeyword] = useState("");
  const [page, setPage] = useState(1);
  const [editing, setEditing] = useState<AdminAppPluginRecord | null>(null);
  const [creating, setCreating] = useState(false);
  const [problems, setProblems] = useState<{ path: string; message: string }[]>([]);

  const install = detail.data?.install;
  // 搜索框文案跟着实体声明走：后端搜的是「编号 + 业务唯一值」两列，
  // 写死「按编号搜索」会让人以为搜不了单号（那才是他手上有的东西）。
  const keywordHint = activeEntity?.unique
    ? `按${fieldOf(activeEntity, activeEntity.unique)?.label ?? "唯一值"}或编号搜索`
    : "按编号搜索";
  // 已卸载/停用都不发记录请求：明知会 409/404 还打一次，只会把错误态画到页面上。
  const canRead = !!install && install.status !== 3;
  const canWrite = !!install && install.status === 1 && !detail.data?.catalog_disabled;
  const records = useAdminAppPluginRecordsQuery(
    appId,
    pluginKey,
    { entity: activeEntity?.key ?? "", keyword, page, page_size: PAGE_SIZE },
    canRead && !!activeEntity,
  );

  const create = useCreateAppPluginRecordMutation();
  const patchPublic = usePatchAppPluginMutation();
  const update = useUpdateAppPluginRecordMutation();
  const remove = useDeleteAppPluginRecordMutation();

  const pickEntity = (key: string) => {
    setPage(1);
    setKeyword("");
    // 写 URL 而不是只改本地 state：地址要能直接分享/刷新。
    void navigate({ to: ".", search: { entity: key } as never, replace: true });
  };

  const failWith = (err: unknown, fallback: string) => {
    const data = (err as { data?: { problems?: { path: string; message: string }[] } })?.data;
    setProblems(data?.problems ?? []);
    toastApiError(err, fallback);
  };

  const data = detail.data;
  // 标题先渲染：加载中/失败/未安装时页面仍要有一个 h1 ——
  // 否则「每页唯一 h1」与「DOM id 唯一」两条门禁在异常态下量不到元素。
  const heading = (
    <PageHead
      title={data ? `${data.plugin.name} · ${data.app.name}` : "插件"}
      desc={data ? `插件 ${data.install.plugin_version || data.plugin.version} · 共 ${data.record_count} 条` : undefined}
    />
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
        <EmptyState title="插件未安装" desc="回到「接入应用 → 插件」安装后再打开。" />
      </>
    );
  }

  const entityView = activeEntity;
  const canCreate = canWrite && !!entityView?.list.actions.includes("create");
  const canExport = !!entityView?.list.actions.includes("export");

  // 对外只读：开关是**安装级**，能公开哪些列由 manifest 的 entities[].public 决定。
  // 两者同时成立才真的下发数据，所以这里既显示开关也显示"这个对象公不公开"。
  const publicURL =
    install?.public_enabled && entityView && entityView.public.length > 0
      ? `${window.location.origin}/api/apps/${data.app.slug}/p/${pluginKey}/${entityView.key}`
      : "";
  const canTogglePublic = !!install && install.status === 1 && !data.catalog_disabled;

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
        <div className="alert-banner info">该插件在本应用内已停用，写入已关闭；可在「接入应用 → 插件」里启用。</div>
      ) : null}
      {install?.status === 3 ? (
        <div className="alert-banner info">
          该插件已卸载，数据仍保留（{data.record_count} 条）。重新启用后即可继续使用。
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
          onChange={(e) => {
            setPage(1);
            setKeyword(e.target.value);
          }}
        />
        <button
          type="button"
          className="btn btn-primary"
          disabled={!canCreate}
          onClick={() => {
            setProblems([]);
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
            void exportAppPluginRecords(appId, pluginKey, { entity: entityView.key, keyword }).catch((err) =>
              toastApiError(err, "导出失败"),
            );
          }}
        >
          导出 CSV
        </button>
      </div>

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
          pageSize={PAGE_SIZE}
          loading={records.isPending}
          error={
            records.isError ? (
              <ErrorState error={records.error} onRetry={() => void records.refetch()} />
            ) : undefined
          }
          onPageChange={setPage}
          canWrite={canWrite}
          onEdit={(row) => {
            setProblems([]);
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

      {/* key 随目标记录变化：Dialog 关闭不卸载子树，只有重挂才能清空表单。 */}
      <RecordDialog
        key={editing?.record_key ?? "new"}
        entity={entityView ?? { key: "", name: "", unique: "", fields: [], list: { columns: [], actions: [] } }}
        open={creating || editing !== null}
        record={editing}
        submitting={create.isPending || update.isPending}
        problems={problems}
        onClose={() => {
          setCreating(false);
          setEditing(null);
          setProblems([]);
        }}
        onSubmit={(payload) => {
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

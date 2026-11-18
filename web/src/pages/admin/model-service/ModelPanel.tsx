import { useMemo, useState, type ReactNode } from "react";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { toast } from "sonner";

import { VendorManagerDialog } from "@/components/admin/VendorManagerDialog";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { DataTable } from "@/components/common/DataTable";
import { Dialog } from "@/components/common/Dialog";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityCell } from "@/components/common/EntityCell";
import { EntityIcon } from "@/components/common/EntityIcon";
import { EntitySelect } from "@/components/common/EntitySelect";
import { Field } from "@/components/common/Field";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { Icon } from "@/components/common/Icon";
import { Pagination } from "@/components/common/Pagination";
import { Tag } from "@/components/common/StatusTag";
import {
  useAdminModelsQuery,
  useDeleteAdminChannelMutation,
  useSetChannelStatusMutation,
  useTestChannelMutation,
  useToggleAdminModelMutation,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { actionsCol } from "@/lib/table";
import type { AdminModelItem, ChannelItem, TagColorOption } from "@/types";

import { BindDialog } from "./BindDialog";
import { ChannelOverview } from "./ChannelOverview";
import { ModelDialog } from "./ModelDialog";
import { priceSummary } from "./modelForm";
import { SyncDialog } from "./SyncDialog";
import { channelStatusLabel, type AdapterGroup } from "./grouping";

const EMPTY_FILTERS = { fKw: "", fEnabled: "", fVendor: "" };

/**
 * 右栏顶条：三态（渠道 / 适配器总览 / 根提示）共用一条壳。
 *
 * 三处各写一遍的结果是「同一件事三种排布」：渠道条有四个动作、总览条有一个、
 * 根提示只有一句话 —— 但它们的左对齐、间距、分隔线必须完全一样，否则切一次视图
 * 顶条就跳一下。窄屏的抽屉开关也放在这里：两个视图都有它，用户不用先找。
 */
function MainBar({
  leading,
  title,
  meta,
  actions,
  onOpenRail,
}: {
  leading: ReactNode;
  title: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  onOpenRail: () => void;
}) {
  return (
    <div className="ms-main__bar ms-chanbar">
      <button
        type="button"
        className="icon-btn ms-rail-toggle"
        aria-label="打开适配器列表"
        onClick={onOpenRail}
      >
        <Icon name="rows" size={18} />
      </button>
      <div className="ms-chanbar__id">
        {leading}
        <b>{title}</b>
        {meta}
      </div>
      {actions ? <div className="row-acts">{actions}</div> : null}
    </div>
  );
}

/**
 * 右栏：选中渠道下的模型列表 + 该渠道的操作台。
 *
 * 三条语义必须写在界面上（否则运营会按错误的心智模型操作）：
 *  1. 模型与价格是**全局**的，一个模型可挂多条渠道；这里改的只是本渠道的绑定；
 *  2. 解绑 ≠ 删除模型 —— 文案必须点明"其他渠道不受影响"；
 *  3. 渠道的启停/测试/删除只能从**渠道条**做（左栏是导航，不是操作面板）。
 */
export function ModelPanel({
  channel,
  group,
  onEditChannel,
  onChannelDeleted,
  onSelectChannel,
  onCreateChannel,
  onOpenRail,
}: {
  channel: ChannelItem | null;
  /** 未选中渠道时要展示的适配器（`?a` 指向的那个）；null = 根提示态 */
  group: AdapterGroup | null;
  onEditChannel: (ch: ChannelItem) => void;
  /** 渠道被删除：带上它的适配器 id，页面据此清 `c`、保留 `a` */
  onChannelDeleted: (adapterId: string) => void;
  /** 总览里点一行 → 选中该渠道 */
  onSelectChannel: (ch: ChannelItem) => void;
  onCreateChannel: (adapterId: string) => void;
  /** ≤899 打开左栏抽屉（按钮在桌面被 CSS 隐藏） */
  onOpenRail: () => void;
}) {
  const [filters, setFilters] = useState(EMPTY_FILTERS);
  const [page, setPage] = useState(1);
  const [sorting, setSorting] = useState<SortingState>([]);
  /** 保存后的价格告警：留在列表上方而不是只弹 toast —— toast 几秒就没了，而它说的是"合法但会长期少收/多收"。 */
  const [warnings, setWarnings] = useState<string[]>([]);

  // 弹窗目标：model = 编辑对象（null 且 open 时为新建）
  const [dialog, setDialog] = useState<{ open: boolean; model: AdminModelItem | null }>({
    open: false,
    model: null,
  });
  const [bindOpen, setBindOpen] = useState<{ open: boolean; model: AdminModelItem | null }>({
    open: false,
    model: null,
  });
  const [vendorOpen, setVendorOpen] = useState(false);
  const [syncOpen, setSyncOpen] = useState(false);
  /** 停用确认（停用要填原因，入审计） */
  const [statusTarget, setStatusTarget] = useState<{ ch: ChannelItem; next: number } | null>(null);
  const [statusReason, setStatusReason] = useState("");
  const [testingId, setTestingId] = useState<number | null>(null);

  const params = useMemo(
    () => ({
      keyword: filters.fKw.trim() || undefined,
      // 三态："" = 不限；"true"/"false" = 模型自身启停
      enabled: filters.fEnabled === "" ? undefined : filters.fEnabled === "true",
      vendor: filters.fVendor || undefined,
      channel_id: channel?.id,
      page,
      page_size: DEFAULT_PAGE_SIZE,
      sort: sorting[0]?.id,
      order: sorting[0] ? ((sorting[0].desc ? "desc" : "asc") as "desc" | "asc") : undefined,
    }),
    [filters, page, sorting, channel?.id],
  );

  // 没有渠道就不发请求：params 里 channel_id 为 undefined 时后端会回**全量模型**，
  // 而总览视图根本不渲染这张表。
  const q = useAdminModelsQuery(params, !!channel);
  const rows = q.data?.items ?? [];
  const total = q.data?.total ?? 0;
  // 总览标题里的「N 条停用」从已全量下发的渠道行数出来，不额外请求。
  const stoppedCount = group?.channels.filter((c) => c.status !== 1).length ?? 0;
  // 供应商列表随 /admin/models 一起下发，**不再单独请求 /admin/vendors**：
  // 那是同一份数据，两个请求只会在改分组后短暂显示不一致。
  const vendors = q.data?.vendors ?? [];
  const tagPalette = (q.data?.tag_palette ?? []) as TagColorOption[];

  const deleteChannel = useDeleteAdminChannelMutation();
  const statusMut = useSetChannelStatusMutation();
  const testMut = useTestChannelMutation();
  const toggleModel = useToggleAdminModelMutation();

  /** 该渠道上已有的模型 id（选择器里据此标「已绑定」）—— 来自渠道行的全量绑定摘要，不额外请求。 */
  const boundIds = useMemo(
    () => new Set((channel?.bound_models ?? []).map((b) => b.model)),
    [channel],
  );

  const runTest = (ch: ChannelItem) => {
    setTestingId(ch.id);
    testMut.mutate(ch.id, {
      onSuccess: (data) => {
        const r = data.result;
        if (r.ok) toast.success(`${ch.name}：${r.message}（${r.latency_ms}ms）`);
        else toast.error(`${ch.name} 连通失败：${r.message}`);
      },
      onError: (e) => toast.error(e instanceof ApiError ? e.message : "测试失败，请稍后重试"),
      onSettled: () => setTestingId(null),
    });
  };

  const confirmStatus = () => {
    if (!statusTarget) return;
    const { ch, next } = statusTarget;
    statusMut.mutate(
      { id: ch.id, status: next, reason: statusReason.trim() || undefined },
      {
        onSuccess: () => {
          toast.success(next === 1 ? `${ch.name} 已启用` : `${ch.name} 已停用`);
          setStatusTarget(null);
          setStatusReason("");
        },
        onError: (e) => toast.error(e instanceof ApiError ? e.message : "操作失败，请稍后重试"),
      },
    );
  };

  const removeChannel = async (ch: ChannelItem) => {
    if (ch.bound_models.length) {
      // 与后端判据逐字一致：**任意绑定（含停用）**都拦住删除。
      // 少于这条判据的表现是前端放行、后端 409，运营看到「空渠道却删不掉」。
      toast.error(`「${ch.name}」仍绑定 ${ch.bound_models.length} 个模型，请先解绑`);
      return;
    }
    const ok = await confirmDialog({
      title: "删除渠道",
      body: (
        <div>
          <p>
            将删除渠道 <b>#{ch.id} {ch.name}</b>（{ch.adapter_id}，{ch.key_count} 个密钥）。
          </p>
          <p className="muted small">密钥池与健康快照一并移除，删除后无法恢复。</p>
        </div>
      ),
      okText: "删除",
      okVariant: "danger",
    });
    if (!ok) return;
    deleteChannel.mutate(ch.id, {
      onSuccess: () => {
        toast.success("渠道已删除");
        onChannelDeleted(ch.adapter_id);
      },
      onError: (e) => toast.error(e instanceof ApiError ? e.message : "删除失败，请稍后重试"),
    });
  };

  const columns = useMemo<ColumnDef<AdminModelItem, unknown>[]>(() => {
    const base: ColumnDef<AdminModelItem, unknown>[] = [
      {
        id: "display_name",
        accessorKey: "display_name",
        enableSorting: true,
        meta: { label: "模型" },
        cell: ({ row }) => (
          <EntityCell
            iconUrl={row.original.icon_url}
            name={row.original.display_name || row.original.id}
          >
            <span className="mono small muted">{row.original.id}</span>
          </EntityCell>
        ),
      },
      {
        id: "vendor",
        accessorKey: "vendor_name",
        enableSorting: true,
        meta: { label: "供应商" },
        cell: ({ row }) => (
          <span className="small">{row.original.vendor_name || row.original.vendor}</span>
        ),
      },
      {
        id: "billing_mode",
        accessorKey: "billing_mode",
        enableSorting: true,
        meta: { label: "计费" },
        cell: ({ row }) => (
          <span className="small">
            {priceSummary(row.original)}
            {row.original.is_free ? <Tag tone="success">免费</Tag> : null}
          </span>
        ),
      },
    ];
    // 「本渠道」这一列只在选中渠道时存在：没有渠道上下文时它没有意义。
    if (channel) {
      base.push({
        id: "binding",
        enableSorting: false,
        meta: { label: "本渠道" },
        cell: ({ row }) => {
          const b = row.original.binding;
          if (!b) return <span className="muted small">—</span>;
          return (
            <span className="small mono">
              {b.upstream_model}
              {b.weight !== 1 ? <span className="muted"> · 权重 {b.weight}</span> : null}
              {b.enabled ? null : <Tag tone="warning">已停用</Tag>}
            </span>
          );
        },
      });
    }
    base.push({
      id: "enabled",
      accessorKey: "enabled",
      enableSorting: true,
      meta: { label: "状态" },
      cell: ({ row }) =>
        row.original.enabled ? <Tag tone="success">启用</Tag> : <Tag tone="gray">停用</Tag>,
    });
    base.push(
      actionsCol<AdminModelItem>(
        (m) => {
          const bound = Boolean(m.binding);
          return [
            { icon: "edit", label: "编辑模型", onClick: () => setDialog({ open: true, model: m }) },
            {
              icon: "link",
              // 未绑定时说清楚"点了会发生什么"：这一步是**新建**绑定，
              // 而"绑定设置"读起来像在改一个已存在的配置。
              label: bound ? "绑定设置" : "绑定到本渠道",
              onClick: () => setBindOpen({ open: true, model: m }),
            },
            {
              icon: "close",
              label: "解除绑定",
              tone: "danger",
              // 只有已绑定的行才显示「解除绑定」：对未绑定的行显示等于给一个必然 404 的入口。
              hidden: !bound,
              onClick: () => setBindOpen({ open: true, model: m }),
            },
            {
              icon: "power",
              label: m.enabled ? "停用模型" : "启用模型",
              onClick: () =>
                toggleModel.mutate(
                  { id: m.id, enabled: !m.enabled },
                  {
                    onSuccess: (data) =>
                      toast(data?.enabled ? `已启用 ${m.id}` : `已禁用 ${m.id}`),
                    onError: (e) => toast.error(e instanceof ApiError ? e.message : "操作失败"),
                  },
                ),
            },
          ];
        },
        "操作",
        "act",
      ),
    );
    return base;
  }, [channel, toggleModel]);

  const filterFields: FilterField[] = [
    { t: "search", id: "fKw", label: "关键词", placeholder: "搜索模型 ID / 名称" },
    {
      t: "select",
      id: "fEnabled",
      label: "状态",
      allLabel: "全部状态",
      items: [
        ["true", "启用"],
        ["false", "停用"],
      ],
    },
    {
      t: "select",
      id: "fVendor",
      label: "供应商",
      allLabel: "全部供应商",
      items: vendors.map((v) => [v.id, v.name] as [string, string]),
      // 供应商名经常长到被截断，只有图标能一眼分辨同名分组 —— 与模型表单里的
      // 供应商选择器共用同一个组件，避免"这里能选图标、那里不能"的两套观感。
      renderer: (value, onChange) => (
        <EntitySelect
          ariaLabel="供应商"
          value={value}
          allLabel="全部供应商"
          placeholder="全部供应商"
          options={vendors.map((v) => ({
            value: v.id,
            label: v.name,
            iconUrl: v.icon_url,
            color: v.color,
          }))}
          onChange={onChange}
        />
      ),
    },
    { t: "btn", id: "btnNewModel", label: "新建模型", variant: "primary", icon: "plus" },
    { t: "btn", id: "btnBind", label: "绑定已有模型", icon: "link" },
    { t: "btn", id: "btnVendors", label: "供应商", icon: "grid" },
    { t: "btn", id: "btnSync", label: "上游同步", icon: "refresh" },
  ];

  return (
    <section className="ms-main" aria-label="模型列表">
      <MainBar
        onOpenRail={onOpenRail}
        leading={
          channel ? (
            <EntityIcon size="sm" url={channel.icon_url} name={channel.name} />
          ) : group ? (
            <EntityIcon size="sm" url={group.iconUrl} name={group.label} />
          ) : (
            <Icon name="grid" size={15} />
          )
        }
        title={channel ? channel.name : group ? group.label : "模型服务"}
        meta={
          channel ? (
            <>
              <span className="mono small muted">{channel.base_url}</span>
              {channel.status === 1 ? null : (
                <Tag tone="warning">{channelStatusLabel(channel.status)}</Tag>
              )}
            </>
          ) : group ? (
            <span className="small muted">
              {group.channels.length} 条渠道
              {stoppedCount > 0 ? `（${stoppedCount} 条停用）` : ""}
            </span>
          ) : (
            <span className="small muted">从左侧选一个适配器</span>
          )
        }
        actions={
          channel ? (
            <>
              <button type="button" className="btn btn-sm" onClick={() => onEditChannel(channel)}>
                编辑渠道
              </button>
              <button
                type="button"
                className="btn btn-sm"
                disabled={testingId === channel.id}
                onClick={() => runTest(channel)}
              >
                {testingId === channel.id ? "测试中…" : "测试连通"}
              </button>
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => setStatusTarget({ ch: channel, next: channel.status === 1 ? 2 : 1 })}
              >
                {channel.status === 1 ? "停用渠道" : "启用渠道"}
              </button>
              <button
                type="button"
                className="btn btn-sm btn-danger"
                onClick={() => void removeChannel(channel)}
              >
                删除渠道
              </button>
            </>
          ) : group ? (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => onCreateChannel(group.id)}
            >
              新建渠道
            </button>
          ) : null
        }
      />

      {/* 模型域的工具条与价格告警只在渠道视图里出现：
          搜索/筛选过滤的是模型，没有渠道上下文时它们"按了没反应"。 */}
      {channel ? (
        <>
          <div className="ms-main__tools">
            <FilterBar
              fields={filterFields}
              value={filters}
              onChange={(id, value) => {
                setFilters((f) => ({ ...f, [id]: value }));
                setPage(1);
              }}
              onReset={() => {
                setFilters(EMPTY_FILTERS);
                setPage(1);
              }}
              onAction={(id) => {
                if (id === "btnNewModel") setDialog({ open: true, model: null });
                else if (id === "btnBind") setBindOpen({ open: true, model: null });
                else if (id === "btnVendors") setVendorOpen(true);
                else if (id === "btnSync") setSyncOpen(true);
              }}
            />
          </div>

          {warnings.length > 0 ? (
            <div className="alert-banner warn" role="status">
              <span>
                <b>已保存，但有 {warnings.length} 条价格提醒</b>
                <ul>
                  {warnings.map((w) => (
                    <li key={w}>{w}</li>
                  ))}
                </ul>
              </span>
              <button type="button" className="btn btn-sm right" onClick={() => setWarnings([])}>
                知道了
              </button>
            </div>
          ) : null}
        </>
      ) : null}

      <div className="ms-main__body">
        {channel ? (
          <>
            <DataTable
              label="渠道模型"
              columns={columns}
              data={rows}
              total={total}
              page={page}
              pageSize={DEFAULT_PAGE_SIZE}
              onPageChange={setPage}
              sort={sorting}
              onSortChange={setSorting}
              loading={q.isLoading}
              error={q.isError ? "模型列表加载失败" : undefined}
              empty={
                <EmptyState
                  title="这个渠道还没有模型"
                  desc="点「新建模型」建一个并自动绑到本渠道，或用「绑定已有模型」把现有模型挂上来。"
                />
              }
            />
            <Pagination total={total} page={page} pageSize={DEFAULT_PAGE_SIZE} onPageChange={setPage} />
          </>
        ) : group ? (
          <ChannelOverview
            group={group}
            onSelect={onSelectChannel}
            onCreateChannel={onCreateChannel}
          />
        ) : (
          <EmptyState
            title="选择左侧的渠道"
            desc="模型挂在渠道上；先选一条渠道，再管理它的模型。"
          />
        )}
      </div>

      {/* 弹窗常驻（不随 open 卸载）：价格告警、草稿与开合动画都依赖节点保留。 */}
      <ModelDialog
        open={dialog.open}
        model={dialog.model}
        channel={channel}
        vendors={vendors}
        tagPalette={tagPalette}
        onClose={() => setDialog((d) => ({ ...d, open: false }))}
        onSaved={(w) => setWarnings(w)}
        onDeleted={() => setDialog((d) => ({ ...d, open: false }))}
      />

      <BindDialog
        open={bindOpen.open}
        channel={channel}
        boundIds={boundIds}
        model={bindOpen.model}
        onClose={() => setBindOpen((b) => ({ ...b, open: false }))}
      />

      <VendorManagerDialog open={vendorOpen} onClose={() => setVendorOpen(false)} />
      <SyncDialog open={syncOpen} onClose={() => setSyncOpen(false)} />

      <Dialog
        open={statusTarget !== null}
        onClose={() => setStatusTarget(null)}
        title={statusTarget?.next === 1 ? "启用渠道" : "停用渠道"}
        actions={[
          { label: "取消", onClick: () => setStatusTarget(null) },
          {
            label: `确认${statusTarget?.next === 1 ? "启用" : "停用"}`,
            variant: "primary",
            disabled: statusMut.isPending,
            onClick: confirmStatus,
          },
        ]}
      >
        {statusTarget ? (
          <>
            <Field label="渠道">
              <input type="text" readOnly value={`#${statusTarget.ch.id} ${statusTarget.ch.name}`} />
            </Field>
            {statusTarget.next === 1 ? (
              <div className="hint">启用后该渠道立即参与选路，可重新转发请求。</div>
            ) : (
              <Field
                label="停用原因（选填，写入审计）"
                htmlFor="chz-reason"
                hint="停用后该渠道不再参与选路，已绑定的模型需改由其他渠道承载。"
              >
                <textarea
                  id="chz-reason"
                  rows={2}
                  placeholder="例如：上游密钥泄漏，等待更换"
                  value={statusReason}
                  onChange={(e) => setStatusReason(e.target.value)}
                />
              </Field>
            )}
          </>
        ) : null}
      </Dialog>
    </section>
  );
}

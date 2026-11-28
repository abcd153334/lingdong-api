import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
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
import { ModelDialog } from "./ModelDialog";
import { priceSummary } from "./modelForm";
import { channelStatusLabel } from "./grouping";

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
 * 四条语义必须写在界面上（否则运营会按错误的心智模型操作）：
 *  1. 模型与价格是**全局**的，一个模型可挂多条渠道；这里改的只是本渠道的绑定；
 *  2. 解绑 ≠ 删除模型 —— 文案必须点明"其他渠道不受影响"；
 *  3. 渠道的启停/测试/删除只能从**渠道条**做（左栏是导航，不是操作面板）；
 *  4. **新建模型只在渠道内**：模型必须在一个事务里带着 `initial_binding` 落库。
 *     全部模型视图没有渠道上下文，在那里新建等于生产一个绑不上、也没人能用上的孤儿模型。
 */
export function ModelPanel({
  channel,
  channelPending,
  onEditChannel,
  onChannelDeleted,
  onOpenRail,
}: {
  channel: ChannelItem | null;
  /**
   * `?c` 已写但渠道列表还没回来（深链 / 刚刷新）。
   *
   * 这一位存在是为了**避免打两次请求**：此时 `channel` 还是 null，若直接按
   * “无 channel_id” 发一次，拿到的会是全量模型，等列表回来后立刻再发一次带
   * channel_id 的 —— 首屏闪一下全站数据，还多一次慢查询。
   */
  channelPending: boolean;
  onEditChannel: (ch: ChannelItem) => void;
  /** 渠道被删除：页面清 `c` 回到全部模型 */
  onChannelDeleted: (adapterId: string) => void;
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

  // 两态都发请求：无 channel_id = 跨渠道全量（后端原生支持）。
  // 唯一要挡住的是 `?c` 尚未解析完的那一帧（见 channelPending 的注释）。
  const q = useAdminModelsQuery(params, !channelPending);
  const rows = q.data?.items ?? [];
  const total = q.data?.total ?? 0;

  /**
   * 切渠道 → 回到第 1 页。
   *
   * 没有这一行就是**数据错**（不是样式问题）：在全部模型翻到第 5 页，再点进一条
   * 只有 2 个模型的渠道，请求带 page=5 → 空表 + 「这个渠道还没有模型」，
   * 而用户看到的是"明明刚才还列着"。
   *
   * 价格告警一起清：它说的是"上一次保存"，切走之后还挂在列表上方，
   * 会让人以为当前渠道刚保存过（保存成功的告警本来就只在保存现场有意义）。
   */
  useEffect(() => {
    setPage(1);
    setWarnings([]);
  }, [channel?.id]);
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

  /**
   * 模型级启停（**全局**：所有渠道一起生效）。
   *
   * 停用要在渠道视图里二次确认：这里的上下文是"某条渠道"，而这条动作关的是整个模型 ——
   * 不点明就会变成"我只想让它别走这条线路"，实际把其他渠道也一起下线了。
   * 只想关本渠道有另一条动作：行内「绑定设置」→「启用这条绑定」。
   * 启用没有这种歧义，不拦。
   */
  const requestToggle = useCallback(
    async (m: AdminModelItem) => {
      if (channel && m.enabled) {
        const ok = await confirmDialog({
          title: "停用模型（全部渠道）",
          body: (
            <div>
              <p>
                停用后 <b>{m.id}</b> 在<b>所有渠道</b>上立即停止服务，不只是「{channel.name}」。
              </p>
              <p className="muted small">
                只想让本渠道不再使用它，请用行内的「绑定设置」取消「启用这条绑定」。
              </p>
            </div>
          ),
          okText: "全部渠道停用",
          okVariant: "danger",
        });
        if (!ok) return;
      }
      toggleModel.mutate(
        { id: m.id, enabled: !m.enabled },
        {
          onSuccess: (data) => toast(data?.enabled ? `已启用 ${m.id}` : `已禁用 ${m.id}`),
          onError: (e) => toast.error(e instanceof ApiError ? e.message : "操作失败"),
        },
      );
    },
    [channel, toggleModel],
  );

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
            {/* 出厂标记：告诉运营这个模型来自内置预设（会被「恢复出厂」覆盖），
                不是自己建的。只在有标记时出现，不占固定列宽。 */}
            {row.original.preset_group ? (
              <Tag tone="gray" title={`出厂组：${row.original.preset_group}`}>
                内置
              </Tag>
            ) : null}
          </EntityCell>
        ),
      },
      {
        id: "vendor",
        accessorKey: "vendor_name",
        enableSorting: true,
        // 900px 下左栏 200 + 6 列会横向溢出：按"越靠上下文越可省"降级，
        // 先收供应商（priority 2），再收计费（priority 3）。状态与可路由渠道必须留着 ——
        // 这一页要回答的就是"这个模型挂在哪、能不能用"。
        meta: { label: "供应商", priority: 2 },
        cell: ({ row }) => (
          <span className="small">{row.original.vendor_name || row.original.vendor}</span>
        ),
      },
      {
        id: "billing_mode",
        accessorKey: "billing_mode",
        enableSorting: true,
        meta: { label: "计费", priority: 3 },
        cell: ({ row }) => (
          <span className="small">
            {priceSummary(row.original)}
            {row.original.is_free ? <Tag tone="success">免费</Tag> : null}
          </span>
        ),
      },
    ];
    // 两态的第四列不同源：
    //  - 有渠道 → 「本渠道」读 binding（该渠道上的上游名/权重/启停）；
    //  - 无渠道 → 「绑定渠道」读 bindings（计数），因为全量查询不下发逐条绑定。
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
    } else {
      /**
       * 列名必须说「可路由」而不是「绑定」。
       *
       * 全量查询下发的 `bindings` 是**可路由**的绑定数（启用绑定 ∩ 启用渠道），
       * 不是绑定总条数：一个模型挂在一条停用渠道上时它会是 0。叫「绑定渠道」再显示
       * 「未绑定」就是在说反话 —— 运营会据此认为模型是孤儿（去删，被 409；
       * 去重建，被 409），而它其实只是这条线路当前不可用。
       */
      base.push({
        id: "bindings",
        accessorKey: "bindings",
        enableSorting: false,
        meta: { label: "可路由渠道" },
        cell: ({ row }) =>
          row.original.bindings > 0 ? (
            <span className="small">{row.original.bindings} 条渠道</span>
          ) : (
            <Tag tone="gray">无可用</Tag>
          ),
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
          /** 绑定相关动作必须有渠道上下文：`BindDialog` 在 channel=null 时 save() 直接 return，
           *  留在全部模型里就是"点了没反应也不报错"的死入口。 */
          const bindActs = channel
            ? [
                {
                  icon: "link" as const,
                  // 未绑定时说清楚"点了会发生什么"：这一步是**新建**绑定，
                  // 而"绑定设置"读起来像在改一个已存在的配置。
                  label: bound ? "绑定设置" : "绑定到本渠道",
                  onClick: () => setBindOpen({ open: true, model: m }),
                },
                {
                  icon: "close" as const,
                  label: "解除绑定",
                  tone: "danger" as const,
                  // 只有已绑定的行才显示「解除绑定」：对未绑定的行显示等于给一个必然 404 的入口。
                  hidden: !bound,
                  onClick: () => setBindOpen({ open: true, model: m }),
                },
              ]
            : [];
          return [
            { icon: "edit", label: "编辑模型", onClick: () => setDialog({ open: true, model: m }) },
            ...bindActs,
            {
              icon: "power",
              label: m.enabled ? "停用模型" : "启用模型",
              onClick: () => void requestToggle(m),
            },
          ];
        },
        "操作",
        "act",
      ),
    );
    return base;
  }, [channel, requestToggle]);

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
  ];
  /**
   * 「新建模型 / 绑定已有模型」**只在渠道态出现**，理由同一条：
   *
   *  - 新建：模型必须在同一事务里带 `initial_binding` 落库。全部模型视图没有
   *    "绑到哪"这个主语，建出来就是一个谁也用不上的孤儿（还删不掉 —— 想删它得先绑，
   *    想绑它得先进渠道，而建它的时候偏偏没有渠道）；
   *  - 绑定：`BindDialog` 在 channel=null 时 save() 直接 return，留着就是个
   *    点了没反应也不报错的死入口。
   *
   * 两者必须相邻：都是"往本渠道加模型"的动线。供应商不是 —— 它排在最后。
   */
  if (channel) {
    filterFields.push({ t: "btn", id: "btnNewModel", label: "新建模型", variant: "primary", icon: "plus" });
    filterFields.push({ t: "btn", id: "btnBind", label: "绑定已有模型", icon: "link" });
  }
  filterFields.push({ t: "btn", id: "btnVendors", label: "供应商", icon: "grid" });

  /** 空表有两种含义：筛选没命中 / 一个模型都没有 —— 引导完全不同，不能共用一句文案。 */
  const hasFilter = Boolean(filters.fKw.trim() || filters.fEnabled || filters.fVendor);

  return (
    <section className="ms-main" aria-label="模型列表">
      <MainBar
        onOpenRail={onOpenRail}
        leading={
          channel ? (
            <EntityIcon size="sm" url={channel.icon_url} name={channel.name} />
          ) : (
            <Icon name="grid" size={16} />
          )
        }
        title={channel ? channel.name : "全部模型"}
        meta={
          channel ? (
            <>
              <span className="mono small muted">{channel.base_url}</span>
              {channel.website_url ? (
                // 渠道条是"快速跳转"的落点：编辑抽屉要三步才打开，而这里地址就在眼前。
                <a
                  className="ms-chanbar__site"
                  href={channel.website_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  aria-label="打开上游官网"
                  title={`上游官网：${channel.website_url}`}
                >
                  <Icon name="external" size={13} />
                </a>
              ) : null}
              {channel.status === 1 ? null : (
                <Tag tone="warning">{channelStatusLabel(channel.status)}</Tag>
              )}
              {/* 内置标记：让运营知道「这条会被恢复出厂覆盖」，以及它来自出厂而不是自己建的。
                  只是个说明徽标，不是编辑开关 —— 内置渠道照样能改名改价。 */}
              {channel.preset_group ? (
                <Tag tone="gray" title={`出厂组：${channel.preset_group}`}>
                  内置
                </Tag>
              ) : null}
            </>
          ) : (
            // 全量视图的唯一元信息就是条数：没有渠道就没有地址/状态可说
            <span className="small muted">
              {q.isLoading ? "加载中…" : `共 ${total} 个模型`}
            </span>
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
          ) : undefined
        }
      />

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

      <div className="ms-main__body">
        <DataTable
          label={channel ? "渠道模型" : "全部模型"}
          columns={columns}
          data={rows}
          total={total}
          page={page}
          pageSize={DEFAULT_PAGE_SIZE}
          onPageChange={setPage}
          sort={sorting}
          onSortChange={setSorting}
          loading={q.isLoading || channelPending}
          error={q.isError ? "模型列表加载失败" : undefined}
          empty={
            channel ? (
              /**
               * 渠道视图也要分两种情况：筛选没命中和真的没有模型。
               * 只说「这个渠道还没有模型」会让运营去建一个**已经存在**的模型（409），
               * 而实际原因只是关键词还留着上一条渠道的搜索词。
               */
              hasFilter ? (
                <EmptyState title="没有符合条件的模型" desc="换个关键词，或清空筛选再试。" />
              ) : (
                <EmptyState
                  title="这个渠道还没有模型"
                  desc="点「新建模型」建一个并自动绑到本渠道，或用「绑定已有模型」把现有模型挂上来。"
                />
              )
            ) : (
              /**
               * 全部模型视图没有"新建"入口（见 filterFields 的注释），所以空表不能只说
               * "换个关键词" —— 一个模型都没有时，得把**唯一**的正确动线写出来，
               * 否则运营在这一页找不到任何能点的东西。
               */
              <EmptyState
                title={hasFilter ? "没有符合条件的模型" : "还没有任何模型"}
                desc={
                  hasFilter
                    ? "换个关键词，或清空筛选再试。"
                    : "模型必须挂在渠道上提供：请在左侧选择一个适配器 → 渠道，在渠道内「新建模型」（建完即绑定），已有模型可用「绑定已有模型」挂上去。"
                }
              />
            )
          }
        />
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

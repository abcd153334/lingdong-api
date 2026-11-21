import { useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { toast } from "sonner";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { DataTable } from "@/components/common/DataTable";
import { Dialog } from "@/components/common/Dialog";
import { Drawer } from "@/components/common/Drawer";
import { EntityCell } from "@/components/common/EntityCell";
import { Field } from "@/components/common/Field";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { Input } from "@/components/common/Input";
import { MoneyText, QuotaUnit } from "@/components/common/MoneyText";
import { PageHead } from "@/components/common/PageHead";
import { Select } from "@/components/common/Select";
import { StatusTag } from "@/components/common/StatusTag";
import {
  useAdminAdjustQuotaMutation,
  useAdminAppOptionsQuery,
  useAdminLogsQuery,
  useAdminUpdateUserMutation,
  useAdminUserKeysQuery,
  useAdminUserTransactionsQuery,
  useAdminUsersQuery,
  useAdminConfigQuery,
  type AdminUsersParams,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { appFilterField, appIdParam } from "@/lib/appFilter";
import { TRANSACTION_LABELS, formatDateTime } from "@/lib/format";
import { createAdjustQuotaSchema } from "@/lib/schemas";
import { actionsCol } from "@/lib/table";
import { cn } from "@/lib/utils";
import { currentSymbol, groupDiscountText, microOf, microToInput, money2, moneyOf, otherMoneyOf } from "@/lib/money";
import type {
  AdminAppOption,
  AdminLogItem,
  AdminUserKeyItem,
  AdminUserRow,
  TransactionItem,
} from "@/types";

const EMPTY_FILTERS: Record<string, string> = { fKw: "", fGroup: "", fStatus: "", fApp: "" };

/**
 * 筛选字段：分组集合来自门户配置（S18），随配置热更新。
 *
 * 应用筛选的语义是「在该应用下持有托管密钥」——**不是**归属：用户是全局池，
 * 同一个人可以同时在多个应用下各持 1 把密钥。因此这一页不显示「所属应用」列
 * （见页面注释），筛选是唯一的正确表达。
 */
const filterFields = (groupKeys: string[], appItems: AdminAppOption[]): FilterField[] => [
  // withDirect=false：不存在「属于平台直连的用户」，后端会对 app_id=0 直接 400。
  appFilterField(appItems, { withDirect: false }),
  { t: "search", id: "fKw", label: "关键词", placeholder: "用户名关键词" },
  { t: "select", id: "fGroup", label: "分组", allLabel: "全部分组", items: groupKeys },
  { t: "select", id: "fStatus", label: "状态", allLabel: "全部状态", items: [[1, "启用"], [2, "禁用"]] },
  { t: "btn", id: "reset", label: "重置" },
];

/** 列定义：分组折扣展示需要配置快照（S18），故以函数注入 discountOf */
const baseColumns = (discountOf: (g: string) => string): ColumnDef<AdminUserRow, unknown>[] => [
  {
    id: "id",
    accessorKey: "id",
    enableSorting: false,
    meta: { label: "ID", priority: 3 },
    cell: ({ row }) => <span className="mono">{row.original.id}</span>,
  },
  {
    id: "username",
    accessorKey: "username",
    enableSorting: false,
    meta: { label: "用户名" },
    cell: ({ row }) => <span className="mono">{row.original.username}</span>,
  },
  {
    id: "group",
    accessorKey: "group",
    enableSorting: false,
    meta: { label: "分组", priority: 3 },
    cell: ({ row }) => (
      <span className="badge badge-info">
        {row.original.group} · {discountOf(row.original.group)}
      </span>
    ),
  },
  {
    id: "balance",
    accessorKey: "balance",
    enableSorting: false,
    meta: { label: "余额" },
    cell: ({ row }) => <QuotaUnit micro={row.original.balance} />,
  },
  {
    id: "used",
    accessorKey: "used",
    enableSorting: false,
    meta: { label: "累计消耗", priority: 2 },
    cell: ({ row }) => <MoneyText micro={row.original.used} className="small" />,
  },
  {
    id: "request_count",
    accessorKey: "request_count",
    enableSorting: false,
    meta: { label: "请求数", priority: 2 },
    cell: ({ row }) => <span className="num">{row.original.request_count.toLocaleString("en-US")}</span>,
  },
  {
    id: "status",
    accessorKey: "status",
    enableSorting: false,
    meta: { label: "状态" },
    cell: ({ row }) => <StatusTag status={row.original.status} />,
  },
  {
    id: "created_at",
    accessorKey: "created_at",
    enableSorting: false,
    meta: { label: "注册", priority: 2 },
    cell: ({ row }) => <span className="mono small">{row.original.created_at.slice(0, 10)}</span>,
  },
];

/* ---------- 详情抽屉三块只读数据（S17；均由 AdminAuth 保护，均不支持写入） ---------- */

const DETAIL_TX_COLUMNS: ColumnDef<TransactionItem, unknown>[] = [
  {
    id: "created_at",
    accessorKey: "created_at",
    enableSorting: false,
    meta: { label: "时间" },
    cell: ({ row }) => <span className="mono small">{formatDateTime(row.original.created_at)}</span>,
  },
  {
    id: "type",
    accessorKey: "type",
    enableSorting: false,
    meta: { label: "类型" },
    cell: ({ row }) => (
      <span className="small">{TRANSACTION_LABELS[row.original.type] ?? row.original.type}</span>
    ),
  },
  {
    id: "amount",
    accessorKey: "amount",
    enableSorting: false,
    meta: { label: "变动" },
    cell: ({ row }) => <MoneyText micro={row.original.amount} signed />,
  },
  {
    id: "balance_after",
    accessorKey: "balance_after",
    enableSorting: false,
    meta: { label: "余额" },
    cell: ({ row }) => <MoneyText micro={row.original.balance_after} />,
  },
  {
    id: "reason",
    accessorKey: "reason",
    enableSorting: false,
    meta: { label: "说明", wrap: true },
    cell: ({ row }) => <span className="small muted">{row.original.reason || "—"}</span>,
  },
];

/** 密钥列表列：**仅索引前缀**，永不展示明文/密文（管理台不提供密钥回显能力） */
const DETAIL_KEY_COLUMNS: ColumnDef<AdminUserKeyItem, unknown>[] = [
  {
    id: "name",
    accessorKey: "name",
    enableSorting: false,
    meta: { label: "名称" },
    cell: ({ row }) => <span className="small">{row.original.name || "(未命名)"}</span>,
  },
  {
    id: "key_prefix",
    accessorKey: "key_prefix",
    enableSorting: false,
    meta: { label: "索引前缀" },
    cell: ({ row }) => <span className="mono small muted">{row.original.key_prefix}</span>,
  },
  {
    id: "group",
    accessorKey: "group",
    enableSorting: false,
    meta: { label: "分组", priority: 3 },
    cell: ({ row }) => <span className="badge badge-info">{row.original.group || "default"}</span>,
  },
  {
    id: "status",
    accessorKey: "status",
    enableSorting: false,
    meta: { label: "状态" },
    cell: ({ row }) => <StatusTag status={row.original.status} />,
  },
  {
    id: "created_at",
    accessorKey: "created_at",
    enableSorting: false,
    meta: { label: "创建" },
    cell: ({ row }) => <span className="mono small muted">{row.original.created_at.slice(0, 10)}</span>,
  },
];

const DETAIL_LOG_COLUMNS: ColumnDef<AdminLogItem, unknown>[] = [
  {
    id: "created_at",
    accessorKey: "created_at",
    enableSorting: false,
    meta: { label: "时间" },
    cell: ({ row }) => <span className="mono small">{formatDateTime(row.original.created_at)}</span>,
  },
  {
    id: "model",
    accessorKey: "model",
    enableSorting: false,
    meta: { label: "模型" },
    cell: ({ row }) => <EntityCell iconUrl={row.original.model_icon_url} name={row.original.model} mono className="small" />,
  },
  {
    id: "total_tokens",
    accessorKey: "total_tokens",
    enableSorting: false,
    meta: { label: "Token" },
    cell: ({ row }) => <span className="num small">{row.original.total_tokens.toLocaleString("en-US")}</span>,
  },
  {
    id: "charge",
    accessorKey: "charge",
    enableSorting: false,
    meta: { label: "费用" },
    cell: ({ row }) => <MoneyText micro={row.original.charge} />,
  },
  {
    id: "status",
    accessorKey: "http_status",
    enableSorting: false,
    meta: { label: "状态" },
    cell: ({ row }) => <StatusTag status={row.original.http_status} />,
  },
];

/** 抽屉内独立错误态：加载失败不关闭抽屉，可就地重试 */
function BlockError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  return (
    <div className="row-acts muted small">
      <span>{error instanceof ApiError ? error.message : "加载失败，请稍后重试"}</span>
      <button type="button" className="btn btn-sm" onClick={onRetry}>
        重试
      </button>
    </div>
  );
}

/**
 * 用户级余额上限编辑器（0 = 未单独设置、跟随全局配置）。
 *
 * 为什么必须有入口：额度上限在入账侧是**三道闸**（充值下单后校验、兑换码入账、
 * 人工调账），判定口径统一为「用户级 > 0 优先，否则回落全局」。若管理台不给写入口，
 * 这一列永远是 0，「给单个客户放宽/加严」就只能改全局 —— 那等于对所有用户生效，
 * 闸门等于没有。
 *
 * 保存值语义：
 *   - 空输入 → 不提交（避免把「没填」当成「设为 0」静默清空覆盖）；
 *   - 「跟随全局」→ 提交 0，清空覆盖；
 *   - 正数 → 覆盖值。
 */
function UserCapEditor({
  user,
  globalCap,
  saving,
  onSave,
}: {
  user: AdminUserRow;
  globalCap: number;
  saving: boolean;
  onSave: (cap: number) => void;
}) {
  const custom = user.balance_limit > 0;
  const effective = custom ? user.balance_limit : globalCap;
  const [draft, setDraft] = useState("");

  // 上限按**金额**填写（元/美元），提交时转微。空串 = 不提交（避免"没填"被当成"设为 0"）。
  const trimmed = draft.trim();
  const parsed = trimmed === "" ? null : microOf(trimmed);
  const invalid = trimmed !== "" && parsed === null;
  const valid = parsed !== null;
  // 上限低于当前余额不是错误（等价于「冻结后续充值」），但必须说清楚。
  const belowBalance = valid && parsed > 0 && parsed < user.balance;

  return (
    <div className="form-grid mt16">
      <Field
        label={`余额上限（${currentSymbol()}）`}
        htmlFor="user-cap"
        hint={
          invalid
            ? "金额格式不正确（只能填数字，最多 6 位小数）"
            : belowBalance
              ? `低于当前余额 ${moneyOf(user.balance)}，保存后该用户将无法继续充值或兑换`
              : effective > 0
                ? `当前生效 ${moneyOf(effective)}${custom ? "（单独设置）" : "（跟随全局）"}`
                : "当前不限（全局未设上限）"
        }
        hintWarn={invalid || belowBalance}
      >
        <Input
          id="user-cap"
          type="text"
          inputMode="decimal"
          placeholder={custom ? microToInput(user.balance_limit) : "留空表示不修改"}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
        />
        <div className="row-acts mt16">
          <button
            type="button"
            className="btn btn-sm btn-primary"
            disabled={!valid || saving}
            onClick={() => {
              onSave(parsed ?? 0);
              setDraft("");
            }}
          >
            保存上限
          </button>
          <button
            type="button"
            className="btn btn-sm"
            disabled={!custom || saving}
            onClick={() => {
              onSave(0);
              setDraft("");
            }}
          >
            跟随全局
          </button>
        </div>
      </Field>
      <Field label={`全局默认上限（${currentSymbol()}）`} htmlFor="global-cap">
        <Input
          id="global-cap"
          type="text"
          value={globalCap > 0 ? microToInput(globalCap) : "不限"}
          readOnly
        />
      </Field>
    </div>
  );
}

/** ② 余额流水（最近 5 条，与门户钱包流水同源） */
function UserTransactionsBlock({ userId }: { userId: number }) {
  const query = useAdminUserTransactionsQuery(userId, { page: 1, page_size: 5 });
  const items = query.data?.items ?? [];
  const total = query.data?.total ?? 0;
  if (query.isError) return <BlockError error={query.error} onRetry={() => void query.refetch()} />;
  if (!items.length)
    return <div className="muted small">{query.isPending ? "加载中…" : "暂无资金流水"}</div>;
  return (
    <>
      <DataTable columns={DETAIL_TX_COLUMNS} data={items} />
      <div className="muted small">
        共 {total} 条
        {total > items.length ? `，仅显示最近 ${items.length} 条` : ""}
      </div>
    </>
  );
}

/** ③ 密钥列表（只读；永不含明文） */
function UserKeysBlock({ userId }: { userId: number }) {
  const query = useAdminUserKeysQuery(userId);
  const items = query.data?.items ?? [];
  if (query.isError) return <BlockError error={query.error} onRetry={() => void query.refetch()} />;
  if (!items.length)
    return <div className="muted small">{query.isPending ? "加载中…" : "该用户暂无 API KEY"}</div>;
  return (
    <>
      <DataTable columns={DETAIL_KEY_COLUMNS} data={items} />
      <div className="muted small">管理台仅展示索引前缀，不提供明文回显</div>
    </>
  );
}

/** ④ 最近调用（复用管理端调用日志，按 user_id 过滤） */
function UserLogsBlock({ userId }: { userId: number }) {
  const query = useAdminLogsQuery({ user_id: userId, page: 1, page_size: 5 });
  const items = query.data?.items ?? [];
  const total = query.data?.total ?? 0;
  if (query.isError) return <BlockError error={query.error} onRetry={() => void query.refetch()} />;
  if (!items.length)
    return <div className="muted small">{query.isPending ? "加载中…" : "暂无调用记录"}</div>;
  return (
    <>
      <DataTable columns={DETAIL_LOG_COLUMNS} data={items} />
      <div className="muted small">
        共 {total} 条
        {total > items.length ? `，仅显示最近 ${items.length} 条` : ""}
      </div>
    </>
  );
}

/**
 * 用户管理（S2 / S17）
 * 检索 / 分组与状态筛选 / 分页；详情抽屉（基本信息 + 分组·角色变更 + 余额流水 / 密钥列表 / 最近调用）；
 * 额度调账抽屉（负数支持 + 实时预览 + 原因必填）；禁用·启用二次确认。
 */
export function AdminUsersPage() {
  // 管理台分组折扣来源必须是 /api/admin/config（已鉴权，含 group_discount 全表）。
  // 门户端点只下发「自己那一档」，用它做分组下拉会让管理台永远只有一个选项。
  const cfg = useAdminConfigQuery();
  const groupDiscounts = cfg.data?.config?.group_discount ?? { default: 1 };
  const groupKeys = Object.keys(groupDiscounts);
  // 应用选项与「插件」清单同源（/admin/apps/options），四个列表页共用一份缓存。
  const appOpts = useAdminAppOptionsQuery();
  const discountOf = (g: string) => groupDiscountText(groupDiscounts[g]);
  const [page, setPage] = useState(1);
  const [filters, setFilters] = useState<Record<string, string>>(EMPTY_FILTERS);
  const [detailUser, setDetailUser] = useState<AdminUserRow | null>(null);
  const [adjustUser, setAdjustUser] = useState<AdminUserRow | null>(null);
  const [adjustAmount, setAdjustAmount] = useState("");
  const [adjustReason, setAdjustReason] = useState("");

  const statusFilter = filters.fStatus ? Number(filters.fStatus) : undefined;
  const params: AdminUsersParams = {
    page,
    page_size: DEFAULT_PAGE_SIZE,
    keyword: filters.fKw.trim() || undefined,
    group: filters.fGroup || undefined,
    status: statusFilter,
    app_id: appIdParam(filters.fApp),
  };
  const query = useAdminUsersQuery(params);
  const users = query.data?.items ?? [];
  const total = query.data?.total ?? 0;

  const updateUser = useAdminUpdateUserMutation();
  const adjustQuota = useAdminAdjustQuotaMutation();

  /** 抽屉主体：优先取列表最新行（分组/状态变更后即时反映），筛选剔除时回落快照 */
  const selected = detailUser ? (users.find((u) => u.id === detailUser.id) ?? detailUser) : null;

  const onFilterChange = (id: string, value: string) => {
    setFilters((f) => ({ ...f, [id]: value }));
    setPage(1);
  };
  const onFilterReset = () => {
    setFilters(EMPTY_FILTERS);
    setPage(1);
  };

  const changeGroup = (user: AdminUserRow, group: string) => {
    const previous = user.group;
    updateUser.mutate(
      { id: user.id, body: { group } },
      {
        onSuccess: () =>
          toast(`分组已变更：${previous} → ${group}（${discountOf(group)}）`),
      },
    );
  };

  const changeRole = async (user: AdminUserRow) => {
    const toAdmin = user.role !== 0;
    const ok = await confirmDialog({
      title: "调整用户角色",
      body: toAdmin ? (
        <>
          确定将 <b>{user.username}</b> 升级为<b>管理员</b>？升级后将获得管理台全部权限。
        </>
      ) : (
        <>
          确定将 <b>{user.username}</b> 降级为<b>普通用户</b>？降级后将失去管理台访问权限。
        </>
      ),
      okText: "确认调整",
      okVariant: "primary",
    });
    if (!ok) return;
    updateUser.mutate(
      { id: user.id, body: { role: toAdmin ? 0 : 1 } },
      {
        onSuccess: () =>
          toast(`角色已调整：${user.username} → ${toAdmin ? "管理员" : "普通用户"}`),
      },
    );
  };

  const toggleStatus = async (user: AdminUserRow) => {
    const disable = user.status === 1;
    const ok = await confirmDialog({
      title: disable ? "禁用用户" : "启用用户",
      body: disable ? (
        <>
          确定禁用 <b>{user.username}</b>？其全部密钥立即拒绝请求（403），余额与配置保留。
        </>
      ) : (
        <>
          确定启用 <b>{user.username}</b>？其密钥立即恢复可用。
        </>
      ),
    });
    if (!ok) return;
    updateUser.mutate(
      { id: user.id, body: { status: disable ? 2 : 1 } },
      { onSuccess: () => toast(disable ? "用户已禁用" : "用户已启用") },
    );
  };

  const openAdjust = (user: AdminUserRow) => {
    setAdjustUser(user);
    setAdjustAmount("");
    setAdjustReason("");
  };

  const closeAdjust = () => setAdjustUser(null);

  /** 操作列：详情（S17 新增入口）+ 调账（沿用既有调账抽屉） */
  const columns: ColumnDef<AdminUserRow, unknown>[] = [
    ...baseColumns(discountOf),
    actionsCol<AdminUserRow>(
      (row) => [
        { icon: "eye", label: "详情", onClick: () => setDetailUser(row) },
        { icon: "wallet", label: "调账", onClick: () => openAdjust(row) },
        {
          icon: "power",
          label: row.status === 1 ? "禁用" : "启用",
          tone: row.status === 1 ? "danger" : undefined,
          onClick: () => void toggleStatus(row),
        },
      ],
      "操作",
      "acts",
    ),
  ];

  // 调账金额按「微」提交：sign 来自正负号，绝对值走 money.Parse 同款解析。
  const adjustMicro = microOf(adjustAmount.replace(/^[-+]/, ""));
  const adjustSigned =
    adjustMicro === null ? null : adjustAmount.trim().startsWith("-") ? -adjustMicro : adjustMicro;
  const hasAmount = adjustSigned !== null && adjustSigned !== 0;
  const previewBalance = adjustUser ? adjustUser.balance + (hasAmount ? adjustSigned : 0) : 0;

  const submitAdjust = () => {
    if (!adjustUser) return;
    const parsed = createAdjustQuotaSchema(adjustUser.balance).safeParse({
      amount: adjustAmount,
      reason: adjustReason,
    });
    if (!parsed.success) {
      toast.error(parsed.error.issues[0]?.message ?? "请检查输入");
      return;
    }
    const amount = adjustSigned ?? 0;
    adjustQuota.mutate(
      { id: adjustUser.id, body: { amount, reason: adjustReason.trim() } },
      {
        onSuccess: () => {
          toast(`已调账 ${moneyOf(amount)}，审计已记录`);
          closeAdjust();
        },
      },
    );
  };

  return (
    <>
      <PageHead title="用户管理" desc="余额调账、状态管控与密码重置" />

      <FilterBar
        fields={filterFields(groupKeys, appOpts.items)}
        value={filters}
        onChange={onFilterChange}
        onReset={onFilterReset}
        onAction={(id) => {
          if (id === "reset") onFilterReset();
        }}
      />

      <DataTable
        columns={columns}
        data={users}
        total={total}
        page={page}
        pageSize={DEFAULT_PAGE_SIZE}
        onPageChange={setPage}
      />

      <Drawer open={selected !== null} onClose={() => setDetailUser(null)} title="用户详情">
        {selected && (
          <>
            <div className="blk-title">① 基本信息</div>
            <div className="kv-grid">
              <div className="kv">
                <div className="k">ID / 用户名</div>
                <div className="v">
                  #{selected.id} {selected.username}
                </div>
              </div>
              <div className="kv">
                <div className="k">分组</div>
                <div className="v">
                  <Select
                    items={groupKeys.map((g): [string, string] => [g, `${g} · ${discountOf(g)}`])}
                    value={selected.group}
                    aria-label="分组"
                    onChange={(e) => changeGroup(selected, e.target.value)}
                  />
                </div>
              </div>
              <div className="kv">
                <div className="k">状态</div>
                <div className="v">
                  <StatusTag status={selected.status} />
                </div>
              </div>
              <div className="kv">
                <div className="k">角色</div>
                <div className="v">
                  <span className={cn("badge", selected.role === 0 ? "badge-purple" : "badge-gray")}>
                    {selected.role === 0 ? "管理员" : "普通用户"}
                  </span>{" "}
                  <button type="button" className="btn btn-sm btn-text" onClick={() => void changeRole(selected)}>
                    调整为{selected.role === 0 ? "普通用户" : "管理员"}
                  </button>
                </div>
              </div>
              <div className="kv">
                <div className="k">余额</div>
                <div className="v">
                  {moneyOf(selected.balance)}（≈ {otherMoneyOf(selected.balance, 2)}）
                </div>
              </div>
              <div className="kv">
                <div className="k">累计消耗</div>
                <div className="v">{moneyOf(selected.used)}</div>
              </div>
              <div className="kv">
                <div className="k">请求数</div>
                <div className="v">{selected.request_count.toLocaleString("en-US")}</div>
              </div>
            </div>
            {/* 用户级上限：入账三道闸（充值/兑换/调账）都按「用户级 > 0 优先」判定 */}
            <UserCapEditor
              key={selected.id}
              user={selected}
              globalCap={cfg.data?.config?.balance_limit ?? 0}
              saving={updateUser.isPending}
              onSave={(cap) =>
                updateUser.mutate(
                  { id: selected.id, body: { balance_limit: cap } },
                  {
                    onSuccess: () =>
                      toast(
                        cap > 0
                          ? `余额上限已设为 ${money2(cap)}`
                          : "已恢复为跟随全局上限",
                      ),
                  },
                )
              }
            />
            <div className="blk-title">② 余额流水</div>
            <UserTransactionsBlock userId={selected.id} />

            <div className="blk-title">③ 密钥列表</div>
            <UserKeysBlock userId={selected.id} />

            <div className="blk-title">④ 最近调用</div>
            <UserLogsBlock userId={selected.id} />
          </>
        )}
      </Drawer>

      <Dialog
        open={adjustUser !== null}
        onClose={closeAdjust}
        title="额度调账"
        actions={[
          { label: "取消", onClick: closeAdjust },
          {
            label: "确认调账",
            variant: "primary",
            onClick: submitAdjust,
            // 未填金额 / 未填原因 / 调后为负 时直接禁用：调账是一次不可撤销的余额写入，
            // 让按钮可点再弹 toast 等于把「能不能提交」的判断留给了运气。
            disabled: !hasAmount || !adjustReason.trim() || previewBalance < 0 || adjustQuota.isPending,
          },
        ]}
      >
        <Field label="用户" htmlFor="adj-user">
          <input
            id="adj-user"
            type="text"
            readOnly
            value={adjustUser ? `#${adjustUser.id} ${adjustUser.username}` : ""}
          />
        </Field>
        <Field label={`调整额度（${currentSymbol()}，正 = 增加，负 = 扣减）`} req htmlFor="adj-amt">
          {/* 填的是**金额**不是微：microOf 与后端 money.Parse 同规则。
              旧实现用 type=number + step=100000 + 「例如 +1000000」，
              等于按微示范、按元解析 —— 管理员照着填就是 1e12 微（100 万元）。 */}
          <input
            id="adj-amt"
            type="text"
            inputMode="decimal"
            placeholder="例如 100 或 -50.5"
            value={adjustAmount}
            onChange={(e) => setAdjustAmount(e.target.value)}
          />
        </Field>
        <Field label="原因（必填）" req htmlFor="adj-reason">
          <textarea
            id="adj-reason"
            rows={2}
            placeholder="例如：内测补偿"
            value={adjustReason}
            onChange={(e) => setAdjustReason(e.target.value)}
          />
        </Field>
        <Field
          label="调整后余额"
          hint={hasAmount && adjustUser ? `当前 ${moneyOf(adjustUser.balance)}` : undefined}
          hintWarn={hasAmount && previewBalance < 0}
        >
          <span className="mono">
            {hasAmount && adjustUser ? `${moneyOf(previewBalance)}` : "—"}
          </span>
        </Field>
      </Dialog>
    </>
  );
}

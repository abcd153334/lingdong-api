import { useMemo, useState } from "react";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { DataTable } from "@/components/common/DataTable";
import { Drawer } from "@/components/common/Drawer";
import { EmptyState } from "@/components/common/EmptyState";
import { Field } from "@/components/common/Field";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { PageHead } from "@/components/common/PageHead";
import { Select } from "@/components/common/Select";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Tag } from "@/components/common/StatusTag";
import { toastErr, toastOk } from "@/components/common/Toaster";
import {
  useAdminPackageCreateMutation,
  useAdminPackageDeleteMutation,
  useAdminPackageStatusMutation,
  useAdminPackageUpdateMutation,
  useAdminPackagesQuery,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { formatCent, formatNum } from "@/lib/format";
import {
  currentSymbol,
  microFromMinor,
  microOf,
  microToInput,
  minorOf,
  money2,
  moneyOf,
  packageCreditOf,
} from "@/lib/money";
import { actionsCol, monoCol, numCol, renderCol, statusCol } from "@/lib/table";
import type { AdminPackageItem, AdminPackagesParams, PackageBadge, PackagePayload } from "@/types";

/** 列表空筛选（受控 FilterBar 的初始值） */
const EMPTY_FILTERS: Record<string, string> = { fKw: "", fStatus: "", fSort: "" };

const FILTER_FIELDS: FilterField[] = [
  { t: "search", id: "fKw", label: "套餐", placeholder: "搜索套餐名称" },
  {
    t: "select",
    id: "fStatus",
    label: "状态",
    allLabel: "全部状态",
    items: [
      ["1", "启用"],
      ["0", "停用"],
    ],
  },
  {
    t: "select",
    id: "fSort",
    label: "排序",
    allLabel: "排序值 → 售价",
    items: [
      ["price_cny_cent:asc", "售价从低到高"],
      ["price_cny_cent:desc", "售价从高到低"],
      ["created_at:desc", "新建优先"],
    ],
  },
  { t: "btn", id: "btnNew", label: "新建套餐", variant: "primary", icon: "plus" },
];

/** 角标枚举 → 文案。三种共用同一视觉，只换文案（不新增 CSS 类）。 */
const BADGE_LABEL: Record<PackageBadge, string> = { recommend: "推荐", hot: "热销", value: "超值" };
const BADGE_OPTIONS: [PackageBadge | "", string][] = [
  ["", "无"],
  ["recommend", "推荐"],
  ["hot", "热销"],
  ["value", "超值"],
];

/** 额度 → `$x.xx`（两位小数：套餐是标品，$9.9999 与海报口径对不上） */
/** 表单状态（全部用字符串，便于 input 受控；提交时再转数值） */
interface FormState {
  name: string;
  priceYuan: string;
  /**
   * 赠送金额（金额文本，按结算币种填）。
   *
   * 用文本而不是浮点数：`1500096` 微（¥1.500096）走一遍 `Number()` 会变成
   * 二进制不精确的 1.500096，只改个套餐名再保存，到账金额就少几个微，
   * 而界面上看不出任何差异。
   */
  bonus: string;
  sortOrder: string;
  badge: PackageBadge | "";
  /** 仅创建时生效：停用状态创建的套餐不会出现在门户 */
  enabled: boolean;
}

const EMPTY_FORM: FormState = {
  name: "",
  priceYuan: "",
  bonus: "",
  sortOrder: "",
  badge: "",
  enabled: true,
};

interface FormErrors {
  name?: string;
  price?: string;
  bonus?: string;
  sort?: string;
}

/** 充值套餐（后台配置）：列表 + 新建/编辑抽屉 + 启停 + 删除保护。
 *
 * 口径说明：`base` / `credited` 都由**服务端**按 `price_cent`（实付，渠道分）
 * 复算后下发 —— 到账 = 实付分→微 + `bonus`（赠送，微），没有倍率、没有刻度。
 * 页面只做展示与表单预览；预览必须走同一个 `base + bonus` 公式，
 * 否则差 1 分运营会当成 bug。 */
export function AdminPackagesPage() {
  const [filters, setFilters] = useState<Record<string, string>>(EMPTY_FILTERS);
  const [page, setPage] = useState(1);

  const params: AdminPackagesParams = useMemo(
    () => ({
      page,
      page_size: DEFAULT_PAGE_SIZE,
      keyword: filters.fKw || undefined,
      enabled: (filters.fStatus as "" | "0" | "1") || undefined,
      sort: filters.fSort || undefined,
    }),
    [page, filters],
  );

  const list = useAdminPackagesQuery(params);
  const createMut = useAdminPackageCreateMutation();
  const updateMut = useAdminPackageUpdateMutation();
  const statusMut = useAdminPackageStatusMutation();
  const deleteMut = useAdminPackageDeleteMutation();

  const rows = list.data?.items ?? [];
  const total = list.data?.total ?? 0;
  const cap = list.data?.balance_limit ?? 0;
  /* 到账 = 售价（分→微）+ 赠送金额（微）。没有倍率、没有刻度：
     这两个乘数曾经都存在，而它们的乘积决定了用户到手多少，
     运营永远算不出"我标 ¥10 到底给多少"，预览与实际也各自漂移。 */
  const maxPriceCent = list.data?.max_price_cent ?? 10_000_000;
  const maxPriceMoney = (maxPriceCent / 100).toFixed(2);

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<AdminPackageItem | null>(null);
  const [form, setForm] = useState<FormState>(EMPTY_FORM);
  const [errors, setErrors] = useState<FormErrors>({});
  const isPending = createMut.isPending || updateMut.isPending;

  /* ---------- 表单 ---------- */

  const openCreate = () => {
    setEditing(null);
    setForm(EMPTY_FORM);
    setErrors({});
    setFormOpen(true);
  };

  const openEdit = (row: AdminPackageItem) => {
    setEditing(row);
    setForm({
      name: row.name,
      priceYuan: (row.price_cent / 100).toFixed(2),
      // 赠送金额按用户填的原文回显（microToInput 去掉无意义的尾随 0）：
      // 走一遍"微 → 两位小数 → 微"会丢精度，而丢的是用户真金白银买到的东西。
      bonus: microToInput(row.bonus),
      sortOrder: String(row.sort_order),
      badge: row.badge,
      enabled: row.enabled,
    });
    setErrors({});
    setFormOpen(true);
  };

  const closeForm = () => {
    setFormOpen(false);
    setEditing(null);
  };

  /** 预览：与后端一致 —— 到账 = 售价（分→微）+ 赠送（微） */
  const preview = useMemo(() => {
    // 必须走十进制定点解析：`Number(text) * 100` 在 9.995 这类输入上会被浮点误差
    // 拉向错误的一侧，而且无法表达「精度超过分」的非法输入。
    const cents = minorOf(form.priceYuan) ?? 0;
    const bonus = microOf(form.bonus) ?? 0;
    // 走 money.ts 的唯一算法：页面里手写 `×100×10000` 会在小数金额上差 1 分。
    const { base, credit } = packageCreditOf(cents, bonus);
    return { cents, bonus, base, credit };
  }, [form.priceYuan, form.bonus]);

  const overCap = cap > 0 && preview.credit > cap;

  const validate = (): boolean => {
    const next: FormErrors = {};
    const name = form.name.trim();
    if (!name) next.name = "请填写套餐名称";
    else if ([...name].length > 32) next.name = "名称不能超过 32 个字符";
    if (form.priceYuan && minorOf(form.priceYuan) === null) {
      // 精度超过「分」的输入必须明确拒绝：静默取整会让运营以为卖了 9.999 元。
      next.price = "售价最多两位小数（金额以分为单位记账）";
    } else if (!form.priceYuan || preview.cents < 100) next.price = `售价不能低于 ${currentSymbol()}1`;
    else if (preview.cents > maxPriceCent) {
      next.price = `售价不能高于 ${currentSymbol()}${formatNum(Number(maxPriceMoney), 2)}`;
    }
    if (form.bonus.trim() !== "" && microOf(form.bonus) === null) {
      next.bonus = "赠送金额格式不正确（只能填数字，最多 6 位小数）";
    }
    if (form.sortOrder !== "" && (Number(form.sortOrder) < 0 || Number(form.sortOrder) > 999_999)) {
      next.sort = "排序值需在 0 – 999,999 之间";
    }
    setErrors(next);
    return Object.keys(next).length === 0;
  };

  const submitForm = () => {
    if (!validate()) return;
    const payload: PackagePayload = {
      name: form.name.trim(),
      price_cent: preview.cents,
      bonus: preview.bonus,
      badge: form.badge,
      enabled: form.enabled,
    };
    // 排序留空表示「追加到末尾」：不能传 0（0 是显式置顶）。
    if (form.sortOrder !== "") payload.sort_order = Number(form.sortOrder);

    const onSuccess = (res: { warning: { message: string } | null }) => {
      closeForm();
      if (res.warning) toastErr(res.warning.message);
      else toastOk(editing ? "已保存" : "已创建");
    };
    if (editing) {
      updateMut.mutate({ id: editing.id, body: payload }, { onSuccess, onError: onFail });
    } else {
      createMut.mutate(payload, { onSuccess, onError: onFail });
    }
  };

  const onFail = (e: unknown) => {
    toastErr(e instanceof ApiError ? e.message : "保存失败，请稍后重试");
  };

  /* ---------- 行操作 ---------- */

  const toggleStatus = (row: AdminPackageItem) => {
    statusMut.mutate(
      { id: row.id, enabled: !row.enabled },
      {
        onSuccess: () =>
          toastOk(
            row.enabled
              ? `${row.name} 已停用：门户立即不再展示，在途订单不受影响`
              : `${row.name} 已启用`,
          ),
        onError: onFail,
      },
    );
  };

  const remove = async (row: AdminPackageItem) => {
    if (row.order_count > 0) {
      toastErr(`该套餐已被 ${row.order_count} 笔订单引用，请改为「停用」`);
      return;
    }
    const ok = await confirmDialog({
      title: "删除套餐",
      body: (
        <div>
          <p>
            确认删除<b>「{row.name}」</b>？
          </p>
          <p className="muted small">
            删除为软删除：历史订单仍可追溯到该套餐名称，同名套餐可再次创建。
          </p>
        </div>
      ),
      okText: "删除",
      okVariant: "danger",
    });
    if (!ok) return;
    deleteMut.mutate(row.id, { onSuccess: () => toastOk("已删除"), onError: onFail });
  };

  /* ---------- 列 ---------- */

  /* 套餐**没有**自己的币种列：结算币种全站唯一，套餐价必然属于该币种
     （order.Packages 不按币种过滤）。这里只取来格式化表里的售价。 */
  const listCurrency = list.data?.currency;

  const columns = [
    numCol<AdminPackageItem>((r) => r.sort_order, {
      label: "排序",
      id: "sort_order",
      priority: 3,
    }),
    renderCol<AdminPackageItem>("name", "名称", (r) => r.name, (r) => (
      <>
        <b>{r.name}</b>
        {r.badge ? (
          <>
            {" "}
            <Tag tone="primary">{BADGE_LABEL[r.badge]}</Tag>
          </>
        ) : null}
      </>
    ), { minWidth: 160 }),
    renderCol<AdminPackageItem>("price_cent", "售价", (r) => r.price_cent, (r) => (
      <span className="mono">{formatCent(r.price_cent, listCurrency)}</span>
    ), { align: "right" }),
    monoCol<AdminPackageItem>("base", "基础到账", (r) => money2(r.base), { priority: 3 }),
    monoCol<AdminPackageItem>("bonus", "赠送", (r) => money2(r.bonus), { priority: 3 }),
    renderCol<AdminPackageItem>("credited", "到账合计", (r) => r.credited, (r) => (
      <>
        <span className="mono">{money2(r.credited)}</span>
        {cap > 0 && r.credited > cap ? (
          <>
            {" "}
            <Tag tone="danger">超上限</Tag>
          </>
        ) : null}
      </>
    ), { align: "right" }),
    numCol<AdminPackageItem>((r) => r.order_count, {
      label: "订单数",
      id: "order_count",
      priority: 2,
    }),
    statusCol<AdminPackageItem>((r) => (r.enabled ? "1" : "0"), { label: "状态", id: "enabled" }),
    actionsCol<AdminPackageItem>((row) => [
      { icon: "edit", label: "编辑", onClick: () => openEdit(row) },
      {
        icon: "power",
        label: row.enabled ? "停用" : "启用",
        busy: statusMut.isPending,
        onClick: () => toggleStatus(row),
      },
      {
        icon: "trash",
        label: "删除",
        tone: "danger",
        disabled: row.order_count > 0 || deleteMut.isPending,
        disabledReason: `已被 ${row.order_count} 笔订单引用，只能停用`,
        onClick: () => void remove(row),
      },
    ]),
  ];

  return (
    <>
      <PageHead title="充值套餐" desc="门户收银台展示的固定档位商品" />

      {/* 不可关闭的口径说明：到账金额由售价与赠送直接相加决定 —— 运营必须知道这件事 */}
      <div className="alert-banner info">
        <div>
          到账金额 = <b>售价</b> ＋ <b>赠送金额</b>（同一币种，无倍率、无额度刻度，所见即所得）；
          {/* cap = 0 是「不设上限」而不是「上限为 0」：写成 money2(0) 会让运营以为
              所有套餐都超限（而系统实际不拦），进而去"修"一个不存在的配置。 */}
          {cap > 0 ? (
            <>
              本页 <b>充值上限 {money2(cap)}</b>，到账超过上限的套餐客户无法购买。
            </>
          ) : (
            <>当前<b>未设置账户余额上限</b>，任何到账金额的套餐客户都能购买。</>
          )}
        </div>
      </div>

      <FilterBar
        fields={FILTER_FIELDS}
        value={filters}
        onChange={(id, value) => {
          setFilters((f) => ({ ...f, [id]: value }));
          setPage(1); // 条件变了页码必须归位，否则会停在「第 3 页但只有 1 页数据」的空表
        }}
        onReset={() => {
          setFilters(EMPTY_FILTERS);
          setPage(1);
        }}
        onAction={(id) => {
          if (id === "btnNew") openCreate();
        }}
      />

      <DataTable
        columns={columns}
        data={rows}
        total={total}
        page={page}
        pageSize={DEFAULT_PAGE_SIZE}
        onPageChange={setPage}
        empty={
          // 三态必须分开：加载中 / 加载失败 / 真的没有套餐（引导新建）。
          // 把「空」画成「失败」会让运营以为系统坏了。
          <EmptyState
            variant={list.isPending ? "loading" : list.isError ? "error" : "empty"}
            title={list.isPending ? "加载中…" : list.isError ? "套餐列表加载失败" : "暂无充值套餐"}
            desc={
              list.isError
                ? list.error instanceof ApiError
                  ? list.error.message
                  : "请稍后重试"
                : list.isPending
                  ? undefined
                  : "新建套餐后，门户收银台会按排序值展示它们。"
            }
          />
        }
      />

      <Drawer
        open={formOpen}
        onClose={closeForm}
        title={editing ? `编辑套餐 · ${editing.name}` : "新建套餐"}
        actions={[
          { label: "取消", onClick: closeForm },
          { label: "保存", variant: "primary", disabled: isPending, onClick: submitForm },
        ]}
      >
        <Field
          label="名称"
          req
          hint={errors.name ?? "1–32 个字符；不能包含 % 或 emoji，不能以标点结尾（名称会进入支付标题）"}
          hintWarn={!!errors.name}
        >
          <input
            type="text"
            maxLength={32}
            value={form.name}
            aria-label="套餐名称"
            placeholder="例如：标准包"
            onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
          />
        </Field>

        <Field
          label={`售价（${currentSymbol()}）`}
          req
          hint={
            errors.price ??
            `${currentSymbol()}1 – ${currentSymbol()}${formatNum(Number(maxPriceMoney), 2)}；与「充值上限」无关`
          }
          hintWarn={!!errors.price}
        >
          <input
            type="number"
            step="0.01"
            min={1}
            max={Number(maxPriceMoney)}
            value={form.priceYuan}
            aria-label="售价"
            placeholder="216"
            onChange={(e) => setForm((f) => ({ ...f, priceYuan: e.target.value }))}
          />
        </Field>

        <Field
          label="赠送金额"
          hint={errors.bonus ?? `按${currentSymbol()}填金额，当前 ${money2(preview.bonus)}，直接计入到账`}
          hintWarn={!!errors.bonus}
        >
          <input
            type="text"
            inputMode="decimal"
            value={form.bonus}
            aria-label="赠送金额"
            placeholder="留空即无赠送"
            onChange={(e) => setForm((f) => ({ ...f, bonus: e.target.value }))}
          />
        </Field>

        <Field
          label="排序"
          hint={errors.sort ?? "越小越靠前；0 即置顶；留空则自动追加到末尾"}
          hintWarn={!!errors.sort}
        >
          <input
            type="number"
            min={0}
            max={999999}
            value={form.sortOrder}
            aria-label="排序"
            placeholder="留空自动追加到末尾"
            onChange={(e) => setForm((f) => ({ ...f, sortOrder: e.target.value }))}
          />
        </Field>

        <Field label="角标" hint="三种角标共用同一视觉，只换文案">
          <Select
            aria-label="角标"
            value={form.badge}
            items={BADGE_OPTIONS}
            onChange={(e) => setForm((f) => ({ ...f, badge: e.target.value as PackageBadge | "" }))}
          />
        </Field>

        {!editing ? (
          <SwitchRow
            label="创建后立即启用"
            tip="停用状态创建后不出现在门户，可稍后再启用"
            checked={form.enabled}
            onChange={(v: boolean) => setForm((f) => ({ ...f, enabled: v }))}
          />
        ) : null}

        <div className={`alert-banner ${overCap ? "warn" : "info"}`}>
          <div>
            基础 <b className="mono">{money2(preview.base)}</b> ＋ 赠送{" "}
            <b className="mono">{money2(preview.bonus)}</b> ＝ 到账{" "}
            <b className="mono">{money2(preview.credit)}</b>
            {/* 这里的措辞必须与后端一致：到账 = 售价(分→微) + 赠送(微)。
                旧文案写「售价 × 充值倍率 × 额度刻度」——那套乘数早已删除，
                运营照它核对永远对不上账。 */}
            <div className="hint">基础额度 = 售价（分 → 微），到账 = 基础额度 + 赠送金额</div>
            {overCap ? (
              <div>
                到账 {money2(preview.credit)} 超过当前充值上限 {money2(cap)}
                ，客户将无法购买；仍可保存，请先调整上限。
              </div>
            ) : null}
          </div>
        </div>
      </Drawer>
    </>
  );
}

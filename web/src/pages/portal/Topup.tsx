import { useEffect, useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useSearch } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { toast } from "sonner";
import { Card } from "@/components/common/Card";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { DataTable } from "@/components/common/DataTable";
import { Dialog } from "@/components/common/Dialog";
import { Drawer } from "@/components/common/Drawer";
import { EmptyState } from "@/components/common/EmptyState";
import { Field } from "@/components/common/Field";
import { Icon } from "@/components/common/Icon";
import { MoneyText } from "@/components/common/MoneyText";
import { PageHead } from "@/components/common/PageHead";
import { PayMethodIcon } from "@/components/common/PayMethodIcon";
import { PayQRCode } from "@/components/portal/PayQRCode";
import { readLastMethod, writeLastMethod } from "@/lib/payChannel";
import { Select } from "@/components/common/Select";
import { Tabs } from "@/components/common/Tabs";
import { useLooseNavigate, usePageTabs, type PageTab } from "@/lib/pageTabs";
import {
  queryKeys,
  useAffTransferMutation,
  useContinueOrderMutation,
  useMeQuery,
  useOrderQRCodeQuery,
  useOrderVerifyMutation,
  usePortalAffQuery,
  usePortalAffRecordsQuery,
  usePortalConfigQuery,
  usePortalOrdersQuery,
  usePortalPackagesQuery,
  usePortalTransactionsQuery,
  useRedeemMutation,
  useRetryOrderPayMutation,
  useTopupMutation,
  type TopupBody,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import {
  TRANSACTION_LABELS,
  formatCent,
  formatDateTime,
  PACKAGE_BADGE_LABELS,
} from "@/lib/format";
import { orderColumns } from "@/lib/table";
import { useSite } from "@/lib/site";
import { copyText, copyToast } from "@/lib/clipboard";
import {
  currentSymbol,
  getMoneyCtx,
  groupDiscountText,
  microFromMinor,
  microOf,
  microToInput,
  minorFromMicro,
  money2,
  moneyOf,
  otherMoneyOf,
  ratioPercent,
} from "@/lib/money";
import type { AffRecord, OrderItem, TopupResult, TransactionItem, WalletPackage } from "@/types";

/* ---------------------- 常量与助手 ---------------------- */

const ORDER_STATUS_ITEMS: [string, string][] = [
  ["pending", "待支付"],
  ["paid", "已支付"],
  ["expired", "已过期"],
  ["refunding", "退款中"],
  ["refunded", "已退款"],
  ["need_manual", "人工处理"],
];

/** 收银台 UI 倒计时：与后端 pending 订单 TTL 一致（5 分钟） */
const PAY_COUNTDOWN = 300;

function toastError(error: unknown, fallback = "操作失败，请稍后重试"): void {
  toast.error(error instanceof ApiError ? error.message : fallback);
}

const TX_COLUMNS: ColumnDef<TransactionItem, unknown>[] = [
  {
    accessorKey: "created_at",
    header: "时间",
    meta: { label: "时间" },
    cell: ({ row }) => (
      <span className="mono small muted">{formatDateTime(row.original.created_at)}</span>
    ),
  },
  {
    accessorKey: "type",
    header: "类型",
    meta: { label: "类型" },
    cell: ({ row }) => (
      <span className="small">{TRANSACTION_LABELS[row.original.type] ?? row.original.type}</span>
    ),
  },
  {
    accessorKey: "amount",
    header: "变动",
    meta: { label: "变动" },
    cell: ({ row }) => <MoneyText micro={row.original.amount} signed />,
  },
  {
    accessorKey: "balance_after",
    header: "余额",
    meta: { label: "余额" },
    cell: ({ row }) => <MoneyText micro={row.original.balance_after} />,
  },
  {
    accessorKey: "reason",
    header: "说明",
    meta: { label: "说明", wrap: true },
    cell: ({ row }) => <span className="small muted">{row.original.reason || "—"}</span>,
  },
];

/** 返利行状态的中文标签（状态由服务端算好，前端不碰时钟）。 */
const AFF_STATE_LABEL: Record<AffRecord["state"], string> = {
  cooling: "冷却中",
  available: "可转出",
  transferred: "已转出",
  voided: "已退款作废",
};

/** 返利行身份的中文标签：一笔钱是「我邀请别人」还是「我被邀请」拿到的。 */
const AFF_ROLE_LABEL: Record<AffRecord["role"], string> = {
  inviter: "我邀请的好友",
  invitee: "我的邀请人",
};

const AFF_RECORD_COLUMNS: ColumnDef<AffRecord, unknown>[] = [
  {
    accessorKey: "role",
    header: "来源",
    meta: { label: "来源" },
    cell: ({ row }) => (
      <span className="small">{AFF_ROLE_LABEL[row.original.role] ?? "—"}</span>
    ),
  },
  {
    accessorKey: "counterpart",
    header: "对方",
    meta: { label: "对方" },
    cell: ({ row }) => <span className="mono small">{row.original.counterpart}</span>,
  },
  {
    accessorKey: "base_amount",
    header: "本单实付",
    meta: { label: "本单实付" },
    cell: ({ row }) => <MoneyText micro={row.original.base_amount} />,
  },
  {
    accessorKey: "reward",
    header: "我的返利",
    meta: { label: "我的返利" },
    cell: ({ row }) => <MoneyText micro={row.original.reward} />,
  },
  {
    accessorKey: "state",
    header: "状态",
    meta: { label: "状态" },
    cell: ({ row }) => (
      <span className="small muted">{AFF_STATE_LABEL[row.original.state] ?? "—"}</span>
    ),
  },
  {
    accessorKey: "created_at",
    header: "时间",
    meta: { label: "时间" },
    cell: ({ row }) => (
      <span className="mono small muted">{formatDateTime(row.original.created_at)}</span>
    ),
  },
];

/* ---------------------- 收银台状态 ---------------------- */

/** 待确认的充值意图（确认阶段） */
interface Draft {
  source: string;
  /** 套餐的基础 / 赠送拆分；自定义与续付无拆分，仅给 credit */
  base?: number;
  bonus?: number;
  credit?: number;
  /** 应付金额（分，支付渠道最小单位） */
  amountCent: number;
  body: TopupBody;
}

/** 已下单、待支付（支付阶段） */
interface PayState {
  source: string;
  base?: number;
  bonus?: number;
  credit: number;
  result: TopupResult;
}

/* ---------------------- 页面 ---------------------- */

type MainTab = "pkg" | "redeem";
type RecTab = "tx" | "orders";

const MAIN_TABS: readonly PageTab<MainTab>[] = [
  { key: "pkg", label: "充值" },
  { key: "redeem", label: "兑换码" },
];

const REC_TABS: readonly PageTab<RecTab>[] = [
  { key: "tx", label: "资金流水" },
  { key: "orders", label: "充值订单" },
];

export function TopupPage() {
  const queryClient = useQueryClient();
  const meQuery = useMeQuery();
  const pkgQuery = usePortalPackagesQuery();
  const affQuery = usePortalAffQuery();

  const cfg = pkgQuery.data;
  const me = meQuery.data;
  const aff = affQuery.data;
  const site = useSite();
  /** 充值开关与最低金额以 /portal/config 为唯一来源（/portal/packages 仅提供套餐数据） */
  const portalCfg = usePortalConfigQuery().data;
  const customTopupEnabled = portalCfg?.custom_topup_enabled ?? false;
  const minTopup = portalCfg?.min_topup ?? 0;
  // 分组折扣只取自己的单值（公开端点已裁剪整张折扣表）。
  const myDiscount = groupDiscountText(me?.group_discount);

  /* ---------- 页签：?tab=pkg|redeem + ?rec=tx|orders（第二组必须命名，避免撞 tab） ----------
     #redeem 兼容：首帧同步读 hash 决定 fallback（不能只靠 effect 纠偏，否则会先闪一下「充值」），
     再由下面的 effect 一次性把 fragment 收敛成 ?tab=redeem 并清掉 hash。
     Home 的两处 hash="redeem" 入口与 Settings 的 /topup#redeem 都靠这一处收口。 */
  const initialMain: MainTab = window.location.hash === "#redeem" ? "redeem" : "pkg";
  const mainTab = usePageTabs(MAIN_TABS, initialMain);
  const recTab = usePageTabs(REC_TABS, "tx", { param: "rec" });
  const nav = useLooseNavigate();
  const search = useSearch({ strict: false }) as Record<string, unknown>;
  useEffect(() => {
    if (window.location.hash !== "#redeem") return;
    const next: Record<string, unknown> = { ...search };
    if (next.tab === undefined) next.tab = "redeem";
    void nav({ search: () => next, hash: "", replace: true, resetScroll: false });
    // 只跑一次；StrictMode 二次进入时 hash 已清空，天然幂等
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const [customAmt, setCustomAmt] = useState("");
  const [redeemCode, setRedeemCode] = useState("");
  const [orderStatus, setOrderStatus] = useState("");
  const [orderPage, setOrderPage] = useState(1);
  const [txPage, setTxPage] = useState(1);
  const [txType, setTxType] = useState("");
  const [affDrawer, setAffDrawer] = useState(false);
  const [affPage, setAffPage] = useState(1);

  const topupMut = useTopupMutation();
  const continueMut = useContinueOrderMutation();
  const redeemMut = useRedeemMutation();
  const transferMut = useAffTransferMutation();
  const verifyMut = useOrderVerifyMutation();
  const retryMut = useRetryOrderPayMutation();

  const ordersQuery = usePortalOrdersQuery({ page: orderPage, page_size: DEFAULT_PAGE_SIZE, status: orderStatus });
  const txQuery = usePortalTransactionsQuery({ page: txPage, page_size: DEFAULT_PAGE_SIZE, type: txType });
  const affRecordsQuery = usePortalAffRecordsQuery({ page: affPage, page_size: DEFAULT_PAGE_SIZE });

  const orders = ordersQuery.data;
  const txs = txQuery.data;
  const payOptions = cfg?.pay_options ?? [];

  /* ---- 收银台 ---- */
  const [draft, setDraft] = useState<Draft | null>(null);
  const [pay, setPay] = useState<PayState | null>(null);
  const [done, setDone] = useState<{ quota: number; outTradeNo: string } | null>(null);
  const [method, setMethod] = useState("");
  const [left, setLeft] = useState(PAY_COUNTDOWN);

  const invalidateWallet = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    void queryClient.invalidateQueries({ queryKey: queryKeys.portalOrdersAll });
    void queryClient.invalidateQueries({ queryKey: queryKeys.portalTransactionsAll });
    void queryClient.invalidateQueries({ queryKey: queryKeys.portalAff });
  };

  const closeCashier = () => {
    setDraft(null);
    setPay(null);
    setDone(null);
  };

  const openCashier = (d: Draft) => {
    if (!payOptions.length) {
      toast.error("管理员未启用任何支付方式，请联系管理员");
      return;
    }
    setDraft(d);
    setPay(null);
    setDone(null);
    // 记住上次用的方式：老用户第二次充值不用再点一次。
    // 必须先确认它还可用 —— 运营可能刚把那种方式停用，否则会提交一个已失效的选项。
    const remembered = readLastMethod();
    const hit = payOptions.find((o) => o.method === remembered);
    setMethod(hit ? hit.method : payOptions[0].method);
  };

  const submitPay = () => {
    if (!draft) return;
    const opt = payOptions.find((o) => o.method === method);
    topupMut.mutate(
      // 只回传 method：承接渠道由服务端按方式路由（前端不持有 channel_id）。
      { ...draft.body, method: opt?.method },
      {
        onSuccess: (res) => {
          // 运营改价的瞬间，用户可能停在「确认订单」上看的是旧价。
          // 金额一律以服务端为准（不会多扣），但必须说清楚，否则观感是「偷偷涨价」。
          if (res.amount_cent !== draft.amountCent) {
            toast.warning(`该套餐价格已更新为 ${formatCent(res.amount_cent, res.currency)}，请确认后支付`);
          }
          // 到账金额也要比对：运营改了套餐赠送额，同一价格到账就会变。
          // 用户看的是「到账 X」结果只到账 Y，等于悄悄缩水。
          if (draft.credit != null && res.credited !== draft.credit) {
            toast.warning(
              `到账金额已调整：由 ${money2(draft.credit)} 变为 ${money2(res.credited)}，请确认后支付`,
            );
          }
          setDraft(null);
          setPay({
            source: draft.source,
            credit: res.credited,
            result: res,
          });
        },
        onError: (e) => toastError(e, "下单失败，请稍后重试"),
      },
    );
  };

  const continueOrder = (o: OrderItem) => {
    continueMut.mutate(o.id, {
      onSuccess: (res) => {
        setDone(null);
        setPay({
          source: `续付订单 ${o.out_trade_no}`,
          credit: res.credited,
          result: res,
        });
      },
      onError: (e) => toastError(e, "续付失败，请稍后重试"),
    });
  };

  /**
   * 主动核对支付结果。
   *
   * 比等 5s 轮询更快，也用于「我已完成支付」：查单失败只提示「暂时无法查询」，
   * 绝不提示「支付失败」——查不到不等于没付，误导会让用户重复付款。
   */
  const verifyPaid = () => {
    if (!pay) return;
    verifyMut.mutate(pay.result.id, {
      onSuccess: (r) => {
        if (r.order.status === "paid") {
          setDone({ quota: r.order.credited, outTradeNo: r.order.out_trade_no });
          invalidateWallet();
          return;
        }
        if (r.query_ok) {
          toast.info("暂未查询到支付结果，如已完成支付请稍等 1 分钟");
        } else {
          toast.warning("暂时无法查询支付状态，请稍后再试");
        }
      },
      onError: (e) => toastError(e, "核对失败，请稍后重试"),
    });
  };

  /**
   * 重试创建支付：复用**同一订单号**。
   *
   * 绝不能改成「重新下单」——新单号会让上游存在两笔订单，用户付了新的又付旧的，双倍扣款。
   */
  const retryPay = () => {
    if (!pay) return;
    retryMut.mutate(pay.result.id, {
      onSuccess: (res) => {
        setPay((p) => (p ? { ...p, result: res } : p));
        setLeft(res.expires_in > 0 ? res.expires_in : PAY_COUNTDOWN);
      },
      onError: (e) => toastError(e, "重试失败，请稍后再试"),
    });
  };

  /* 支付检测：待支付期间 5s 轮询本人订单，命中同一单号且已支付即完成 */
  const pollQuery = usePortalOrdersQuery(
    { page: 1, page_size: DEFAULT_PAGE_SIZE },
    { enabled: pay !== null && done === null, refetchInterval: 5000 },
  );

  useEffect(() => {
    if (!pay || done) return;
    const hit = pollQuery.data?.items.find((o) => o.id === pay.result.id && o.status === "paid");
    if (!hit) return;
    setDone({ quota: hit.credited, outTradeNo: hit.out_trade_no });
    void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    void queryClient.invalidateQueries({ queryKey: queryKeys.portalOrdersAll });
    void queryClient.invalidateQueries({ queryKey: queryKeys.portalTransactionsAll });
  }, [pollQuery.data, pay, done, queryClient]);

  useEffect(() => {
    if (!pay || done) return;
    // 用服务端下发的剩余秒数起算，**不用本地时钟减 expire_at**：
    // 用户本机时间快 10 分钟就会一进页面就判定超时。
    setLeft(pay.result.expires_in > 0 ? pay.result.expires_in : PAY_COUNTDOWN);
    const timer = window.setInterval(() => setLeft((v) => v - 1), 1000);
    return () => window.clearInterval(timer);
  }, [pay, done]);

  useEffect(() => {
    if (left > 0 || !pay || done) return;
    closeCashier();
    toast.warning("订单超时未支付，已关闭");
    void queryClient.invalidateQueries({ queryKey: queryKeys.portalOrdersAll });
  }, [left, pay, done, queryClient]);

  /* ---- 自定义充值换算 ----
     到账金额 **就是** 用户付的钱：不存在充值倍率，也不存在额度刻度。
     唯一要做的是把「金额」落到支付渠道的最小单位（分）上，再转回微 ——
     这条路径同时保证"预览到的到账 = 实际到账"，两端用同一个整数。 */
  const amtMicro = microOf(customAmt);
  const amtCent = amtMicro === null ? null : minorFromMicro(amtMicro);
  const amtValid = amtMicro !== null && amtCent !== null && amtMicro > 0;
  /** 到账金额（微）；与订单金额同源 */
  const previewCredit = amtCent === null ? 0 : (microFromMinor(amtCent) ?? 0);
  /* 余额上限 = 0 表示**不限制**（conf.BalanceLimitFor 的口径：两层都没配就是 0）。
     旧实现直接 `balance + 到账 > 0` 判定，于是「不设上限」的站把每一次充值都判成超限，
     自定义充值按钮永远点不动 —— 这是最坏的一类缺陷：功能看起来还在，只是永远失败。 */
  const balanceCap = cfg?.balance_limit ?? 0;
  const overLimit = !!me && balanceCap > 0 && me.balance + previewCredit > balanceCap;
  const minTopupMicro = minTopup;

  const submitCustom = () => {
    if (!cfg) return;
    if (amtCent === null) {
      toast.error("充值金额最多保留 2 位小数（支付渠道以分为最小单位）");
      return;
    }
    if (!amtValid || amtMicro < minTopupMicro) {
      toast.error(`单次充值金额不得低于 ${money2(minTopupMicro)}`);
      return;
    }
    if (overLimit) {
      toast.error("本次充值后将超过账户余额上限，请调整金额或先消费");
      return;
    }
    openCashier({
      source: "自定义充值",
      credit: previewCredit,
      amountCent: amtCent,
      // 后端 `topupReq.Amount` 是**十进制字符串**（money.Parse）：传 number 会 400，
      // 传微会放大 1e6 倍。这里从「分」反推十进制串，保证「预览到账 = 实际下单金额」。
      body: { amount: microToInput(previewCredit) },
    });
  };

  /* ---- 兑换码 ---- */
  const submitRedeem = () => {
    // 回车路径不走按钮的 disabled：不在这里挡住，连按回车会并发核销同一个码，
    // 后端第二笔必然撞「已使用」，用户看到一条刺眼的失败提示（额度其实已经到账）。
    if (redeemMut.isPending) return;
    const code = redeemCode.trim().toUpperCase();
    if (!code) {
      toast.error("请输入兑换码");
      return;
    }
    redeemMut.mutate(code, {
      onSuccess: (res) => {
        toast.success(`兑换成功，已入账 ${moneyOf(res.amount)}`);
        setRedeemCode("");
        void queryClient.invalidateQueries({ queryKey: queryKeys.me });
        void queryClient.invalidateQueries({ queryKey: queryKeys.portalTransactionsAll });
      },
      onError: (e) => toastError(e, "兑换失败"),
    });
  };

  /* ---- 邀请奖励转入余额 ---- */
  const transferAff = async () => {
    if (!aff) return;
    const ok = await confirmDialog({
      title: "邀请奖励转入余额",
      body: (
        <>
          将可转出返利 <b className="mono">{moneyOf(aff.aff_available)}</b>（≈{" "}
          {otherMoneyOf(aff.aff_available, 2)}）转入账户余额，累计返利不受影响。
          {aff.aff_cooling > 0 ? (
            <>
              {" "}
              另有 {moneyOf(aff.aff_cooling)} 冷却中，不在本次转入内。
            </>
          ) : null}
        </>
      ),
      okText: "确认转入",
    });
    if (!ok) return;
    transferMut.mutate(undefined, {
      onSuccess: (res) => {
        toast.success(`已转入 ${moneyOf(res.amount)} 至余额`);
        invalidateWallet();
      },
      onError: (e) => toastError(e, "转入失败"),
    });
  };

  const columns = useMemo<ColumnDef<OrderItem, unknown>[]>(
    () =>
      orderColumns<OrderItem>({
        actions: (row) =>
          row.status === "expired"
            ? [
                {
                  icon: "refresh",
                  label: "续付",
                  busy: continueMut.isPending,
                  onClick: () => continueOrder(row),
                },
              ]
            : [],
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [continueMut.isPending],
  );

  const affLink =
    aff && typeof window !== "undefined"
      ? `${site.siteUrl || window.location.origin}/register?aff=${aff.aff_code}`
      : "";
  /**
   * 支付中面板显示「微信支付」这类中文方式名。
   *
   * 直接用后端下发的 pay_method_label（唯一真相源）：门户已拿不到 pay_channel
   * 这种「渠道名-方式名」的快照串，也不该自己维护一张枚举→中文的映射表。
   * 历史订单若 label 缺失，退回 pay_method 原文（可能是中文原文）。
   */
  const methodLabel = (label?: string, fallback?: string) => label || fallback || "";
  const leftMM = String(Math.floor(Math.max(left, 0) / 60)).padStart(2, "0");
  const leftSS = String(Math.max(left, 0) % 60).padStart(2, "0");

  const dialogOpen = draft !== null || pay !== null || done !== null;

  /* 到账拆分/预估摘要（套餐三段、自定义与续付单段）
     赠送为 0 时**降级成单段**：+$0.00 那一行既无信息量，又会让用户以为「说好的赠送呢」。
     判据用 bonus > 0 而不是「有 bonus 字段」—— 0 是合法业务值，字段永远存在。 */
  const summary = (d: { base?: number; bonus?: number; credit?: number }) =>
    d.base !== undefined && d.bonus !== undefined && d.bonus > 0 ? (
      <div className="pay-sum">
        <div className="flex-between">
          <span className="small muted">基础额度</span>
          {/* 与套餐卡同口径（两位小数）：同一屏里 $9.9999 与 $10.00 混排会被当成算错账 */}
          <span className="mono">{money2(d.base)}</span>
        </div>
        <div className="flex-between">
          <span className="small muted">赠送额度</span>
          <span className="mono text-success">+{money2(d.bonus)}</span>
        </div>
        <div className="divider mt0 mb0" />
        <div className="flex-between">
          <span className="small muted">到账合计</span>
          <b className="mono">{money2(d.base + d.bonus)}</b>
        </div>
      </div>
    ) : (
      <div className="pay-sum">
        <div className="flex-between">
          <span className="small muted">预计入账</span>
          <b className="mono">{money2(d.credit ?? 0)}</b>
        </div>
      </div>
    );

  /* 收银台底部动作：确认 → 取消/去支付；支付中 → 稍后支付/我已完成支付；成功 → 完成 */
  const cashierFooter = done ? (
    <button type="button" className="btn btn-primary" onClick={closeCashier}>
      完成
    </button>
  ) : pay ? (
    <>
      <button type="button" className="btn" onClick={closeCashier}>
        稍后支付
      </button>
      <button type="button" className="btn btn-primary" disabled={verifyMut.isPending} onClick={verifyPaid}>
        {verifyMut.isPending ? "正在核对…" : "我已完成支付"}
      </button>
    </>
  ) : (
    <>
      <button type="button" className="btn" onClick={closeCashier}>
        取消
      </button>
      <button
        type="button"
        className="btn btn-primary"
        disabled={!payOptions.length || topupMut.isPending}
        onClick={submitPay}
      >
        去支付
      </button>
    </>
  );

  return (
    <>
      <PageHead title="钱包" desc="充值 · 兑换码 · 邀请奖励 · 订单实时入账" />

      {/* ---------- 余额概览 ---------- */}
      <section className="balance-card" data-testid="wallet-hero">
        <div className="bl">
          账户余额
          {me?.group ? (
            <span className="badge">
              {me.group} · {myDiscount}
            </span>
          ) : null}
        </div>
        <div className="bv" data-testid="wallet-balance">{money2(me?.balance ?? 0)}</div>
        <div className="bs">
          <span>≈ {otherMoneyOf(me?.balance ?? 0, 2)}</span>
          <span>
            冻结中 <b className="mono">{moneyOf(me?.frozen ?? 0)}</b>
          </span>
          <span>
            累计消耗 <b className="mono">{moneyOf(me?.used ?? 0)}</b>
          </span>
        </div>
      </section>

      {/* ---------- 充值 / 兑换码 ---------- */}
      <Card
        title="充值"
        actions={
          <Tabs
            variant="pill"
            items={MAIN_TABS}
            value={mainTab.tab}
            onChange={(k) => mainTab.setTab(k as MainTab)}
            idPrefix="topup-main"
            label="充值方式"
          />
        }
      >
        {mainTab.tab === "pkg" ? (
          <div>
            {!payOptions.length ? (
              <div className="alert-banner warn">
                管理员尚未启用任何支付方式，暂时无法下单；请联系管理员完成支付配置。
              </div>
            ) : null}

            {pkgQuery.isLoading ? (
              <div className="small muted">加载中…</div>
            ) : pkgQuery.data?.items?.length ? (
              <div id="pkg-grid" className="grid grid-4">
                {pkgQuery.data.items.map((p: WalletPackage) => (
                  <button
                    key={p.id}
                    type="button"
                    className="pkg-card"
                    data-testid="pkg-card"
                    onClick={() =>
                      openCashier({
                        source: p.name,
                        base: p.base,
                        bonus: p.bonus,
                        credit: p.credited,
                        amountCent: p.price_cent,
                        body: { package_id: p.id },
                      })
                    }
                  >
                    {/* 角标为空时不渲染 DOM（而不是空 span）：e2e 靠「无节点」断言首卡无角标 */}
                    {p.badge ? (
                      <span className="pkg-ribbon">{PACKAGE_BADGE_LABELS[p.badge]}</span>
                    ) : null}
                    <div className="pkg-name">{p.name}</div>
                    {/* 价签符号必须跟随结算币种：套餐价是「该币种的绝对值」，
                        写死 ¥ 会让美元部署下的 $30 套餐显示成 ¥30（差 7.2 倍）。
                        用 formatCent 而不是 toFixed(0)：后者会把 $72.50 显示成 $73。 */}
                    <div className="amt">{formatCent(p.price_cent)}</div>
                    <div className="pkg-credit">
                      到账 <b className="mono">{money2(p.credited)}</b>
                    </div>
                    {/* 赠送为 0 时整块不渲染：此时「基础」与「到账」相等，
                        再列一行「赠送 +$0.00」纯属噪音（e2e 有「无节点」断言）。 */}
                    {p.bonus > 0 ? (
                      <div className="p-detail">
                        <span>
                          基础 <b className="mono">{money2(p.base)}</b>
                        </span>
                        <span>
                          赠送 <b className="mono text-success">+{money2(p.bonus)}</b>
                        </span>
                      </div>
                    ) : null}
                  </button>
                ))}
              </div>
            ) : (
              <EmptyState
                icon={<Icon name="coin" size={22} />}
                title="暂无充值套餐"
                desc="管理员尚未配置充值套餐，可使用下方自定义金额充值。"
              />
            )}

            {cfg && customTopupEnabled ? (
              <>
                <div className="divider" />
                <div className="small muted count-line">
                  付多少到账多少（无赠送、无倍率）；单笔不低于 {money2(minTopupMicro)}
                </div>
                <div className="custom-row">
                  <Field label={`金额（${currentSymbol()}）`} htmlFor="custom-amt">
                    <input
                      id="custom-amt"
                      type="text"
                      inputMode="decimal"
                      placeholder="例如 30"
                      value={customAmt}
                      onChange={(e) => setCustomAmt(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") {
                          e.preventDefault();
                          submitCustom();
                        }
                      }}
                    />
                  </Field>
                  <button
                    type="button"
                    className="btn btn-primary"
                    /* 金额非法/超上限时直接不可点：只靠 onClick 里的 toast 提示，
                       用户会反复点、以为页面卡住了。旁边的横幅已经说明原因。 */
                    disabled={topupMut.isPending || !amtValid || overLimit}
                    onClick={submitCustom}
                  >
                    立即充值
                  </button>
                </div>
                {amtValid && previewCredit > 0 ? (
                  <div className="conv-out mt16">
                    预计入账 <b className="mono">{money2(previewCredit)}</b>
                    <span className="small muted">（应付 {formatCent(amtCent ?? 0)}）</span>
                  </div>
                ) : null}
                {!amtValid && customAmt.trim() !== "" ? (
                  <div className="alert-banner warn mt16">金额格式不正确（只能填数字，最多 2 位小数）。</div>
                ) : null}
                {overLimit ? (
                  <div className="alert-banner warn mt16">
                    本次充值后将超过账户余额上限 {money2(balanceCap)}，请调整金额或先消费。
                  </div>
                ) : null}
              </>
            ) : null}
          </div>
        ) : (
          <div>
            <div className="small muted count-line">兑换码由管理员发放，核销后额度即时入账。</div>
            <div className="custom-row">
              <Field label="兑换码" htmlFor="redeem-code">
                <input
                  id="redeem-code"
                  type="text"
                  placeholder="例如 ABCD-EFGH-IJKL"
                  value={redeemCode}
                  onChange={(e) => setRedeemCode(e.target.value.toUpperCase())}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      submitRedeem();
                    }
                  }}
                />
              </Field>
              <button
                type="button"
                className="btn btn-primary"
                disabled={redeemMut.isPending}
                onClick={submitRedeem}
              >
                兑换
              </button>
            </div>
          </div>
        )}
      </Card>

      {/* ---------- 邀请奖励 | 资金记录 ---------- */}
      <div className="wallet-grid">
        <Card title="邀请奖励">
          {!aff ? (
            affQuery.isError ? (
              <div className="alert-banner warn">
                邀请返利信息加载失败。
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => void affQuery.refetch()}
                >
                  重试
                </button>
              </div>
            ) : (
              <div className="small muted count-line">加载中…</div>
            )
          ) : (
            <>
              {!aff.enabled ? (
                <div className="alert-banner info">
                  管理员未开启邀请返利：新的邀请不再绑定，也不再产生返利；已获得的返利仍可转入余额。
                </div>
              ) : null}
              {aff.degraded ? (
                <div className="alert-banner warn">
                  邀请返利配置不完整或超出范围，当前按默认值生效，请联系管理员核对。
                </div>
              ) : null}
              <div className="small muted count-line">
                好友前 <b className="mono">{aff.max_rewards}</b> 笔充值，你可得每笔实付的{" "}
                <b className="mono">{ratioPercent(aff.inviter_rate)}</b>；好友每笔再得{" "}
                <b className="mono">{moneyOf(aff.invitee_amount)}</b>（不超过该笔实付）。返利需转入余额后方可使用。
              </div>

              {aff.enabled ? (
                <>
                  <div className="uc-row">
                    <div className="uc-row-main">
                      <div className="rl">我的邀请码</div>
                      <div className="rs mono" data-testid="aff-code">
                        {aff.aff_code || "—"}
                      </div>
                    </div>
                    <div className="act">
                      <button
                        type="button"
                        className="btn btn-sm"
                        disabled={!aff.aff_code}
                        onClick={() => aff.aff_code && void copyToast(aff.aff_code)}
                      >
                        <Icon name="copy" size={14} />
                        复制
                      </button>
                    </div>
                  </div>

                  <div className="uc-row">
                    <div className="uc-row-main">
                      <div className="rl">邀请链接</div>
                      <div className="rs mono break-all">{affLink || "—"}</div>
                    </div>
                    <div className="act">
                      <button
                        type="button"
                        className="btn btn-sm"
                        disabled={!affLink}
                        onClick={() => affLink && void copyToast(affLink)}
                      >
                        <Icon name="copy" size={14} />
                        复制
                      </button>
                    </div>
                  </div>
                </>
              ) : null}

              <div className="uc-stats">
                <div className="uc-stat">
                  <div className="k">可转出返利</div>
                  <div className="v mono">{moneyOf(aff.aff_available)}</div>
                </div>
                <div className="uc-stat">
                  <div className="k">冷却中</div>
                  <div className="v mono">{moneyOf(aff.aff_cooling)}</div>
                </div>
                <div className="uc-stat">
                  <div className="k">累计返利</div>
                  <div className="v mono">{moneyOf(aff.aff_history)}</div>
                </div>
              </div>

              <div className="row-acts mt16">
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={
                    transferMut.isPending ||
                    aff.aff_available <= 0 ||
                    aff.aff_available < aff.min_transfer
                  }
                  onClick={() => void transferAff()}
                >
                  转入余额
                </button>
                <button type="button" className="btn" onClick={() => setAffDrawer(true)}>
                  邀请记录{aff.aff_count ? `（${aff.aff_count}）` : ""}
                </button>
              </div>
              {aff.aff_available < aff.min_transfer && (aff.aff_available > 0 || aff.aff_cooling > 0) ? (
                <div className="small muted mt16">
                  {aff.aff_available > 0 ? (
                    <>
                      可转出返利满 <b className="mono">{moneyOf(aff.min_transfer)}</b> 后可转入余额。
                    </>
                  ) : null}
                  {aff.aff_cooling > 0
                    ? ` 另有 ${moneyOf(aff.aff_cooling)} 处于 ${aff.hold_hours} 小时冷却期内，按笔到期。`
                    : ""}
                </div>
              ) : null}
            </>
          )}
        </Card>

        <Card
          title="资金记录"
          actions={
            <Tabs
              variant="pill"
              items={[
                { key: "tx", label: "资金流水" },
                { key: "orders", label: "充值订单" },
              ]}
              value={recTab.tab}
              onChange={(k) => recTab.setTab(k as RecTab)}
              idPrefix="topup-rec"
              label="资金记录视图"
            />
          }
        >
          {recTab.tab === "orders" ? (
            <>
              <div className="custom-row count-line">
                <Select
                  aria-label="订单状态"
                  value={orderStatus}
                  allLabel="全部状态"
                  items={ORDER_STATUS_ITEMS}
                  onChange={(e) => {
                    setOrderStatus(e.target.value);
                    setOrderPage(1);
                  }}
                />
                <span className="small muted">
                  共 <b className="mono">{orders?.total ?? 0}</b> 笔订单
                </span>
              </div>
              <DataTable
                columns={columns}
                data={orders?.items ?? []}
                total={orders?.total ?? 0}
                page={orderPage}
                pageSize={DEFAULT_PAGE_SIZE}
                onPageChange={setOrderPage}
                empty={
                  <EmptyState
                    icon={<Icon name="rows" size={22} />}
                    title="暂无充值订单"
                    desc="完成首次充值后，订单会出现在这里。"
                  />
                }
              />
            </>
          ) : (
            <>
              <div className="custom-row count-line">
                <Field label="类型" htmlFor="tx-type">
                  <Select
                    id="tx-type"
                    aria-label="流水类型"
                    value={txType}
                    allLabel="全部类型"
                    items={Object.entries(TRANSACTION_LABELS)}
                    onChange={(e) => {
                      setTxType(e.target.value);
                      setTxPage(1);
                    }}
                  />
                </Field>
                <span className="small muted">
                  共 <b className="mono">{txs?.total ?? 0}</b> 条流水
                </span>
              </div>
              <DataTable
                columns={TX_COLUMNS}
                data={txs?.items ?? []}
                total={txs?.total ?? 0}
                page={txPage}
                pageSize={DEFAULT_PAGE_SIZE}
                onPageChange={setTxPage}
                empty={
                  <EmptyState
                    icon={<Icon name="wallet" size={22} />}
                    title="暂无资金流水"
                    desc="充值、兑换、消费与返利都会记录在这里。"
                  />
                }
              />
            </>
          )}
        </Card>
      </div>

      {/* ---------- 收银台 ---------- */}
      <Dialog open={dialogOpen} onClose={closeCashier} title="收银台" size="lg" footer={cashierFooter}>
        {done ? (
          <div data-testid="cashier-success" className="ok-result">
            <div className="ok-mark">
              <Icon name="check" size={30} />
            </div>
            <div className="blk-title">支付成功</div>
            <div className="small muted">
              已到账 <b className="mono">{moneyOf(done.quota)}</b>
              <br />
              订单号 <span className="mono">{done.outTradeNo}</span>
            </div>
          </div>
        ) : pay ? (
          <div data-testid="cashier-pay">
            <div className="flex-between">
              <span className="muted">应付金额</span>
              <b className="mono pay-amt">{formatCent(pay.result.amount_cent, pay.result.currency)}</b>
            </div>
            <div className="small muted">
              {pay.source} · 订单号 <span className="mono">{pay.result.out_trade_no}</span> ·{" "}
              {methodLabel(pay.result.pay_method_label, pay.result.pay_method)}
            </div>
            <div className="mt16">{summary(pay)}</div>
            <PayPane result={pay.result} onRetry={retryPay} retrying={retryMut.isPending} />
            <div className="flex-between mt16">
              <span className="small muted">在收银台完成支付后自动到账</span>
              <span className="pay-count">
                剩余 {leftMM}:{leftSS}
              </span>
            </div>
          </div>
        ) : draft ? (
          <>
            <div className="flex-between">
              <span className="muted">应付金额</span>
              <b className="mono">{formatCent(draft.amountCent)}</b>
            </div>
            <div className="small muted">来源：{draft.source}</div>
            <div className="mt16">{summary(draft)}</div>
            {payOptions.length ? (
              <Field label="支付方式" className="mt16">
                <div className="pay-channels">
                  {payOptions.map((o) => (
                    <button
                      key={o.method}
                      type="button"
                      className={`pay-ch${o.method === method ? " on" : ""}`}
                      data-method={o.method}
                      onClick={() => {
                        setMethod(o.method);
                        writeLastMethod(o.method);
                      }}
                    >
                      <PayMethodIcon method={o.method} />
                      <span className="pay-ch-name">{o.label}</span>
                    </button>
                  ))}
                </div>
              </Field>
            ) : (
              <div className="alert-banner warn mt16">管理员未启用任何支付方式，请联系管理员后再试。</div>
            )}
          </>
        ) : null}
      </Dialog>

      {/* ---------- 邀请记录 ---------- */}
      <Drawer open={affDrawer} onClose={() => setAffDrawer(false)} title="邀请记录">
        <DataTable
          columns={AFF_RECORD_COLUMNS}
          data={affRecordsQuery.data?.items ?? []}
          total={affRecordsQuery.data?.total ?? 0}
          page={affPage}
          pageSize={DEFAULT_PAGE_SIZE}
          // 必须传 onPageChange，否则 DataTable 根本不渲染分页条（>20 条后续页不可达）。
          onPageChange={setAffPage}
          error={affRecordsQuery.isError ? "返利记录加载失败，请重试。" : undefined}
          empty={
            <EmptyState
              icon={<Icon name="user" size={22} />}
              title="暂无返利记录"
              desc="好友充值后，每一笔的返利会记录在这里；你作为被邀请人获得的赠金也会出现在这里。"
            />
          }
        />
      </Drawer>
    </>
  );
}

/**
 * 支付面板：二维码 / 跳转页 / 不可用重试 三种形态。
 *
 * 二维码为什么不直接用上游地址当 <img src>：
 *  1. 上游可能做 Referer 防盗链，浏览器直接加载会 403 裂图；
 *  2. 站点是 HTTPS 而上游图片是 HTTP 时，浏览器按混合内容拦截；
 *  3. <img> 无法携带 Authorization 头（本项目是 Bearer 鉴权，不是 Cookie）。
 * 所以图片型统一由后端代理成 data URL；内容型（微信 Native 的 weixin://、
 * 支付宝当面付的 https://qr.alipay.com/...）由前端本地画成二维码（PayQRCode）。
 */
function PayPane({
  result,
  onRetry,
  retrying,
}: {
  result: TopupResult;
  onRetry: () => void;
  retrying: boolean;
}) {
  const qrQuery = useOrderQRCodeQuery(result.qr_ref ? result.id : null);
  const qr = qrQuery.data?.qr;

  if (result.qr_ref) {
    if (qrQuery.isLoading) {
      return <div className="muted small mt16">正在获取支付二维码…</div>;
    }
    if (qr?.mode === "image" && qr.data_url) {
      return (
        <div className="pay-qr mt16" data-testid="pay-qr-image">
          <img src={qr.data_url} alt="支付二维码" />
          <div className="small muted">请用微信或支付宝扫码支付；手机端可长按识别</div>
        </div>
      );
    }
    if (qr?.mode === "content" && qr.content) {
      const content = qr.content;
      return (
        <div className="pay-qr mt16" data-testid="pay-qr-content">
          {/*
            官方直连（微信 Native / 支付宝当面付）返回的是二维码**内容**：
            前端自己画成二维码，PC 用户直接扫码；画不出来时下面的「复制链接」兜底。
          */}
          <PayQRCode content={content} />
          <div className="small muted">请用微信或支付宝扫码支付；手机端可复制链接后在对应 App 内打开</div>
          <code className="mono small break-all">{content}</code>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => {
              void copyText(content).then((ok) =>
                ok ? toast.success("已复制") : toast.error("复制失败，请手动选择复制"),
              );
            }}
          >
            复制链接
          </button>
        </div>
      );
    }
    // 二维码取不到（过期 / 上游异常）：不显示裂图，直接给重试入口。
    return (
      <div className="alert-banner warn mt16">
        <div>支付二维码已失效</div>
        <button type="button" className="btn btn-sm" disabled={retrying} onClick={onRetry}>
          {retrying ? "正在重新获取…" : "重新获取"}
        </button>
      </div>
    );
  }

  if (result.pay_url) {
    // 后端用 auto_redirect 告诉前端「这个地址是 hosted 收银台页」（Stripe 一类）：
    // 拿到即跳转，不留中间态。其它情况保留「打开支付页面」按钮，让用户显式点击（避免弹窗拦截）。
    // 判断依据**不再**是 pay_protocol —— 协议枚举属于后端知识，门户拿不到。
    if (result.auto_redirect) {
      return <StripeAutoJump url={result.pay_url} />;
    }
    return (
      <a className="btn btn-primary mt16" href={result.pay_url} target="_blank" rel="noreferrer">
        <Icon name="external" size={15} />
        打开支付页面
      </a>
    );
  }

  // 既没有二维码也没有跳转地址：创建支付失败过，只能重试同一订单。
  // 不展示后端的具体报错 —— 那是支付网关的原始 message（可能带商户号/渠道名），
  // 用户只需要知道「现在付不了，可以重试」。
  return (
    <div className="alert-banner warn mt16" data-testid="pay-unavailable">
      <div>支付方式暂不可用，请重试或更换支付方式</div>
      <button type="button" className="btn btn-sm" disabled={retrying} onClick={onRetry}>
        {retrying ? "正在重试…" : "重试"}
      </button>
    </div>
  );
}

/**
 * Stripe 收银台自动跳转：Stripe 只有 hosted 页、没有二维码，下单成功即跳转。
 * 挂载时 location.href 跳一次；同时保留「手动打开」链接作为兜底（弹窗拦截 / 已在新标签）。
 */
function StripeAutoJump({ url }: { url: string }) {
  useEffect(() => {
    window.location.href = url;
  }, [url]);
  return (
    <div className="mt16">
      <div className="small muted">正在跳转到支付页面…</div>
      <a className="btn btn-primary mt8" href={url} target="_blank" rel="noreferrer">
        <Icon name="external" size={15} />
        未自动跳转？点击打开
      </a>
    </div>
  );
}

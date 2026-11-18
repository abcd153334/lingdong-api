import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { toast } from "sonner";
import { ErrorState } from "@/components/common/QueryStates";
import { DataTable } from "@/components/common/DataTable";
import { Dialog } from "@/components/common/Dialog";
import { Drawer } from "@/components/common/Drawer";
import { Field } from "@/components/common/Field";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { PageHead } from "@/components/common/PageHead";
import { StatusTag } from "@/components/common/StatusTag";
import {
  exportAdminOrders,
  useAdminOrderManualCreditMutation,
  useAdminOrderRefundConfirmMutation,
  useAdminOrderRefundMutation,
  useAdminOrdersQuery,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { formatCent, formatDateTime } from "@/lib/format";
import { orderColumns } from "@/lib/table";
import { moneyAt } from "@/lib/money";
import type { AdminOrderItem } from "@/types";

const ORDER_STATUS_ITEMS: [string, string][] = [
  ["pending", "待支付"],
  ["paid", "已支付"],
  ["refunding", "退款中"],
  ["expired", "已过期"],
  ["refunded", "已退款"],
  ["need_manual", "人工处理"],
];

const EMPTY_FILTERS: Record<string, string> = { fStatus: "", fChannel: "" };

/** 筛选字段：状态 / 渠道（对齐 admin-orders.html）；渠道选项与门户收银台同源 */
const filterFields = (payItems: [string, string][]): FilterField[] => [
  { t: "select", id: "fStatus", label: "状态", allLabel: "全部状态", items: ORDER_STATUS_ITEMS },
  { t: "select", id: "fChannel", label: "渠道", allLabel: "全部渠道", items: payItems },
];

/** 退款/入账原因上限（与后端 orderReasonMax 一致） */
const REASON_MAX = 200;

/** 退款方式 */
type RefundMode = "gateway" | "balance";

/**
 * 默认退款方式：虎皮椒的已支付订单可原路退回；其余（易支付/人工处理态）只能回退余额。
 * 判据必须与后端 resolveRefundMode 一致，否则会出现「前端选了原路退回、后端拒绝」。
 */
function defaultRefundMode(o: AdminOrderItem): RefundMode {
  return o.status === "paid" && o.pay_protocol === "xunhupay" ? "gateway" : "balance";
}

/** 订单管理（S9）：状态/渠道筛选；明细抽屉；退款（原路退回 / 仅回退余额）与人工入账（原因必填，写审计） */
export function AdminOrdersPage() {
  const [page, setPage] = useState(1);
  const [filters, setFilters] = useState<Record<string, string>>(EMPTY_FILTERS);
  const [detail, setDetail] = useState<AdminOrderItem | null>(null);
  const [refundTarget, setRefundTarget] = useState<AdminOrderItem | null>(null);
  const [refundMode, setRefundMode] = useState<RefundMode>("gateway");
  const [creditTarget, setCreditTarget] = useState<AdminOrderItem | null>(null);
  const [confirmTarget, setConfirmTarget] = useState<AdminOrderItem | null>(null);
  const [confirmResult, setConfirmResult] = useState<"success" | "failed">("success");
  const [reason, setReason] = useState("");
  /** 上游核实凭证：确认「退款失败」会无条件回滚额度，后端强制要求留凭证。 */
  const [evidence, setEvidence] = useState("");

  const query = useAdminOrdersQuery({
    page,
    page_size: DEFAULT_PAGE_SIZE,
    status: filters.fStatus || undefined,
    // 按稳定渠道 ID 筛选：渠道改名不会影响历史订单的归属。
    pay_channel_id: filters.fChannel || undefined,
  });
  const orders = query.data?.items ?? [];
  const total = query.data?.total ?? 0;
  // 渠道选项来自订单列表响应（**含停用渠道**）。
  // 原实现用 /portal/packages 的 pay_options，那里只有 enabled=true 的渠道 ——
  // 渠道一停用就再也筛不出它的历史订单，对账只能翻全量。
  const payItems: [string, string][] = (query.data?.pay_channels ?? []).map((c) => [
    c.id,
    c.enabled ? c.name : `${c.name}（已停用）`,
  ]);

  const refundMut = useAdminOrderRefundMutation();
  const confirmMut = useAdminOrderRefundConfirmMutation();
  const creditMut = useAdminOrderManualCreditMutation();

  const onFilterChange = (id: string, value: string) => {
    setFilters((f) => ({ ...f, [id]: value }));
    setPage(1);
  };
  const onFilterReset = () => {
    setFilters(EMPTY_FILTERS);
    setPage(1);
  };

  const openRefund = (o: AdminOrderItem) => {
    setRefundTarget(o);
    setRefundMode(defaultRefundMode(o));
    setReason("");
  };

  const openCredit = (o: AdminOrderItem) => {
    setCreditTarget(o);
    setReason("");
  };

  const openConfirm = (o: AdminOrderItem, result: "success" | "failed") => {
    setConfirmTarget(o);
    setConfirmResult(result);
    setReason("");
    setEvidence("");
  };

  const submitRefund = () => {
    if (!refundTarget) return;
    const text = reason.trim();
    if (!text) {
      toast.error("请填写退款原因");
      return;
    }
    if (text.length > REASON_MAX) {
      toast.error(`退款原因不得超过 ${REASON_MAX} 字`);
      return;
    }
    refundMut.mutate(
      { order_id: refundTarget.id, reason: text, mode: refundMode },
      {
        onSuccess: (out) => {
          // 退款是「三态」：已退款 / 处理中 / 已回滚。混成一个「成功」会误导运营重复操作。
          const no = refundTarget.out_trade_no;
          if (!out) {
            toast.success(`订单 ${no} 已退款`);
          } else if (out.refund_status === "success") {
            toast.success(`订单 ${no} 已退款`);
          } else if (out.refund_status === "pending") {
            toast.info(`订单 ${no} 退款处理中：${out.message}`);
          } else {
            toast.error(`订单 ${no} 退款失败：${out.message}`);
          }
          setRefundTarget(null);
          setDetail(null);
        },
        onError: (e) => toast.error(e instanceof ApiError ? e.message : "退款失败，请稍后重试"),
      },
    );
  };

  const submitConfirm = () => {
    if (!confirmTarget) return;
    const text = reason.trim();
    if (!text) {
      toast.error("请填写核对说明");
      return;
    }
    if (text.length > REASON_MAX) {
      toast.error(`核对说明不得超过 ${REASON_MAX} 字`);
      return;
    }
    const ev = evidence.trim();
    if (confirmResult === "failed" && !ev) {
      // 回滚额度等于把钱还给用户：没有上游核实凭证就无法证明钱没退出去，
      // 后端也会 400（refund_evidence_required），这里提前挡住少一次往返。
      toast.error("确认「上游未退款」必须填写上游核实凭证");
      return;
    }
    confirmMut.mutate(
      { order_id: confirmTarget.id, result: confirmResult, reason: text, evidence: ev },
      {
        onSuccess: () => {
          toast.success(
            confirmResult === "success"
              ? `订单 ${confirmTarget.out_trade_no} 已标记为已退款`
              : `订单 ${confirmTarget.out_trade_no} 已标记退款失败，额度已回滚`,
          );
          setConfirmTarget(null);
          setDetail(null);
        },
        onError: (e) => toast.error(e instanceof ApiError ? e.message : "操作失败，请稍后重试"),
      },
    );
  };

  const submitCredit = () => {
    if (!creditTarget) return;
    const text = reason.trim();
    if (!text) {
      toast.error("请填写入账原因");
      return;
    }
    if (text.length > REASON_MAX) {
      toast.error(`入账原因不得超过 ${REASON_MAX} 字`);
      return;
    }
    creditMut.mutate(
      { order_id: creditTarget.id, reason: text },
      {
        onSuccess: () => {
          toast.success(`订单 ${creditTarget.out_trade_no} 已入账`);
          setCreditTarget(null);
          setDetail(null);
        },
        onError: (e) => toast.error(e instanceof ApiError ? e.message : "入账失败，请稍后重试"),
      },
    );
  };

  const onExport = async () => {
    try {
      await exportAdminOrders({
        status: filters.fStatus || undefined,
        pay_channel_id: filters.fChannel || undefined,
      });
      toast.success("已开始下载 CSV");
    } catch (e) {
      toast.error(e instanceof ApiError ? e.message : "导出失败，请稍后重试");
    }
  };

  const columns = useMemo<ColumnDef<AdminOrderItem, unknown>[]>(
    () =>
      orderColumns<AdminOrderItem>({
        user: (row) => (
          <span className="small">
            #{row.user_id} {row.username ?? "—"}
          </span>
        ),
        // 套餐名（实时）：自定义金额/续付订单为空 → 显示「—」，不占宽
        packageName: (row) => <span className="small muted">{row.package_name || "—"}</span>,
        payChannel: (row) => <span className="small muted">{row.pay_channel || "—"}</span>,
        // 到账金额是下单时固化的绝对值，调价不会改历史订单 ——
        // 因此这里不再有「刻度/倍率差异」标记：金额列与到账列本身就是真相。
        actions: (row) => [
          { icon: "eye", label: "详情", onClick: () => setDetail(row) },
          {
            icon: "wrench",
            label: "人工处理",
            hidden: row.status !== "need_manual",
            onClick: () => openCredit(row),
          },
          {
            icon: "undo",
            label: "退款",
            tone: "danger",
            hidden: row.status !== "paid" && row.status !== "need_manual",
            onClick: () => openRefund(row),
          },
          {
            icon: "check",
            label: "确认退款结果",
            hidden: row.status !== "refunding",
            onClick: () => openConfirm(row, "success"),
          },
        ],
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );

  const refundable = detail !== null && (detail.status === "paid" || detail.status === "need_manual");

  return (
    <>
      <PageHead
        title="订单管理"
        desc="充值订单查询、退款与人工处理"
        actions={[{ type: "btn", label: "导出 CSV", onClick: () => void onExport() }]}
      />

      <FilterBar
        fields={filterFields(payItems)}
        value={filters}
        onChange={onFilterChange}
        onReset={onFilterReset}
      />

      <DataTable
        columns={columns}
        data={orders}
        loading={query.isPending}
        error={query.isError ? <ErrorState error={query.error} onRetry={() => void query.refetch()} /> : undefined}
        total={total}
        page={page}
        pageSize={DEFAULT_PAGE_SIZE}
        onPageChange={setPage}
      />

      <Drawer open={detail !== null} onClose={() => setDetail(null)} title="订单详情">
        {detail ? (
          <>
            <div className="blk-title">① 订单信息</div>
            <div className="kv-grid">
              <div className="kv">
                <div className="k">订单号</div>
                <div className="v mono">{detail.out_trade_no}</div>
              </div>
              <div className="kv">
                <div className="k">交易号</div>
                <div className="v mono">{detail.trade_no || "—"}</div>
              </div>
              <div className="kv">
                <div className="k">用户</div>
                <div className="v">
                  #{detail.user_id} {detail.username ?? ""}
                </div>
              </div>
              <div className="kv">
                <div className="k">状态</div>
                <div className="v">
                  <StatusTag status={detail.status} />
                </div>
              </div>
              <div className="kv">
                <div className="k">支付渠道</div>
                <div className="v mono">{detail.pay_channel || "—"}</div>
              </div>
              <div className="kv">
                <div className="k">应付金额</div>
                {/* 明细必须用**订单自己的币种**：这单是按当时的币种收的钱，
                    用当前币种符号展示等于给历史账单换了个单位（¥100 变 $100）。 */}
                <div className="v mono">{formatCent(detail.amount_cent, detail.currency)}</div>
              </div>
              <div className="kv">
                <div className="k">到账额度</div>
                <div className="v">
                  {moneyAt(detail.credited, detail.currency)}
                </div>
              </div>
              <div className="kv">
                <div className="k">创建时间</div>
                <div className="v mono">{formatDateTime(detail.created_at)}</div>
              </div>
              <div className="kv">
                <div className="k">支付时间</div>
                <div className="v mono">{detail.paid_at ? formatDateTime(detail.paid_at) : "—"}</div>
              </div>
            </div>

            {detail.refund_mode ? (
              <>
                <div className="blk-title">② 退款信息</div>
                <div className="kv-grid">
                  <div className="kv">
                    <div className="k">退款方式</div>
                    <div className="v">
                      {detail.refund_mode === "gateway" ? "原路退回" : "仅回退平台余额"}
                    </div>
                  </div>
                  <div className="kv">
                    <div className="k">退款状态</div>
                    <div className="v">
                      {detail.refund_status === "success"
                        ? "已退款"
                        : detail.refund_status === "failed"
                          ? "退款失败（额度已回滚）"
                          : "处理中"}
                    </div>
                  </div>
                  <div className="kv">
                    <div className="k">退款单号</div>
                    <div className="v mono">{detail.refund_no || "—"}</div>
                  </div>
                  {/* 两个金额必须分开显示：`refund_amount_cent` 是**真的回到渠道**的钱
                      （仅回退余额时为 0），`refund_credited` 才是实际扣回的平台额度。
                      混成一行时，「只扣了额度」会被显示成「已退款 ¥N」。*/}
                  <div className="kv">
                    <div className="k">原路退款金额</div>
                    <div className="v mono">{formatCent(detail.refund_amount_cent ?? 0, detail.currency)}</div>
                  </div>
                  <div className="kv">
                    <div className="k">扣回平台额度</div>
                    <div className="v mono">{moneyAt(detail.refund_credited ?? 0, detail.currency)}</div>
                  </div>
                  <div className="kv">
                    <div className="k">发起时间</div>
                    <div className="v mono">{detail.refund_at ? formatDateTime(detail.refund_at) : "—"}</div>
                  </div>
                  <div className="kv">
                    <div className="k">完成时间</div>
                    <div className="v mono">
                      {detail.refund_done_at ? formatDateTime(detail.refund_done_at) : "—"}
                    </div>
                  </div>
                </div>
                {detail.refund_reason ? (
                  <div className="small muted">退款原因：{detail.refund_reason}</div>
                ) : null}
                {detail.refund_error ? (
                  <div className="alert-banner warn">{detail.refund_error}</div>
                ) : null}
              </>
            ) : null}

            {refundable ? (
              <>
                <div className="blk-title">② 人工退款</div>
                <div className="alert-banner warn">
                  {detail.status === "paid" && detail.pay_protocol === "xunhupay" ? (
                    <>
                      <b>原路退回会立即向虎皮椒发起全额退款，不可撤销。</b>
                      退款结果可能异步返回（银行卡 1–3 个工作日），订单会先进入「退款中」。
                      若已在虎皮椒后台手工退款，请改选「仅回退平台余额」，避免二次退款。
                    </>
                  ) : (
                    <>
                      <b>「仅回退平台余额」不会把钱原路退回用户</b>，只扣回已发放的额度。
                      {detail.pay_protocol !== "xunhupay"
                        ? "该渠道未接入原路退款接口，如需退款请到上游后台操作。"
                        : "该订单已转人工，上游退款请先在虎皮椒后台核对。"}
                    </>
                  )}
                </div>
                <div className="small muted">
                  退款将从用户余额扣回 <b className="mono">{moneyAt(detail.credited, detail.currency)}</b>
                  ，余额不足时整笔中止；操作原因将写入审计日志。
                </div>
                <button
                  type="button"
                  className="btn btn-danger"
                  onClick={() => openRefund(detail)}
                >
                  退款
                </button>
              </>
            ) : null}

            {detail.status === "refunding" ? (
              <>
                <div className="blk-title">③ 退款结果确认</div>
                <div className="alert-banner warn">
                  该订单正在退款中（额度已扣）。虎皮椒没有退款查询接口，结果只靠回调；
                  <b>请先到虎皮椒商户后台按订单号核对</b>，再在此确认。
                </div>
                <div className="row-acts">
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => openConfirm(detail, "success")}
                  >
                    确认已退款
                  </button>
                  <button
                    type="button"
                    className="btn btn-sm btn-ghost-danger"
                    onClick={() => openConfirm(detail, "failed")}
                  >
                    确认退款失败并回滚额度
                  </button>
                </div>
              </>
            ) : null}

            {detail.status === "need_manual" ? (
              <>
                <div className="blk-title">③ 人工入账</div>
                <div className="small muted">
                  该订单为丢单兜底态（回调入账超限转人工），人工核实支付凭据后将入账{" "}
                  <b className="mono">{moneyAt(detail.credited, detail.currency)}</b>{" "}
                  到用户余额；操作原因将写入审计日志。
                </div>
                <button
                  type="button"
                  className="btn btn-danger"
                  onClick={() => openCredit(detail)}
                >
                  人工入账
                </button>
              </>
            ) : null}
          </>
        ) : null}
      </Drawer>

      <Dialog
        open={refundTarget !== null}
        onClose={() => setRefundTarget(null)}
        title="人工退款"
        actions={[
          { label: "取消", onClick: () => setRefundTarget(null) },
          {
            label: "确认退款",
            variant: "danger",
            disabled: refundMut.isPending,
            onClick: submitRefund,
          },
        ]}
      >
        <Field label="订单" htmlFor="rf-order">
          <input
            id="rf-order"
            type="text"
            readOnly
            value={refundTarget ? refundTarget.out_trade_no : ""}
          />
        </Field>
        <Field label="退款金额（全额）" htmlFor="rf-amt">
          <input
            id="rf-amt"
            type="text"
            readOnly
            value={refundTarget ? formatCent(refundTarget.amount_cent, refundTarget.currency) : ""}
          />
        </Field>
        <Field label="退款方式" htmlFor="rf-mode" req>
          <div id="rf-mode" className="row-acts">
            <label className="small">
              <input
                type="radio"
                name="refund-mode"
                value="gateway"
                checked={refundMode === "gateway"}
                disabled={!refundTarget || defaultRefundMode(refundTarget) !== "gateway"}
                onChange={() => setRefundMode("gateway")}
              />{" "}
              原路退回（虎皮椒）
            </label>
            <label className="small">
              <input
                type="radio"
                name="refund-mode"
                value="balance"
                checked={refundMode === "balance"}
                onChange={() => setRefundMode("balance")}
              />{" "}
              仅回退平台余额
            </label>
          </div>
        </Field>
        <Field label="扣回额度" htmlFor="rf-quota">
          <input
            id="rf-quota"
            type="text"
            readOnly
            value={refundTarget ? `${moneyAt(refundTarget.credited, refundTarget.currency)}` : ""}
          />
        </Field>
        <Field label={`退款原因（必填，≤${REASON_MAX} 字）`} req htmlFor="rf-reason">
          <textarea
            id="rf-reason"
            rows={2}
            maxLength={REASON_MAX}
            placeholder="例如：重复支付，用户申请退款"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </Field>
        <div className="alert-banner warn">
          {refundMode === "gateway"
            ? "将向虎皮椒发起全额原路退款，退款结果可能异步返回；提交后不可撤销。"
            : "不会原路退款，只扣回平台额度；请确认上游是否已退款，避免重复退款或用户投诉。"}
        </div>
      </Dialog>

      <Dialog
        open={confirmTarget !== null}
        onClose={() => setConfirmTarget(null)}
        title="确认退款结果"
        actions={[
          { label: "取消", onClick: () => setConfirmTarget(null) },
          {
            label: "确认",
            variant: "danger",
            disabled: confirmMut.isPending,
            onClick: submitConfirm,
          },
        ]}
      >
        <Field label="订单" htmlFor="rc-order">
          <input
            id="rc-order"
            type="text"
            readOnly
            value={confirmTarget ? confirmTarget.out_trade_no : ""}
          />
        </Field>
        <Field label="核对结果" req htmlFor="rc-result">
          <div id="rc-result" className="row-acts">
            <label className="small">
              <input
                type="radio"
                name="refund-result"
                checked={confirmResult === "success"}
                onChange={() => setConfirmResult("success")}
              />{" "}
              上游已退款
            </label>
            <label className="small">
              <input
                type="radio"
                name="refund-result"
                checked={confirmResult === "failed"}
                onChange={() => setConfirmResult("failed")}
              />{" "}
              上游未退款（回滚额度）
            </label>
          </div>
        </Field>
        <Field label={`核对说明（必填，≤${REASON_MAX} 字）`} req htmlFor="rc-reason">
          <textarea
            id="rc-reason"
            rows={2}
            maxLength={REASON_MAX}
            placeholder="例如：虎皮椒后台显示退款成功，单号 xxx"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </Field>
        {confirmResult === "failed" ? (
          <Field
            label={`上游核实凭证（必填，≤${REASON_MAX} 字）`}
            req
            htmlFor="rc-evidence"
            tip="网关后台的订单号 / 退款单号 / 工单号。选了「上游未退款」会把额度退回用户，"
          >
            <input
              id="rc-evidence"
              type="text"
              maxLength={REASON_MAX}
              placeholder="例如：虎皮椒后台订单 20251027001，状态「未退款」"
              value={evidence}
              onChange={(e) => setEvidence(e.target.value)}
            />
          </Field>
        ) : null}
        <div className="alert-banner warn">
          该操作会终结退款并写入审计日志。选择「上游未退款」会把已扣额度退回用户余额。
        </div>
      </Dialog>

      <Dialog
        open={creditTarget !== null}
        onClose={() => setCreditTarget(null)}
        title="人工入账"
        actions={[
          { label: "取消", onClick: () => setCreditTarget(null) },
          {
            label: "确认入账",
            variant: "danger",
            disabled: creditMut.isPending,
            onClick: submitCredit,
          },
        ]}
      >
        <Field label="订单" htmlFor="mc-order">
          <input
            id="mc-order"
            type="text"
            readOnly
            value={creditTarget ? creditTarget.out_trade_no : ""}
          />
        </Field>
        <Field label="入账额度" htmlFor="mc-amt">
          <input
            id="mc-amt"
            type="text"
            readOnly
            value={creditTarget ? `${moneyAt(creditTarget.credited, creditTarget.currency)}` : ""}
          />
        </Field>
        <Field label={`入账原因（必填，≤${REASON_MAX} 字）`} req htmlFor="mc-reason">
          <textarea
            id="mc-reason"
            rows={2}
            maxLength={REASON_MAX}
            placeholder="例如：线下核实已付款，超限转人工"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </Field>
        <div className="alert-banner warn">人工入账不可撤销，请核实支付凭据后再执行。</div>
      </Dialog>
    </>
  );
}

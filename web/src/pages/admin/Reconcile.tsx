import { useMemo } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { toast } from "sonner";
import { Card } from "@/components/common/Card";
import { DataTable } from "@/components/common/DataTable";
import { MoneyText } from "@/components/common/MoneyText";
import { PageHead } from "@/components/common/PageHead";
import { StatCard } from "@/components/common/StatCard";
import { Tag } from "@/components/common/StatusTag";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import {
  useAdminReconcileQuery,
  useAffiliateBackfillMutation,
  type ReconcileAffiliateRow,
  type ReconcileChainRow,
  type ReconcileTaskRow,
} from "@/lib/api";
import { formatDateTime } from "@/lib/format";

/** 差异计数：0 是正常态（绿），非 0 必须刺眼（红）。 */
function Diff({ n }: { n: number }) {
  return <Tag tone={n > 0 ? "danger" : "success"}>{n}</Tag>;
}

/**
 * 任务口径差异的五类成因。
 *
 * 中文名只在这一处维护：后端返回的是稳定的英文枚举（对账脚本/告警要用，
 * 不能随文案变），给运维看的解释放在表格里。
 */
const TASK_ISSUE: Record<string, { label: string; tone: "danger" | "warning" }> = {
  settled_shortfall: { label: "交付未收款", tone: "danger" },
  charged_but_failed: { label: "收了钱但失败", tone: "danger" },
  settled_mismatch: { label: "结算额不符", tone: "danger" },
  pending_after_terminal: { label: "终态仍冻结", tone: "danger" },
  frozen_mismatch: { label: "冻结额不符", tone: "warning" },
};

/**
 * 检查段的中文名。
 *
 * 后端只回段名（不放 SQL 错误原文，那会泄露库结构），
 * 这里负责把段名翻译成人话；认不出的键原样显示 ——
 * 将来新增段时页面会露出英文名，总比显示成空白好。
 */
const SEGMENT_LABEL: Record<string, string> = {
  users: "用户余额与冻结",
  usages: "运行态（pending）",
  ledger: "流水与用量汇总",
  orders: "订单到账",
  refunds: "退款",
  affiliate: "返利凭据",
  tasks: "任务口径",
  indexes: "索引健康度",
};

/**
 * 资金对账（`/admin/reconcile`）。
 *
 * 只读报表：全页没有任何写数据的动作。返利额度是 affiliate_rewards 的派生量，
 * 退款只在冷却期内作废、不追缴余额，所以这里既没有「欠款」也没有「补收」按钮。
 *
 * 这里的所有数字都是后端 SQL 算好的，前端不做任何算术 ——
 * 每一项都是「两份账本对不上」的计数，自己再算一遍只会引入第二套口径。
 */
export function AdminReconcilePage() {
  const rec = useAdminReconcileQuery();
  const backfill = useAffiliateBackfillMutation();
  const d = rec.data;
  const chainRows = d?.users.chain_samples ?? [];
  const taskRows = d?.tasks.samples ?? [];
  const incomplete = d?.errors ?? [];

  /**
   * 补发漏发的返利：先试算、再确认、最后执行。
   *
   * 为什么不直接 apply：这是会写库的动作，而「最近 7 天有几笔」只有跑完才知道。
   * 先跑 dry_run 拿到计数，再让人看着数字点头，比一个盲目的「一键补发」安全。
   */
  async function onBackfill(): Promise<void> {
    try {
      const pre = await backfill.mutateAsync({ days: 7, dry_run: true });
      if (pre.scanned === 0) {
        toast.info("最近 7 天没有需要补发的订单");
        return;
      }
      const ok = await confirmDialog({
        title: "补发漏发的邀请返利",
        body: (
          <>
            最近 7 天有 <b className="mono">{pre.scanned}</b> 笔已入账订单没有任何返利行。
            补发只写返利凭据、<b>不改余额</b>，用户仍需自行「转入余额」。
          </>
        ),
        okText: "确认补发",
      });
      if (!ok) return;
      const res = await backfill.mutateAsync({ days: 7, limit: pre.scanned });
      toast.success(`补发完成：成功 ${res.rewarded} 笔，跳过 ${res.skipped} 笔`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "补发失败");
    }
  }

  const columns = useMemo<ColumnDef<ReconcileAffiliateRow, unknown>[]>(
    () => [
      {
        accessorKey: "order_id",
        header: "订单",
        meta: { label: "订单" },
        cell: (c) => <span className="mono">#{c.getValue<number>()}</span>,
      },
      {
        accessorKey: "invitee_id",
        header: "被邀请人",
        meta: { label: "被邀请人" },
        cell: (c) => <span className="mono">{c.getValue<number>()}</span>,
      },
      {
        accessorKey: "beneficiary_id",
        header: "受益人",
        meta: { label: "受益人" },
        cell: (c) => {
          const v = c.getValue<number>();
          return <span className="mono">{v > 0 ? v : "—"}</span>;
        },
      },
      {
        accessorKey: "role",
        header: "身份",
        meta: { label: "身份" },
        cell: (c) => {
          const v = c.getValue<string>();
          const text = v === "inviter" ? "邀请人" : v === "invitee" ? "被邀请人" : "—";
          return <span className="small">{text}</span>;
        },
      },
      {
        accessorKey: "amount",
        header: "返利金额",
        meta: { label: "返利金额" },
        // 金额是**微**：必须走 MoneyText，不能当额度或元自己除。
        cell: (c) => <MoneyText micro={c.getValue<number>()} digits={2} />,
      },
      {
        accessorKey: "available_at",
        header: "应收时间",
        meta: { label: "应收时间" },
        cell: (c) => formatDateTime(c.getValue<string>()),
      },
      {
        accessorKey: "issue",
        header: "成因",
        meta: { label: "成因" },
        cell: (c) => (
          <span className="muted">
            {c.getValue<string>() === "missing_reward" ? "已入账但未发返利" : "钱已退、返利仍有效"}
          </span>
        ),
      },
    ],
    [],
  );

  /**
   * 余额链差异样本。
   *
   * 只有计数时完全没法判断性质：差 1–2 微基本是单位换算的舍入残差
   * （一次性校正即可，`implant fix-ledger`），差得多才是丢账/重复入账。
   * 所以这里必须把具体流水列出来，让运维一眼定性。
   */
  const chainColumns = useMemo<ColumnDef<ReconcileChainRow, unknown>[]>(
    () => [
      {
        accessorKey: "user_id",
        header: "用户",
        meta: { label: "用户" },
        cell: (c) => <span className="mono">#{c.getValue<number>()}</span>,
      },
      {
        accessorKey: "txn_id",
        header: "流水",
        meta: { label: "流水" },
        cell: (c) => <span className="mono">#{c.getValue<number>()}</span>,
      },
      {
        accessorKey: "prev_balance_after",
        header: "上一行余额",
        meta: { label: "上一行余额" },
        cell: (c) => <MoneyText micro={c.getValue<number>()} digits={6} />,
      },
      {
        accessorKey: "amount",
        header: "本行金额",
        meta: { label: "本行金额" },
        cell: (c) => <MoneyText micro={c.getValue<number>()} digits={6} />,
      },
      {
        accessorKey: "balance_after",
        header: "本行余额",
        meta: { label: "本行余额" },
        cell: (c) => <MoneyText micro={c.getValue<number>()} digits={6} />,
      },
      {
        accessorKey: "delta",
        header: "差额",
        meta: { label: "差额" },
        // 差额是**微**，且常态应该正好是 0；非 0 时把数值原样摊开，
        // 不要做四舍五入成 0.00 —— 那样看起来「没有问题」。
        cell: (c) => {
          const v = c.getValue<number>();
          return <span className="mono">{v > 0 ? `+${v}` : v}</span>;
        },
      },
    ],
    [],
  );

  /**
   * 任务口径差异样本。
   *
   * 为什么必须摊开而不是只给计数：任务链路的钱全在 usages（frozen / charge 都那张表），
   * tasks 上的 settled / frozen 只是副本。两者算错时账面依然“平”，
   * 只有把两列并排摆出来，才能看出是「任务侧没跟上」还是「账务侧记错了」。
   */
  const taskColumns = useMemo<ColumnDef<ReconcileTaskRow, unknown>[]>(
    () => [
      {
        accessorKey: "task_id",
        header: "任务",
        meta: { label: "任务" },
        cell: (c) => <span className="mono">#{c.getValue<number>()}</span>,
      },
      {
        accessorKey: "status",
        header: "任务状态",
        meta: { label: "任务状态" },
        cell: (c) => <span className="mono">{c.getValue<string>()}</span>,
      },
      {
        accessorKey: "issue",
        header: "问题",
        meta: { label: "问题" },
        cell: (c) => {
          const k = c.getValue<string>();
          const hit = TASK_ISSUE[k];
          return <Tag tone={hit?.tone ?? "danger"}>{hit?.label ?? k}</Tag>;
        },
      },
      {
        accessorKey: "settled",
        header: "任务已结算",
        meta: { label: "任务已结算" },
        cell: (c) => <MoneyText micro={c.getValue<number>()} digits={6} />,
      },
      {
        accessorKey: "charge",
        header: "账务应收",
        meta: { label: "账务应收" },
        cell: (c) => <MoneyText micro={c.getValue<number>()} digits={6} />,
      },
      {
        accessorKey: "task_frozen",
        header: "任务冻结",
        meta: { label: "任务冻结" },
        cell: (c) => <MoneyText micro={c.getValue<number>()} digits={6} />,
      },
      {
        accessorKey: "usage_frozen",
        header: "账务冻结",
        meta: { label: "账务冻结" },
        cell: (c) => <MoneyText micro={c.getValue<number>()} digits={6} />,
      },
    ],
    [],
  );


  return (
    <>
      <PageHead
        title="资金对账"
        desc="只读比对：余额链、冻结、任务口径、用量、订单到账、退款与返利凭据。报表不修改任何业务数据。"
        actions={[
          {
            type: "btn",
            label: rec.isFetching ? "刷新中…" : "刷新",
            icon: "refresh",
            onClick: () => void rec.refetch(),
            disabled: rec.isFetching,
          },
        ]}
      />

      <div className="grid grid-3">
        <StatCard
          label="余额链差异"
          value={<Diff n={d?.users.chain_mismatch ?? 0} />}
          sub="上一行余额 + 本行金额 ≠ 本行余额"
        />
        <StatCard
          label="余额不符"
          value={<Diff n={d?.users.balance_mismatch ?? 0} />}
          sub="用户表余额 ≠ 末条流水余额"
        />
        <StatCard
          label="冻结不符"
          value={<Diff n={d?.users.frozen_mismatch ?? 0} />}
          sub="用户冻结 ≠ 待结算合计"
        />
        <StatCard
          label="已付未入账"
          value={<Diff n={d?.orders.missing_ledger ?? 0} />}
          sub="订单 paid 但没有 recharge 流水"
        />
        <StatCard
          label="钱到单未结"
          value={<Diff n={d?.orders.pending_but_credited ?? 0} />}
          sub="已有到账流水，订单仍是 pending"
        />
        <StatCard
          label="在途退款"
          value={<Diff n={d?.refunds.stuck_refunding ?? 0} />}
          sub="超过 30 分钟未终结的退款"
        />
        <StatCard
          label="上游已退未扣"
          value={<Diff n={d?.refunds.upstream_only ?? 0} />}
          sub="需在订单页「仅回退余额」补扣"
        />
        <StatCard
          label="返利未作废"
          value={<Diff n={d?.affiliate.leak_rows ?? 0} />}
          sub={
            d ? (
              <>
                合计 <MoneyText micro={d.affiliate.leak_total} digits={2} />
              </>
            ) : (
              "—"
            )
          }
        />
        <StatCard
          label="返利未发放"
          value={<Diff n={d?.affiliate.missing_rows ?? 0} />}
          sub="已入账却没有返利行，需补发"
        />
        <StatCard
          label="返利转出差"
          value={<Diff n={Math.abs(d?.affiliate.ledger_diff ?? 0)} />}
          sub={
            d ? (
              <>
                已转出 <MoneyText micro={d.affiliate.transferred_sum} digits={2} /> · 冷却中{" "}
                <MoneyText micro={d.affiliate.cooling_total} digits={2} />
              </>
            ) : (
              "—"
            )
          }
        />
        <StatCard
          label="任务结算不符"
          value={<Diff n={d?.tasks.settled_mismatch ?? 0} />}
          sub="任务已成功，settled ≠ 账务 charge"
        />
        <StatCard
          label="交付未收款"
          value={<Diff n={d?.tasks.settled_shortfall ?? 0} />}
          sub="任务成功但账已释放：迟到补收失败 / 账目归档"
        />
        <StatCard
          label="收了钱但失败"
          value={<Diff n={d?.tasks.charged_but_failed ?? 0} />}
          sub="钱扣了、任务失败/超时：需人工退款"
        />
        <StatCard
          label="终态仍冻结"
          value={<Diff n={d?.tasks.pending_after_terminal ?? 0} />}
          sub="任务已终结，账务仍 pending（钱一直占着）"
        />
        <StatCard
          label="任务冻结不符"
          value={<Diff n={d?.tasks.frozen_mismatch ?? 0} />}
          sub={
            d ? (
              <>
                差额{" "}
                <MoneyText micro={d.tasks.frozen_diff} digits={6} signed />
              </>
            ) : (
              "任务冻结 ≠ 账务冻结"
            )
          }
        />
      </div>

      {/*
        未完成提示条必须排在所有样本之前。
        失败段的计数是 0，而 0 与「查过、没差异」在页面上完全同形 ——
        不把「哪几段没跑完」摆在最上面，这份报表会把「没查」伪装成「账目完美」，
        比完全不跑对账更危险。
      */}
      {incomplete.length > 0 ? (
        <Card
          title="本次未完成的检查项"
          sub="这些段查询失败，对应的 0 只能理解为「没查过」，不能理解为「没有差异」"
        >
          <ul className="muted mono">
            {incomplete.map((seg) => (
              <li key={seg}>{SEGMENT_LABEL[seg] ?? seg}</li>
            ))}
          </ul>
        </Card>
      ) : null}

      {chainRows.length > 0 ? (
        <Card
          title="余额链差异样本"
          sub="上一行余额 + 本行金额 ≠ 本行余额；差 1–2 微多为单位换算舍入，差得多才是真丢账"
        >
          <DataTable
            columns={chainColumns}
            data={chainRows}
            empty={<span className="muted">没有余额链差异。</span>}
          />
        </Card>
      ) : null}

      {taskRows.length > 0 ? (
        <Card
          title="任务口径差异样本"
          sub="任务侧 settled/frozen 只是副本，扣款依据在账务；两列并排看才知道该改哪边"
        >
          <DataTable
            columns={taskColumns}
            data={taskRows}
            empty={<span className="muted">没有任务口径差异。</span>}
          />
        </Card>
      ) : null}

      <Card
        title="返利凭据差异"
        sub="退款发生在冷却期内、该笔返利却仍可转出：说明某条退款路径没作废"
      >
        <DataTable
          columns={columns}
          data={d?.affiliate.samples ?? []}
          empty={<span className="muted">没有应作废未作废的返利凭据。</span>}
        />
      </Card>

      {d && d.indexes.missing.length > 0 ? (
        <Card title="索引缺失" sub="缺失或 INVALID 会让去重/扫描保护失效，请尽快补建">
          <ul className="muted mono">
            {d.indexes.missing.map((name) => (
              <li key={name}>{name}</li>
            ))}
          </ul>
        </Card>
      ) : null}

      <p className="muted mono">
        生成时间：{d ? formatDateTime(d.generated_at) : "—"}
      </p>
    </>
  );
}

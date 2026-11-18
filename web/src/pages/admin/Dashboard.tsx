import { useMemo, useState } from "react";
import { Link } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { Card } from "@/components/common/Card";
import { RankBars, TrendChart } from "@/components/common/Charts";
import { DataTable } from "@/components/common/DataTable";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityIcon } from "@/components/common/EntityIcon";
import { PageHead } from "@/components/common/PageHead";
import { StatCard } from "@/components/common/StatCard";
import { StatusTag, Tag } from "@/components/common/StatusTag";
import { Tabs } from "@/components/common/Tabs";
import { useAdminBoardQuery, useAdminCapacityQuery } from "@/lib/api";
import {
  amountNum,
  formatBytes,
  formatNum,
} from "@/lib/format";
import { currentSymbol, money2, moneyOf } from "@/lib/money";
import type { AdminCapacityResult, BoardChannelHealth, BoardUserRow } from "@/types";

const RANGE_TABS = [
  { key: "d7", label: "近 7 天" },
  { key: "d30", label: "近 30 天" },
];

/** 排行条的 tooltip 文本：币种符号前置（「¥12.34」而不是「12.34¥」）。 */
function moneyAxis(v: number): string {
  return `${currentSymbol()}${v.toLocaleString("en-US", { maximumFractionDigits: 4 })}`;
}

/** 健康点：失败率 <8% 良好 / <20% 注意 / 其余熔断风险（对齐原型 .health-dot） */
function healthDot(failRate: number): string {
  if (failRate < 0.08) return "health-dot ok";
  if (failRate < 0.2) return "health-dot warn";
  return "health-dot bad";
}

/** 仪表盘（S11 §4.9.4 / G17）：今日指标 + 近 7 日趋势 + 消耗排行 + 渠道健康。 */
export function AdminDashboardPage() {
  const board = useAdminBoardQuery();
  const capacity = useAdminCapacityQuery();
  const [range, setRange] = useState("d7");

  const data = board.data;
  const rank = data?.boards[range as "d7" | "d30"];

  /**
   * 趋势统一到**当前结算币种**：三列都是微，只需要换成金额数字，不做跨量纲换算。
   * （后端已把充值从订单的「分」换算成微，页面再乘一次就是差一万倍的错账。）
   */
  const trend = useMemo(
    () =>
      (data?.trend ?? []).map((p) => ({
        date: p.date.slice(5),
        收入: amountNum(p.recharge),
        成本: amountNum(p.cost),
      })),
    [data?.trend],
  );

  /** 消费占比以区间合计为分母（对齐原型 sum 口径，不随最大项放大） */
  const userColumns = useMemo<ColumnDef<BoardUserRow, unknown>[]>(() => {
    const users = rank?.users ?? [];
    const sum = users.reduce((s, u) => s + u.charge, 0);
    return [
      {
        accessorKey: "username",
        header: "用户",
        meta: { label: "用户" },
        cell: (c) => <span className="mono">{c.getValue<string>()}</span>,
      },
      {
        accessorKey: "group",
        header: "分组",
        meta: { label: "分组" },
        cell: (c) => <Tag tone="info">{c.getValue<string>() || "default"}</Tag>,
      },
      {
        accessorKey: "requests",
        header: "请求数",
        meta: { label: "请求数" },
        cell: (c) => <span className="num">{formatNum(c.getValue<number>())}</span>,
      },
      {
        accessorKey: "charge",
        header: "消费",
        meta: { label: "消费" },
        cell: (c) => <span className="mono">{moneyOf(c.getValue<number>())}</span>,
      },
      {
        id: "share",
        header: "消费占比",
        meta: { label: "消费占比" },
        cell: (c) => {
          const pct = sum > 0 ? Math.round((c.row.original.charge / sum) * 100) : 0;
          return (
            <div className="share-cell">
              <div className="progress">
                <i style={{ width: `${pct}%` }} />
              </div>
              <span className="mono small muted">{pct}%</span>
            </div>
          );
        },
      },
    ];
  }, [rank?.users]);

  /* 排行条的 value 是「微」，图表要的是币种金额数字 —— 统一在这里换算，图表组件不认识微。 */
  const userTop = useMemo(
    () => (rank?.user_top ?? []).map((r) => ({ label: r.label, value: amountNum(r.value) })),
    [rank?.user_top],
  );
  const modelTop = useMemo(
    () =>
      (rank?.model_top ?? []).map((r) => ({
        label: r.label,
        value: amountNum(r.value),
        iconUrl: r.icon_url,
      })),
    [rank?.model_top],
  );

  const warnChannels = (data?.channels ?? []).filter((c) => c.status === 3);
  /* 数组字段全部兜底成 []：后端约定「数组永不为 null」，但一旦某次改动漏了投影层，
     `.length` 会让 ErrorBoundary 把整页换成 Something went wrong（HTTP 200，接口层看不出）。 */
  const failTop = data?.fail_top ?? [];
  const channels = data?.channels ?? [];
  const rankUsers = rank?.users ?? [];
  const today = data?.today;
  const costCollected = today?.cost_collected ?? false;
  /* 毛利 = 收入 − 成本。两者都是微，直接相减；此处**不需要**任何换算。 */
  const profitMicro = today && costCollected ? today.recharge - today.cost : null;

  return (
    <>
      <PageHead title="仪表盘" desc="收入、成本、成功率与渠道健康总览" />

      {board.isPending ? (
        <div className="card">
          <EmptyState variant="loading" title="加载中…" />
        </div>
      ) : !data || !today ? (
        <div className="card">
          <EmptyState
            variant="error"
            title="看板数据加载失败"
            action={
              <button className="btn" type="button" onClick={() => board.refetch()}>
                重试
              </button>
            }
          />
        </div>
      ) : (
        <>
          {today.price_missing_count > 0 ? (
            <div className="alert-banner warn">
              <span>
                有 <b className="mono">{today.price_missing_count}</b> 个启用模型未配置价格，
                未配价模型调用将被拒绝（防白嫖策略）。
              </span>
              <Link className="right" to="/admin/model-service">
                去处理 →
              </Link>
            </div>
          ) : null}
          {warnChannels.length > 0 ? (
            <div className="alert-banner danger">
              <span>
                有 <b className="mono">{warnChannels.length}</b> 个渠道已被熔断自动禁用
                （{warnChannels.map((c) => c.name).join("、")}），探测 Worker 将按配置自动恢复。
              </span>
              <Link className="right" to="/admin/model-service">
                去处理 →
              </Link>
            </div>
          ) : null}

          <div className="grid grid-3">
            <StatCard
              icon="rows"
              label="今日请求"
              value={formatNum(today.requests)}
              sub="全渠道合计"
            />
            <StatCard
              icon="wallet"
              label="今日收入"
              value={money2(today.recharge)}
              sub="今日已支付订单实付"
            />
            <StatCard
              icon="coin"
              label="今日成本"
              value={costCollected ? money2(today.cost) : "—"}
              sub={costCollected ? "上游成本（按当前币种）" : "成本采集未启用"}
            />
            <StatCard
              icon="zap"
              label="今日毛利"
              value={money2(profitMicro)}
              sub={costCollected ? "收入 − 成本" : "启用成本采集后展示收入减成本"}
            />
            <StatCard
              icon="check"
              label="成功率"
              value={`${(today.success_rate * 100).toFixed(1)}%`}
              sub="成功请求占比"
            />
            <StatCard
              icon="grid"
              label="在线渠道"
              value={`${today.channels_online}/${today.channels_total}`}
              sub="探活通过 / 渠道总数"
            />
          </div>

          <Card title={`收入 vs 成本（近 7 天，${currentSymbol()}）`}>
            <TrendChart
              data={trend}
              xKey="date"
              height={240}
              series={[
                { key: "收入", name: `收入（${currentSymbol()}）` },
                { key: "成本", name: `成本（${currentSymbol()}）`, color: "var(--m2)" },
              ]}
            />
          </Card>

          <div className="grid grid-2">
            <Card title="失败 Top 5（按模型）">
              {failTop.length ? (
                <RankBars data={failTop} unit="次" height={220} />
              ) : (
                <EmptyState title="近 7 天无失败调用" />
              )}
            </Card>

            <Card title="渠道健康" sub="失败率低于 8% 良好，超过 20% 需注意">
              {channels.length ? (
                <div>
                  {channels.map((ch: BoardChannelHealth) => (
                    // 直接落到该渠道：`?c=` 会被页面解析成"选中它并展开所属适配器"，
                    // 否则运营点进来还要在左栏里再找一次这条渠道。
                    <Link
                      key={ch.id}
                      to="/admin/model-service"
                      search={{ c: ch.id }}
                      className="chan-row"
                      title="前往该渠道的模型"
                    >
                      <span className={healthDot(ch.fail_rate ?? 0)} />
                      {/* 健康点在左（状态），图标在名前后（身份）：两件事不混在同一处 */}
                      <EntityIcon url={ch.icon_url} name={ch.name} size="xs" />
                      <span className="cn">{ch.name}</span>
                      <span className="mono small muted">
                        {((ch.fail_rate ?? 0) * 100).toFixed(1)}%
                      </span>
                      <StatusTag status={ch.status} />
                    </Link>
                  ))}
                </div>
              ) : (
                <EmptyState title="暂无渠道" />
              )}
            </Card>
          </div>

          <Card
            title="运维与容量"
            sub="每小时自动体检；保留期与指标口由环境变量控制"
          >
            {capacity.data ? <CapacityBlock data={capacity.data} /> : <EmptyState title="容量数据加载中" />}
          </Card>

          <Card
            title="数据看板"
            sub="按用户聚合的用量与消费"
            actions={<Tabs items={RANGE_TABS} value={range} onChange={setRange} variant="pill" />}
          >
            <div className="grid grid-2 items-start">
              <div>
                <div className="blk-title">用户消耗 Top</div>
                {userTop.length ? (
                  <RankBars data={userTop} format={moneyAxis} height={200} />
                ) : (
                  <EmptyState title="该区间暂无用户消耗排行" />
                )}
              </div>
              <div>
                <div className="blk-title">模型消耗 Top</div>
                {modelTop.length ? (
                  <RankBars data={modelTop} format={moneyAxis} height={200} />
                ) : (
                  <EmptyState title="该区间暂无模型消耗" />
                )}
              </div>
            </div>

            <div className="blk-title">用户聚合明细</div>
            <DataTable
              columns={userColumns}
              data={rankUsers}
              empty={<EmptyState title="该区间暂无用户消耗" />}
            />
          </Card>
        </>
      )}
    </>
  );
}

/**
 * 容量体检区块（B7）。
 *
 * `free_ratio < 0` 表示磁盘信息读取失败，**不能**当作 0% 可用去渲染成「磁盘满了」：
 * 「监控瞎了」与「磁盘爆了」的处置动作完全相反（查挂载 vs 清数据）。
 */
function CapacityBlock({ data }: { data: AdminCapacityResult }) {
  const s = data.snapshot;
  const diskOk = s.free_ratio >= 0;
  return (
    <>
      {s.error ? <div className="alert-banner danger">容量监控异常：{s.error}</div> : null}
      {diskOk && s.warn ? <div className="alert-banner danger">磁盘水位告警：{s.warning}</div> : null}
      <div className="grid grid-2">
        {/* 两列都走 .kv-stack：.kv 自身不带外边距（它原本只为 .kv-grid 设计），
            裸堆叠会让相邻两块贴成一体、边框叠成双线。 */}
        <div className="kv-stack">
          <div className="kv">
            <div className="k">产物占用</div>
            <div className="v mono">{formatBytes(s.bytes)}</div>
          </div>
          <div className="kv">
            <div className="k">产物数量</div>
            <div className="v mono">{formatNum(s.files)}</div>
          </div>
        </div>
        <div className="kv-stack">
          <div className="kv">
            <div className="k">磁盘可用 / 总量</div>
            <div className="v mono">
              {diskOk ? `${formatBytes(s.disk_avail)} / ${formatBytes(s.disk_total)}` : "—"}
            </div>
          </div>
          <div className="kv">
            <div className="k">可用占比</div>
            <div className="v mono">{diskOk ? `${(s.free_ratio * 100).toFixed(1)}%` : "—"}</div>
          </div>
          <div className="kv">
            <div className="k">任务保留</div>
            <div className="v">
              {data.task_retention_days > 0 ? `${data.task_retention_days} 天` : "未启用（只报告不删）"}
            </div>
          </div>
          <div className="kv">
            <div className="k">指标口</div>
            <div className="v">{data.metrics_enabled ? "已开放 /metrics" : "未开放"}</div>
          </div>
        </div>
      </div>
      <div className="small muted">
        告警阈值：已用 ≥ {data.thresholds.warn_percent}% 或剩余 &lt; {data.thresholds.min_free_gb} GiB
        {s.stats_error ? `；产物统计失败：${s.stats_error}` : ""}
      </div>
    </>
  );
}

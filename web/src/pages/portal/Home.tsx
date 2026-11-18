import { useMemo, useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";

import { Card } from "@/components/common/Card";
import { TrendChart } from "@/components/common/Charts";
import { DataTable } from "@/components/common/DataTable";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityCell } from "@/components/common/EntityCell";
import { Icon } from "@/components/common/Icon";
import { QuotaUnit } from "@/components/common/MoneyText";
import { PageHead } from "@/components/common/PageHead";
import { useNoticeOpen } from "@/components/chrome/noticeOpen";
import { StatCard, type StatCardStatus } from "@/components/common/StatCard";
import { ModalityTag, usageStatusKey } from "@/components/common/StatusTag";
import { Tabs } from "@/components/common/Tabs";
import {
  useAnnouncementsQuery,
  useMeQuery,
  usePortalDefaultsQuery,
  usePortalOverviewQuery,
  usePortalUsageQuery,
  usePricingQuery,
} from "@/lib/api";
import { amountNum, formatNum } from "@/lib/format";
import { chargeSub, monthSub, todaySub } from "@/lib/homeStats";
import { renderInline } from "@/lib/markdown";
import { noticeTime, unreadOf } from "@/lib/notice";
import { useIsMobile } from "@/lib/responsive";
import { usageColumns } from "@/lib/table";
import { cn } from "@/lib/utils";
import {
  currentSymbol,
  getMoneyCtx,
  groupDiscountText,
  money2,
  moneyAxis,
  moneyOf,
  otherMoneyOf,
} from "@/lib/money";
import type { Modality, PricingModel, UsageItem } from "@/types";

/**
 * 余额不足提示阈值（**微**，= 6 个结算货币单位）。
 *
 * 必须**每次现算**，不能写成模块级常量：模块加载发生在接口回来之前，
 * 那一刻 money 上下文还是默认值（scale=1e6）。今天两者恰好相等，
 * 但只要 scale 由后端下发这件事还有意义，模块级求值就是「用启动时的刻度
 * 去量运行时的金额」—— 这正是上一版 3000000 quota 漂移的同款病。
 */
function lowBalanceMicro(): number {
  return 6 * getMoneyCtx().scale;
}

const RANGE_ITEMS = [
  { key: "7", label: "7 天" },
  { key: "30", label: "30 天" },
];

/** 快捷操作（原型动作语义；兑换码直达充值页兑换区；「创建 API KEY」单独处理以携带 ?create=1） */
const QUICK_ACTIONS = [
  { label: "前往充值", icon: "wallet", to: "/topup", hash: undefined },
  { label: "兑换码", icon: "zap", to: "/topup", hash: "redeem" },
  { label: "调用记录", icon: "rows", to: "/usage", hash: undefined },
  { label: "上传文件", icon: "doc", to: "/files", hash: undefined },
  { label: "模型广场", icon: "grid", to: "/models", hash: undefined },
] as const;

function greeting(): string {
  const h = new Date().getHours();
  if (h < 12) return "早晨好";
  if (h < 18) return "下午好";
  return "晚上好";
}

/** 页头 desc：原型由脚本填充当日日期（home.html L69） */
function todayText(): string {
  return new Date().toLocaleDateString("zh-CN", {
    year: "numeric",
    month: "long",
    day: "numeric",
    weekday: "long",
  });
}

const RECENT_COLUMNS: ColumnDef<UsageItem, unknown>[] = usageColumns<UsageItem>({
  statusMap: usageStatusKey,
});

/**
 * 门户主页（S15 §4.13.1 #3）：Bento 工作台。
 * 数据：余额/冻结 ← /portal/me（单一真相源）；今日/本月/趋势/在途 ← /portal/overview；
 * 最近调用 ← /portal/usage（复用既有端点，不另建明细接口）。
 */
export function HomePage() {
  const navigate = useNavigate();
  const { setOpen: setNoticeOpen } = useNoticeOpen();
  const isMobile = useIsMobile();
  const [range, setRange] = useState("7");
  const days = Number(range);

  const me = useMeQuery();
  const overview = usePortalOverviewQuery(days);
  const ann = useAnnouncementsQuery(true);
  const recent = usePortalUsageQuery({ page: 1, page_size: 5 });
  const defaults = usePortalDefaultsQuery();
  // pricing 在这里**只用于查图标与品牌色**，不再判断「谁是默认模型」——
  // 那个事实的唯一真相源是 /portal/defaults（pricing.models.is_default 没有任何写入口，
  // 从 /api/pricing 里筛它永远筛不出东西）。
  const pricing = usePricingQuery();

  // 分组折扣从 /portal/me 的单值取（公开 /api/pricing 已裁剪整张折扣表）。
  const discount = me.data?.group_discount ?? getMoneyCtx().scale;
  const discountText = groupDiscountText(discount);
  const balance = me.data?.balance ?? 0;
  const frozen = me.data?.frozen ?? 0;
  // 必须等 /portal/me 返回后再判定：加载期余额缺省为 0，否则会闪出一条错误的「余额不足」横幅
  const lowBalance = me.data !== undefined && balance < lowBalanceMicro();

  // 「最新公告」= 最新一条**未读**（items 已按发布时间倒序）。旧判据是 id>0，
  // 于是读过的公告永远挂在首屏；正文也从「原样注入 HTML」改为转义渲染。
  const annItems = ann.data?.enabled ? ann.data.items : [];
  const latestUnread = annItems.find((i) => !i.read) ?? null;
  const annUnread = unreadOf(annItems);

  const trend = useMemo(
    () =>
      (overview.data?.trend ?? []).map((p) => ({
        date: p.date.slice(5),
        消耗: amountNum(p.charge),
        充值: amountNum(p.recharge),
      })),
    [overview.data],
  );

  const today = overview.data?.today;
  const month = overview.data?.month;
  const activeTasks = overview.data?.active_tasks ?? 0;
  const keyTotal = me.data?.key_count ?? 0;
  const keyEnabled = me.data?.key_count_enabled ?? 0;
  // 类型分布与「最近调用」同源：`/portal/usage` 一次请求同时给出 items 与 modality_dist，
  // 所以迁到首页不新增任何请求。条宽分母取「最大项」而不是总额 —— 与迁移前调用记录页
  // 的计算完全一致，本次只搬位置、不改口径。
  const dist = recent.data?.modality_dist ?? [];
  const maxCharge = dist.reduce((m, d) => Math.max(m, d.charge), 0);

  // 三态。error 优先：有 placeholderData 时「重新取数失败」会是 isPending=false + isError=true，
  // 反过来（首次失败）两者都为真 —— 先判 error 才不会把失败说成「加载中」。
  const overviewStatus: StatCardStatus = overview.isError
    ? "error"
    : overview.isPending
      ? "loading"
      : "ready";
  const meStatus: StatCardStatus = me.isError ? "error" : me.isPending ? "loading" : "ready";

  /** 默认模型（唯一真相源 = /portal/defaults）：只列真的配了模型的模态。 */
  const defaultRows = useMemo(() => {
    const rows: { modality: string; model: string; allowed: boolean }[] = [];
    for (const [modality, v] of Object.entries(defaults.data ?? {})) {
      if (!v?.model) continue;
      rows.push({ modality, model: v.model, allowed: v.any_key_allowed });
    }
    return rows;
  }, [defaults.data]);

  /** 图标查找表（纯展示，不参与「谁是默认模型」的判定）。 */
  const modelMeta = useMemo(() => {
    const m = new Map<string, PricingModel>();
    for (const it of pricing.data?.models ?? []) m.set(`${it.modality}/${it.id}`, it);
    return m;
  }, [pricing.data]);

  return (
    <>
      <PageHead
        title={`${greeting()}，${me.data?.username ?? ""}`}
        desc={todayText()}
        actions={[
          {
            type: "btn",
            label: "创建 API KEY",
            icon: "plus",
            variant: "primary",
            onClick: () => navigate({ to: "/keys", search: { create: 1 } }),
          },
        ]}
      />

      {latestUnread ? (
        <div className="alert-banner info">
          <span>
            <b>最新公告</b>
            {/* 转义后渲染：公告正文由管理员填写，原样注入 HTML 就是一个存储型 XSS 面 */}
            <span
              className="nt-md"
              dangerouslySetInnerHTML={{ __html: renderInline(latestUnread.content) }}
            />
            <span className="muted small">· {noticeTime(latestUnread.publishDate)}</span>
            {annUnread > 1 ? <span className="muted small">· 共 {annUnread} 条未读</span> : null}
          </span>
          <button type="button" className="btn" onClick={() => setNoticeOpen(true)}>
            查看
          </button>
        </div>
      ) : null}

      {lowBalance ? (
        <div className="alert-banner warn">
          <span>
            账户余额不足（当前 <b className="mono">{moneyOf(balance)}</b>
            ），低于提示阈值 {money2(lowBalanceMicro())}。请及时充值，避免调用中断。
          </span>
          <Link className="btn" to="/topup">
            去充值
          </Link>
        </div>
      ) : null}

      <div className="bento">
        <div className="balance-card span-2">
          <div className="bl">
            账户余额（分组{" "}
            <span className="badge badge-info">
              {me.data?.group ?? "default"} · {discountText}
            </span>
            ）
          </div>
          <div className="bv">{me.isPending || me.isError ? "—" : money2(balance)}</div>
          <div className="bs">
            {me.isPending || me.isError ? (
              <span className="muted">{me.isError ? "余额加载失败" : "余额加载中…"}</span>
            ) : (
              <>
                <span>≈ {otherMoneyOf(balance, 2)}</span>
                <span className="muted">
                  在途异步任务{" "}
                  <b className="mono">
                    {overview.isPending || overview.isError ? "—" : formatNum(activeTasks)}
                  </b>{" "}
                  个 · 冻结 <b className="mono">{moneyOf(frozen)}</b>，完成后多退少补
                </span>
                <span className="muted">
                  累计消费 <b className="mono">{moneyOf(me.data?.used ?? 0)}</b> · 累计结算{" "}
                  <b className="mono">{formatNum(me.data?.request_count ?? 0)}</b> 次
                </span>
              </>
            )}
          </div>
          <div className="bbtns">
            <Link className="btn" to="/topup">
              充值
            </Link>
            <Link className="btn ghost" to="/topup" hash="redeem">
              兑换码
            </Link>
          </div>
        </div>

        {/* 四个指标合并成一张 2 栏大卡，与左侧资金卡同排同高。
            四格仍是 StatCard（各带独立三态与重试），外观由 .kpi-grid 中和成
            「一块面板切四格」——卡里再放四张带边框的小卡会出现两圈边框两层阴影。 */}
        <Card className="span-2 kpi-card" testId="kpi-card">
          <div className="kpi-grid">
            <StatCard
              icon="rows"
              label="今日请求"
              status={overviewStatus}
              onRetry={() => void overview.refetch()}
              value={formatNum(today?.requests ?? 0)}
              sub={
                today && today.requests > 0 ? (
                  todaySub(today)
                ) : today ? (
                  <>
                    今日暂无调用 ·{" "}
                    <Link to="/models" className="small">
                      去模型广场
                    </Link>
                  </>
                ) : null
              }
            />
            <StatCard
              icon="wallet"
              label="今日费用"
              status={overviewStatus}
              onRetry={() => void overview.refetch()}
              value={moneyOf(today?.charge ?? 0)}
              sub={chargeSub(today, overview.data?.generated_at)}
            />
            <StatCard
              icon="doc"
              label="本月费用"
              status={overviewStatus}
              onRetry={() => void overview.refetch()}
              value={moneyOf(month?.charge ?? 0)}
              sub={monthSub(month, overview.data?.month_recharge)}
            />
            <StatCard
              icon="key"
              label="API KEY"
              status={meStatus}
              onRetry={() => void me.refetch()}
              value={`${formatNum(keyEnabled)} 个启用`}
              sub={
                keyTotal > 0 ? (
                  <>
                    共 {formatNum(keyTotal)} 个 ·{" "}
                    <Link to="/keys" search={{ create: 0 }} className="small">
                      管理 →
                    </Link>
                  </>
                ) : (
                  <Link to="/keys" search={{ create: 1 }} className="small">
                    尚未创建 · 立即创建 →
                  </Link>
                )
              }
            />
          </div>
        </Card>

        <Card
          title="用量趋势"
          className="span-4"
          testId="trend-card"
          actions={
            <Tabs
              variant="pill"
              items={RANGE_ITEMS}
              value={range}
              onChange={setRange}
              idPrefix="portal-home-range"
              label="趋势区间"
            />
          }
        >
          {overview.isPending ? (
            <EmptyState variant="loading" title="加载中…" />
          ) : overview.isError ? (
            <EmptyState
              variant="error"
              title="趋势数据加载失败"
              action={
                <button className="btn" type="button" onClick={() => void overview.refetch()}>
                  重试
                </button>
              }
            />
          ) : (
            <TrendChart
              data={trend}
              xKey="date"
              // 整行后 200 会显得扁平；移动端屏窄，保持 200 避免图表高过一屏内容
              height={isMobile ? 200 : 280}
              // 两个系列都必须显式给 stackId=undefined（默认即 undefined）：
              // 误设会把「消耗 + 充值」叠成一条和，图形看起来完全正常、数值完全错误。
              showLegend
              yTickFormatter={moneyAxis}
              valueFormatter={moneyAxis}
              // 系列名必须随结算币种走：写死「美元」在人民币站上是纯错误信息。
              series={[
                { key: "消耗", name: `消耗（${currentSymbol()}）` },
                { key: "充值", name: `充值（${currentSymbol()}）` },
              ]}
              empty={
                <EmptyState
                  icon="◇"
                  title="这段时间没有消耗或充值"
                  desc="调用模型或充值后，这里会出现走势。"
                />
              }
            />
          )}
        </Card>

        {/* 类型分布：从「调用记录」页搬来（那里它独占一行，但属于概览信息）。
            放在趋势下方形成「趋势 → 分布」的连续分析区，且 span-4 与相邻整行卡对齐，
            不会在 4 列网格里留空洞。条宽沿用「相对最大项」，不借迁移改语义。
            副标题刻意不写天数：窗口由系统配置 usage_default_window_days 决定（默认 30，
            可被管理员改），前端拿不到真实值，写死就是一句迟早过期的谎话。 */}
        <Card
          title="类型分布"
          sub="按消耗排序 · 与调用记录同口径"
          className="span-4"
          testId="modality-dist"
        >
          {recent.isPending ? (
            <EmptyState variant="loading" title="正在统计…" />
          ) : recent.isError ? (
            // 失败绝不能说成「没有调用」：与「最近调用」卡同一规矩（e2e T18 锁死）。
            <EmptyState
              variant="error"
              title="类型分布加载失败"
              action={
                <button className="btn" type="button" onClick={() => void recent.refetch()}>
                  重试
                </button>
              }
            />
          ) : dist.length ? (
            dist.map((d) => (
              <div className="share-cell count-line" key={d.modality}>
                <span>
                  <ModalityTag modality={d.modality} />
                </span>
                <div className="progress">
                  <i
                    style={{
                      width: `${maxCharge > 0 ? Math.round((d.charge / maxCharge) * 100) : 0}%`,
                    }}
                  />
                </div>
                <span className="num small">{d.requests} 次</span>
                <QuotaUnit micro={d.charge} />
              </div>
            ))
          ) : (
            <EmptyState
              icon="◇"
              title="这段时间没有调用记录"
              desc="调用任意模型后，这里会按类型汇总消耗。"
            />
          )}
        </Card>

        <Card title="快捷操作" className="span-2">
          <div className="quick-grid">
            <Link to="/keys" search={{ create: 1 }}>
              <span className="qi">
                <Icon name="plus" size={17} />
              </span>
              创建 API KEY
            </Link>
            {QUICK_ACTIONS.map((q) => (
              <Link key={q.label} to={q.to} hash={q.hash}>
                <span className="qi">
                  <Icon name={q.icon} size={17} />
                </span>
                {q.label}
              </Link>
            ))}
          </div>
        </Card>

        <Card title="各类型默认模型" className="span-2" testId="default-models">
          {defaults.isPending ? (
            <EmptyState variant="loading" title="正在加载默认模型…" />
          ) : defaults.isError ? (
            <EmptyState
              variant="error"
              title="默认模型加载失败"
              desc="计费目录暂时不可用，请稍后重试"
              action={
                <button
                  type="button"
                  className="btn btn-primary"
                  onClick={() => void defaults.refetch()}
                >
                  重试
                </button>
              }
            />
          ) : defaultRows.length ? (
            <div className="defaults-grid">
              {defaultRows.map((r) => {
                const meta = modelMeta.get(`${r.modality}/${r.model}`);
                return (
                  <div key={r.modality} className="flex-between">
                    {/* 首页只列 id 的话，非技术用户认不出这是哪个模型；
                        icon 取自计费目录（纯展示），与「谁是默认模型」无关 —— 后者只看 /portal/defaults。 */}
                    <EntityCell
                      iconUrl={meta?.icon_url}
                      name={r.model}
                      color={meta?.vendor_color}
                      size="xs"
                      mono
                      className="small"
                    >
                      <ModalityTag modality={r.modality as Modality} />
                    </EntityCell>
                    {r.allowed ? null : (
                      <Link
                        to="/keys"
                        search={{ create: 0 }}
                        className="badge badge-warning"
                        title="当前密钥未开放该模型"
                      >
                        ⚠ 未授权
                      </Link>
                    )}
                  </div>
                );
              })}
              <div className="flex-between">
                <span />
                <Link className="small" to="/models">
                  查看模型广场 →
                </Link>
              </div>
            </div>
          ) : (
            <EmptyState icon="◇" title="暂未配置默认模型" desc="可在模型广场查看全部可用模型。" />
          )}
        </Card>

        <Card
          title="最近调用"
          className="span-4"
          actions={
            <Link className="btn btn-sm" to="/usage">
              全部记录 →
            </Link>
          }
        >
          <DataTable
            columns={RECENT_COLUMNS}
            data={recent.data?.items ?? []}
            loading={recent.isPending}
            error={
              <EmptyState
                variant="error"
                title="最近调用加载失败"
                action={
                  <button className="btn" type="button" onClick={() => void recent.refetch()}>
                    重试
                  </button>
                }
              />
            }
            empty={
              <EmptyState
                title="今日暂无调用"
                desc="调用任意模型后，这里会显示最近 5 条记录。"
              />
            }
            onRowClick={() => navigate({ to: "/usage" })}
          />
        </Card>
      </div>
    </>
  );
}

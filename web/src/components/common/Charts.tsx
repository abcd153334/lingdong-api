import { useId, type ReactNode } from "react";

import { initialOf } from "@/lib/utils";
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  ResponsiveContainer,
  Tooltip as ReTooltip,
  XAxis,
  YAxis,
} from "recharts";

export interface SeriesDef {
  key: string;
  name: string;
  /** 取色：默认主渐变（var(--primary)/var(--m2)），可传第二色 */
  color?: string;
  /** 堆叠 id（同组堆叠） */
  stackId?: string;
}

export interface TrendChartProps {
  data: Record<string, string | number>[];
  xKey: string;
  series: SeriesDef[];
  height?: number;
  /** Y 轴刻度格式化（如金额加币种符号、上万换「万」）。 */
  yTickFormatter?: (value: number) => string;
  /** Tooltip 数值格式化。 */
  valueFormatter?: (value: number) => string;
  /** 是否在底部渲染图例（多系列时必须开，否则两条线无法区分）。 */
  showLegend?: boolean;
  /** 全部系列都为 0（或 data 为空）时渲染的内容。不给则照旧画空坐标轴。 */
  empty?: ReactNode;
}

const GRID = "var(--border)";
const AXIS = { fontSize: 11, fill: "var(--t3)" };

function axisTick() {
  return AXIS;
}

/** 所有系列在整段区间上都为 0（或缺失）。 */
function seriesAllZero(
  data: Record<string, string | number>[],
  series: SeriesDef[],
): boolean {
  if (data.length === 0) return true;
  // Number(undefined) 是 NaN，`!NaN` 为 true —— 缺键天然按 0 处理，不需要额外判空。
  return data.every((row) => series.every((s) => !Number(row[s.key])));
}

/** 渐变面积趋势图（Recharts 封装；S11 接数据） */
export function TrendChart({
  data,
  xKey,
  series,
  height = 220,
  yTickFormatter,
  valueFormatter,
  showLegend,
  empty,
}: TrendChartProps) {
  // 渐变 id 必须逐实例唯一（D14）：同页多个 TrendChart 共用静态 id 会触发重复 id 与填充串台
  const uid = useId().replace(/[^a-zA-Z0-9]/g, "");
  const areaId = `ldArea-${uid}`;
  const areaAltId = `ldAreaAlt-${uid}`;

  if (empty !== undefined && seriesAllZero(data, series)) {
    return <>{empty}</>;
  }

  // 图例占约 30px（12px 字 + 上间距 + 行高）：从绘图区里扣掉，否则会撑破固定高度。
  const plotHeight = showLegend ? Math.max(80, height - 30) : height;

  return (
    <div className="chart" style={{ height }}>
      <ResponsiveContainer width="100%" height={plotHeight}>
        <AreaChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
          <defs>
            <linearGradient id={areaId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--primary)" stopOpacity={0.34} />
              <stop offset="100%" stopColor="var(--primary)" stopOpacity={0.02} />
            </linearGradient>
            <linearGradient id={areaAltId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--m2)" stopOpacity={0.3} />
              <stop offset="100%" stopColor="var(--m2)" stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid stroke={GRID} strokeDasharray="3 3" vertical={false} />
          <XAxis dataKey={xKey} tick={axisTick()} tickLine={false} axisLine={{ stroke: GRID }} />
          {/* 宽 56 而不是 44：`$1,234.56` 在 44px 下会被裁掉末尾，看起来像 `$1,234.5` */}
          <YAxis
            tick={axisTick()}
            tickLine={false}
            axisLine={false}
            width={56}
            tickFormatter={yTickFormatter}
          />
          <ReTooltip
            contentStyle={{
              background: "var(--card)",
              border: "1px solid var(--border)",
              borderRadius: 10,
              fontSize: 12,
            }}
            labelStyle={{ color: "var(--t3)" }}
            formatter={
              valueFormatter
                ? (value: number | string) => valueFormatter(Number(value))
                : undefined
            }
          />
          {series.map((s, i) => (
            <Area
              key={s.key}
              type="monotone"
              dataKey={s.key}
              name={s.name}
              stackId={s.stackId}
              stroke={s.color ?? (i === 0 ? "var(--primary)" : "var(--m2)")}
              strokeWidth={2}
              fill={i === 0 ? `url(#${areaId})` : `url(#${areaAltId})`}
            />
          ))}
        </AreaChart>
      </ResponsiveContainer>
      {showLegend && series.length > 0 ? (
        <div className="legend">
          {series.map((s, i) => (
            <span key={s.key}>
              <i
                style={{ background: s.color ?? (i === 0 ? "var(--primary)" : "var(--m2)") }}
              />
              {s.name}
            </span>
          ))}
        </div>
      ) : null}
    </div>
  );
}

export interface BarsChartProps {
  data: Record<string, string | number>[];
  xKey: string;
  series: SeriesDef[];
  height?: number;
}

/** 竖向柱状图（Recharts 封装；S11 接数据） */
export function BarsChart({ data, xKey, series, height = 200 }: BarsChartProps) {
  return (
    <div className="chart" style={{ height }}>
      <ResponsiveContainer width="100%" height="100%">
        <BarChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
          <CartesianGrid stroke={GRID} strokeDasharray="3 3" vertical={false} />
          <XAxis dataKey={xKey} tick={axisTick()} tickLine={false} axisLine={{ stroke: GRID }} />
          <YAxis tick={axisTick()} tickLine={false} axisLine={false} width={44} />
          <ReTooltip
            cursor={{ fill: "var(--primary-bg)" }}
            contentStyle={{
              background: "var(--card)",
              border: "1px solid var(--border)",
              borderRadius: 10,
              fontSize: 12,
            }}
          />
          {series.map((s, i) => (
            <Bar
              key={s.key}
              dataKey={s.key}
              name={s.name}
              stackId={s.stackId}
              radius={[6, 6, 0, 0]}
              fill={s.color ?? (i === 0 ? "var(--primary)" : "var(--m2)")}
            />
          ))}
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

/** 排行条的一项：`iconUrl` 是实体图标地址（模型榜/渠道榜有，用户榜没有）。 */
export interface RankBarItem {
  label: string;
  value: number;
  /** 图标地址（后端下发的 `icon_url`；空 = 用色点 + 首字） */
  iconUrl?: string;
}

/**
 * 排行条 Y 轴刻度：图标 + 名称。
 *
 * 为什么要在刻度上画图：这一栏回答的是"哪个模型/渠道"，而模型名往往长到被截断
 * （`kling-video-v2-master`），此时图标是唯一能一眼认出来的东西。
 *
 * 刻度是 SVG，只能用 `<image>`（`<img>` 进不去），因此不能直接复用 EntityIcon；
 * 地址仍然只用后端下发的 `icon_url`，不在前端另做一套解析。
 */
function rankTick(data: RankBarItem[]) {
  const byLabel = new Map(data.map((d) => [d.label, d]));
  return (props: { x: number; y: number; payload: { value: string } }) => {
    const { x, y, payload } = props;
    const name = payload.value;
    const url = byLabel.get(name)?.iconUrl ?? "";
    // 刻度宽度固定 140，图标占右侧 16，文字最多留 100 左右
    const short = name.length > 11 ? `${name.slice(0, 10)}…` : name;
    return (
      <g transform={`translate(${x},${y})`}>
        <text x={-22} y={0} dy={4} textAnchor="end" fontSize={12} fill="var(--t2)">
          {short}
        </text>
        {url ? (
          <image href={url} x={-18} y={-7} width={14} height={14} preserveAspectRatio="xMidYMid meet" />
        ) : (
          <>
            <circle cx={-11} cy={0} r={7} fill="var(--primary)" />
            <text x={-11} y={0} dy={3.5} textAnchor="middle" fontSize={9} fill="#fff">
              {initialOf(name)}
            </text>
          </>
        )}
      </g>
    );
  };
}

export interface RankBarsProps {
  data: RankBarItem[];
  height?: number;
  /** 名称后缀（如「次」这类量词）。**金额不要用它**：币种符号是前缀，见 format。 */
  unit?: string;
  /**
   * 自定义数值文本（tooltip 用）。
   *
   * 金额必须走这里而不是 `unit`：`unit="¥"` 会渲染成「12.34¥」——
   * 中文站点的习惯是「¥12.34」，而把符号当后缀在美式界面里等于把 $12.34 写成 12.34$。
   */
  format?: (v: number) => string;
}

/** 横向排行条（Recharts 封装；S11 接数据） */
export function RankBars({ data, height = 220, unit, format }: RankBarsProps) {
  return (
    <div className="chart" style={{ height }}>
      <ResponsiveContainer width="100%" height="100%">
        <BarChart data={data} layout="vertical" margin={{ top: 4, right: 16, bottom: 4, left: 4 }}>
          <XAxis type="number" hide />
          <YAxis
            type="category"
            dataKey="label"
            // 判据是「有没有可绘制的地址」，不是「字段在不在」：后端对模型/渠道榜
            // 恒定下发该键，但值可能是空串（这家没配图标）—— 那时画一排色点没有意义。
            tick={data.some((d) => Boolean(d.iconUrl)) ? rankTick(data) : axisTick()}
            tickLine={false}
            axisLine={false}
            width={140}
          />
          <ReTooltip
            cursor={{ fill: "var(--primary-bg)" }}
            contentStyle={{
              background: "var(--card)",
              border: "1px solid var(--border)",
              borderRadius: 10,
              fontSize: 12,
            }}
            formatter={(v: number) => (format ? format(v) : `${v}${unit ?? ""}`)}
          />
          <Bar dataKey="value" radius={[0, 7, 7, 0]} fill="var(--primary)">
            {data.map((d, i) => (
              <Cell key={d.label} fillOpacity={1 - i * 0.08} />
            ))}
          </Bar>
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

import type { ColumnDef } from "@tanstack/react-table";
import type { ReactNode } from "react";
import { Elapsed } from "@/components/common/Elapsed";
import { EntityCell } from "@/components/common/EntityCell";
import { QuotaUnit } from "@/components/common/MoneyText";
import { RowActions, type RowAction } from "@/components/common/RowActions";
import { LogKindTag, ModalityTag, StatusTag } from "@/components/common/StatusTag";
import { formatCent, formatDateTime, formatNum } from "./format";
import { cn } from "./utils";

/**
 * 列的展示元数据（与 `components/common/DataTable.tsx` 的 `TableColumnMeta` 一一对应）。
 * 这里只是声明处，落到 DOM 的规则也在 DataTable 一处。
 */
interface ColOpts {
  /** 内容包裹类（mono / num / muted …） */
  className?: string;
  /** 允许换行（说明 / 备注这类长文本列）；单元格默认不换行 */
  wrap?: boolean;
  /** 1 = 始终可见；2 = ≤1023px 隐藏；3 = ≤1199px 隐藏 */
  priority?: 1 | 2 | 3;
  /** 操作列吸右 */
  pinRight?: boolean;
  align?: "left" | "right";
  /** 关键列最小宽度（px）：名称/模型 id 这类列被压塌就是「竖排单字」的根因 */
  minWidth?: number;
}

/** 列渲染统一出口：`meta.label` 同时用于表头文案与移动端 `data-label` */
function col<T>(
  id: string,
  label: string,
  accessor: (row: T) => unknown,
  render: (row: T) => ReactNode,
  opts: ColOpts = {},
): ColumnDef<T, unknown> {
  const meta = {
    label,
    wrap: opts.wrap,
    priority: opts.priority,
    pinRight: opts.pinRight,
    align: opts.align,
    minWidth: opts.minWidth,
  };
  return {
    id,
    accessorFn: accessor,
    enableSorting: false,
    meta,
    cell: ({ row }) => {
      const node = opts.className ? <span className={opts.className}>{render(row.original)}</span> : render(row.original);
      // 表格单元格自身的 min-width 会被布局算法忽略，只有块级子元素上的 min-width 才算数
      return opts.minWidth ? (
        <div className="t-card-min" style={{ minWidth: opts.minWidth }}>
          {node}
        </div>
      ) : (
        node
      );
    },
  };
}

/** 等宽文本列 */
export function monoCol<T>(
  id: string,
  label: string,
  get: (row: T) => string,
  opts: { small?: boolean; muted?: boolean; priority?: 1 | 2 | 3; minWidth?: number } = {},
): ColumnDef<T, unknown> {
  return col(id, label, get, (r) => get(r), {
    className: cn("mono", opts.small && "small", opts.muted && "muted"),
    priority: opts.priority,
    minWidth: opts.minWidth,
  });
}

/** 时间列（`YYYY-MM-DD HH:mm:ss`，等宽） */
export function timeCol<T>(
  get: (row: T) => string,
  opts: { label?: string; id?: string; muted?: boolean; priority?: 1 | 2 | 3 } = {},
): ColumnDef<T, unknown> {
  const { label = "时间", id = "created_at", muted, priority } = opts;
  return monoCol(id, label, (r) => formatDateTime(get(r)), { small: true, muted, priority });
}

/**
 * 模型名列（等宽 + 图标）。
 *
 * `iconUrlOf` 传的是**地址**（后端下发的 `*_icon_url`），不是 token：
 * 地址规则只在后端一处，前端不拼路径。
 * 但那一列就退回"只有一串字符"——而同一个模型在别的页面有图。
 * 这里刻意不给默认值，让"没有图标"成为一个需要显式选择的决定。
 */
export function modelCol<T>(
  get: (row: T) => string,
  label = "模型",
  id = "model",
  opts: { iconUrlOf?: (row: T) => string | undefined; minWidth?: number } = {},
): ColumnDef<T, unknown> {
  return renderCol(
    id,
    label,
    get,
    (r) => <EntityCell iconUrl={opts.iconUrlOf?.(r)} name={get(r)} mono />,
    { minWidth: opts.minWidth ?? 180 },
  );
}

/** 模态标签列 */
export function modalityCol<T>(get: (row: T) => string, label = "类型", id = "modality"): ColumnDef<T, unknown> {
  return col(id, label, get, (r) => <ModalityTag modality={get(r)} />, {  });
}

/** 状态标签列（`map` 用于把后端原始状态映射为 StatusTag 的键） */
export function statusCol<T>(
  get: (row: T) => string,
  opts: { label?: string; id?: string; map?: (status: string) => string; priority?: 1 | 2 | 3 } = {},
): ColumnDef<T, unknown> {
  const { label = "状态", id = "status", map, priority } = opts;
  return col(id, label, get, (r) => <StatusTag status={map ? map(get(r)) : get(r)} />, {
    priority,
  });
}

/**
 * 费用列（微 → 结算币种金额）。
 *
 * `currency` 是可选的**行内快照**访问器：调用记录这类历史数据必须传，
 * 否则一条美元时代记录会用当前币种的符号展示 —— 金额对、单位错，
 * 比数字错更难发现。
 */
export function moneyCol<T>(
  get: (row: T) => number | null | undefined,
  opts: {
    label?: string;
    id?: string;
    currency?: (row: T) => string | undefined;
    digits?: number;
  } = {},
): ColumnDef<T, unknown> {
  const { label = "费用", id = "charge", currency, digits } = opts;
  return col(id, label, get, (r) => <QuotaUnit micro={get(r)} currency={currency?.(r)} digits={digits} />, {
    align: "right",
  });
}

/** 订单金额列（入参为「分」，展示 `¥x.xx`） */
export function centCol<T>(
  get: (row: T) => number,
  opts: { label?: string; id?: string; currency?: (row: T) => string | undefined } = {},
): ColumnDef<T, unknown> {
  const { label = "金额", id = "amount_cent", currency } = opts;
  return col(id, label, get, (r) => formatCent(get(r), currency?.(r)), {
    className: "mono",
    align: "right",
  });
}

/** 数字列（千分位，最多 digits 位小数） */
export function numCol<T>(
  get: (row: T) => number,
  opts: { label: string; id: string; digits?: number; priority?: 1 | 2 | 3 },
): ColumnDef<T, unknown> {
  return col(opts.id, opts.label, get, (r) => formatNum(get(r), opts.digits), {
    className: "num small",
    align: "right",
    priority: opts.priority,
  });
}

/**
 * 自定义渲染列：页面需要「名称 + 角标」这类组合单元格时用它，
 * 而不是在页面里手写 ColumnDef 字面量 —— 列定义与 `meta.label`（移动端 data-label）必须单源。
 */
export function renderCol<T>(
  id: string,
  label: string,
  get: (row: T) => unknown,
  render: (row: T) => ReactNode,
  opts: Omit<ColOpts, "className"> = {},
): ColumnDef<T, unknown> {
  return col(id, label, get, render, opts);
}

/**
 * 操作列：**唯一**入口是 `RowActions` 的动作数组。
 *
 * 不再接受任意 ReactNode —— 那是「同一个删除在十个页面十个长相」的来源：
 * 三套容器（.row-acts / .ph-acts / .ph-acts--wrap）× 四种按钮样式。现在按钮的尺寸、
 * 图标、悬停提示、禁用原因、行点击隔离都只在 `RowActions` 里定义一次。
 */
export function actionsCol<T>(
  actions: (row: T) => RowAction[] | null,
  label = "操作",
  id = "act",
): ColumnDef<T, unknown> {
  return col(id, label, () => null, (row) => {
    const list = actions(row);
    // 没有可用动作时给「—」而不是空白：空单元格会被误读成「渲染失败」，
    // 而这一列在任何宽度下都占着一格，留白反而最扎眼。
    return list?.length ? <RowActions actions={list} /> : <span className="muted">—</span>;
  }, { pinRight: true });
}

/* ------------------------------ 等价组：订单 ------------------------------ */

/** 订单行公共字段（管理台与门户钱包同一口径） */
export interface OrderRowLike {
  out_trade_no: string;
  /** 订单金额（分）：支付渠道量纲，不绕微 */
  amount_cent: number;
  /** 到账金额（微）：已含赠送 */
  credited: number;
  status: string;
  created_at: string;
  /** 订单币种（可选；下单时固化，缺省回落当前结算币种） */
  currency?: string;
}

/**
 * 订单表公共列：订单号 [用户] 金额 到账 状态 [支付渠道] 创建时间 [操作]
 * `user` / `payChannel` / `actions` 均为可选扩展位（管理台用前两者，门户钱包用 actions）。
 *
 * 「下单时比例」列已删除 —— 倍率与刻度都不存在了，订单金额就是实付金额，
 * 到账金额就是用户到手的钱，多一列解释反而让对账多一处可疑。
 */
export function orderColumns<T extends OrderRowLike>(extras: {
  user?: (row: T) => ReactNode;
  /**
   * 所属应用（管理台传；门户不传 → 门户列不变）。
   *
   * 必须是**可选槽位**而不是常驻列：门户钱包里的订单全是"平台直连"，
   * 给它一列应用只会多出一列全是「—」的噪音。
   */
  app?: (row: T) => ReactNode;
  /** 套餐名（管理台传；门户不传 → 门户列不变） */
  packageName?: (row: T) => ReactNode;
  payChannel?: (row: T) => ReactNode;
  actions?: (row: T) => RowAction[] | null;
} = {}): ColumnDef<T, unknown>[] {
  const { user, app, packageName, payChannel, actions } = extras;
  return [
    monoCol<T>("out_trade_no", "订单号", (r) => r.out_trade_no, { small: true, minWidth: 170 }),
    ...(user ? [renderCol<T>("user", "用户", () => null, user, { priority: 3 })] : []),
    ...(app ? [renderCol<T>("app", "应用", () => null, app, { priority: 3 })] : []),
    centCol<T>((r) => r.amount_cent, { currency: (r) => r.currency }),
    moneyCol<T>((r) => r.credited, {
      label: "到账金额",
      id: "credited",
      currency: (r) => r.currency,
      digits: 2,
    }),
    ...(packageName ? [renderCol<T>("package_name", "套餐", () => null, packageName, { priority: 3 })] : []),
    statusCol<T>((r) => r.status),
    ...(payChannel ? [renderCol<T>("pay_channel", "支付渠道", () => null, payChannel, { priority: 3 })] : []),
    timeCol<T>((r) => r.created_at, { label: "创建时间", muted: true, priority: 2 }),
    ...(actions ? [actionsCol<T>(actions)] : []),
  ];
}

/* ---------------------------- 等价组：调用记录 ---------------------------- */

/** 调用记录行公共字段（门户主页「最近调用」与调用记录页同一口径） */
export interface UsageRowLike {
  created_at: string;
  model: string;
  /** 模型图标地址（后端已回落供应商；空 = 首字占位） */
  model_icon_url?: string;
  modality: string;
  prompt_tokens: number;
  completion_tokens: number;
  /** 本次扣费（微） */
  charge: number;
  status: string;
  key_id?: number;
  latency_ms?: number;
  /** 服务端算的已进行毫秒（进行中才需要，缺省回落 latency_ms） */
  elapsed_ms?: number;
  /** 口径标记（含 reclaimed 时耗时显示 ≥） */
  anomaly?: string;
  /** 行内快照币种（历史行必须用它选符号，不能用全局值） */
  currency?: string;
}

/**
 * 调用记录列：时间 / 模型 / 类型 / [密钥] / Token / 费用 / [耗时] / 状态
 * `keyName` 传入即渲染密钥列；`withLatency` 为 true 即渲染耗时列。
 */
export function usageColumns<T extends UsageRowLike>(opts: {
  keyName?: (id: number) => string;
  withLatency?: boolean;
  statusMap?: (status: string) => string;
} = {}): ColumnDef<T, unknown>[] {
  const { keyName, withLatency, statusMap } = opts;
  return [
    timeCol<T>((r) => r.created_at),
    modelCol<T>((r) => r.model, "模型", "model", { iconUrlOf: (r) => r.model_icon_url }),
    modalityCol<T>((r) => r.modality),
    ...(keyName
      ? [monoCol<T>("key", "密钥", (r) => keyName(r.key_id ?? 0), { small: true, priority: 3 })]
      : []),
    col<T>(
      "tokens",
      "Token",
      (r) => r.prompt_tokens + r.completion_tokens,
      (r) => `${formatNum(r.prompt_tokens)} / ${formatNum(r.completion_tokens)}`,
      { className: "num small", align: "right", priority: 2 },
    ),
    moneyCol<T>((r) => r.charge, { currency: (r) => r.currency }),
    // 耗时列必须走 Elapsed：进行中的记录要每秒上涨，终态才是固定值。
    // 直接渲染 `latency_ms` 正是"生成中永远 0ms、刷新也不变"的根因。
    ...(withLatency
      ? [
          col<T>(
            "latency_ms",
            "耗时",
            (r) => r.elapsed_ms ?? r.latency_ms ?? 0,
            (r) => (
              <Elapsed
                elapsedMs={r.elapsed_ms ?? r.latency_ms ?? 0}
                status={r.status}
                anomaly={r.anomaly}
              />
            ),
            { priority: 3 },
          ),
        ]
      : []),
    statusCol<T>((r) => r.status, { map: statusMap }),
  ];
}

/* ---------------------------- 等价组：管理台调用日志 ---------------------------- */

/** 管理台调用日志行（与 AdminLogItem 结构兼容，避免 lib 依赖 types 造成环） */
export interface LogRowLike {
  id: number;
  created_at: string;
  user_id: number;
  username?: string;
  key_name?: string;
  model: string;
  /** 模型图标地址（后端已回落供应商） */
  model_icon_url?: string;
  channel_name?: string;
  /** 渠道图标地址（只存在于管理台契约） */
  channel_icon_url?: string;
  adapter_id?: string;
  modality: string;
  kind: string;
  http_status: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  /** 本次扣费（微） */
  charge: number;
  /** 行内快照币种（历史行必须用它选符号） */
  currency?: string;
  latency_ms: number;
  elapsed_ms?: number;
  /** 后端原始状态（pending/settled/released/rejected），Elapsed 需要它判断是否在跑 */
  status?: string;
  anomaly?: string;
}

/**
 * 管理台调用日志列（时间 / 用户 / KEY / 模型 / 渠道 / 模态 / Tokens / 费用 / 耗时 / 状态 / 操作）。
 *
 * 为什么搬进 lib/table：原先这 11 列是页面内手写的 ColumnDef 字面量，
 * 门禁 M10（列定义零双写）只认 `usageColumns:` 字面量，于是它一直漏过检查 ——
 * 结果是同一个"渠道"单元格在管理台与门户两处写法不同、移动端 data-label 也对不上。
 */
export function logsColumns<T extends LogRowLike>(opts: {
  onDetail: (row: T) => void;
  /** 所属应用（管理台传；门户「最近调用」不传 → 列不变） */
  app?: (row: T) => ReactNode;
}): ColumnDef<T, unknown>[] {
  return [
    timeCol<T>((r) => r.created_at),
    renderCol<T>("username", "用户", (r) => r.username, (r) => (
      <span className="mono">{r.username || `#${r.user_id}`}</span>
    ), { priority: 3 }),
    renderCol<T>("key_name", "KEY", (r) => r.key_name, (r) => (
      <span className="mono muted">{r.key_name || "—"}</span>
    ), { priority: 3 }),
    ...(opts.app ? [renderCol<T>("app", "应用", () => null, opts.app, { priority: 3 })] : []),
    modelCol<T>((r) => r.model, "模型", "model", { iconUrlOf: (r) => r.model_icon_url }),
    renderCol<T>("channel_name", "渠道", (r) => r.channel_name, (r) =>
      r.channel_name ? (
        <EntityCell iconUrl={r.channel_icon_url} name={r.channel_name}>
          {/* 规格渠道一眼可见：否则排障时得先点开详情才知道这条走的是数据驱动规格 */}
          {r.adapter_id?.startsWith("spec:") ? (
            <span className="small mono muted"> · {r.adapter_id.slice(5)}</span>
          ) : null}
        </EntityCell>
      ) : (
        <span className="muted">—</span>
      ),
    { minWidth: 120, priority: 3 }),
    modalityCol<T>((r) => r.modality, "模态"),
    col<T>(
      "total_tokens",
      "Tokens",
      (r) => r.total_tokens,
      (r) => (
        <>
          {formatNum(r.total_tokens)}
          <span className="muted small">
            {" "}
            {formatNum(r.prompt_tokens)}/{formatNum(r.completion_tokens)}
          </span>
        </>
      ),
      { className: "mono", align: "right", priority: 2 },
    ),
    moneyCol<T>((r) => r.charge, { currency: (r) => r.currency }),
    col<T>(
      "latency_ms",
      "耗时",
      (r) => r.elapsed_ms ?? r.latency_ms,
      (r) => <Elapsed elapsedMs={r.elapsed_ms ?? r.latency_ms} status={r.status ?? ""} anomaly={r.anomaly} />,
      { priority: 2 },
    ),
    renderCol<T>("state", "状态", (r) => r.kind, (r) => (
      <>
        <LogKindTag kind={r.kind} /> <StatusTag status={r.http_status} />
      </>
    ), {  }),
    actionsCol<T>((r) => [{ icon: "eye", label: "详情", onClick: () => opts.onDetail(r) }]),
  ];
}

/** 事件类型文案（与后端 kindOf 派生值一一对应）。 */
export function kindLabel(kind: string): string {
  switch (kind) {
    case "consume":
      return "成功";
    case "pending":
      return "进行中";
    case "client_abort":
      return "客户端断开";
    case "upstream_error":
      return "上游错误";
    case "error":
      return "失败";
    case "rate_limited":
      return "预检限流";
    case "auth_error":
      return "预检鉴权失败";
    case "insufficient_balance":
      return "余额不足";
    case "invalid_request":
      return "预检参数错误";
    default:
      return kind || "—";
  }
}

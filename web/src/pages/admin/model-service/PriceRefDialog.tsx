import { useEffect, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";

import { DataTable } from "@/components/common/DataTable";
import { Dialog } from "@/components/common/Dialog";
import { EmptyState } from "@/components/common/EmptyState";
import { Field } from "@/components/common/Field";
import { ModalityTag, Tag } from "@/components/common/StatusTag";
import { useUpstreamPriceQuery } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { formatAmount, formatDateTime, formatNum } from "@/lib/format";
import { defaultSymbol, type Currency } from "@/lib/money";
import type { UpstreamPriceItem } from "@/types";

/** 可填入的价格通道（上游只报这三个价；call/spec 是另一套量纲，绝不混填） */
export type FillLane = "input" | "output" | "cache";

const LANE_CN: Record<FillLane, string> = { input: "输入价", output: "输出价", cache: "缓存读取价" };

/**
 * 上游参考价面板（模型编辑弹窗底栏的「上游参考价」打开它）。
 *
 * 三件事必须说清楚，否则运营会算错毛利：
 *  1. 数据来自 models.dev 公开目录，**不是这条渠道的结算价**（渠道可能有折扣/人民币结算/逆向）；
 *  2. 折算价按站点汇率算出，只作参考，填入后仍由「保存」提交；
 *  3. 上游只报基础档（输入/输出/缓存读取），阶梯价与长上下文价不体现。
 *
 * 嵌套形态：`portal` + `layer={2}`。父弹窗是 `Dialog`（带 transform，会成为 fixed 后代的
 * 包含块），不升到 body 就会被 `.dl-body` 的 overflow 裁掉；叠层后 Esc 只关栈顶（见
 * useOverlayBehavior 的 overlayStack），不会连父表单一起关掉。
 */
export function PriceRefDialog({
  open,
  onClose,
  initialKeyword,
  vendor,
  fillable,
  onFill,
}: {
  open: boolean;
  onClose: () => void;
  /** 打开时自动搜索的词：上游别名 → 模型 ID → 展示名，由调用方按优先级给 */
  initialKeyword: string;
  /** 当前模型所属供应商：只影响同名去重的排序偏好 */
  vendor?: string;
  /** 只有 Token 计费才允许填入（按次/按规格的量纲不同） */
  fillable: boolean;
  onFill: (lane: FillLane, micro: number, item: UpstreamPriceItem) => void;
}) {
  const [kw, setKw] = useState("");
  const [term, setTerm] = useState("");

  // 每次打开都重置：拿着上一个模型的词和结果对着另一个模型看，是这类工具最典型的误操作
  // （旧版上游同步弹窗就栽在这——注释写着"每次重置"，实际只在换来源时重置）。
  useEffect(() => {
    if (!open) return;
    setKw(initialKeyword);
    setTerm(initialKeyword); // 自动搜索不等防抖
  }, [open, initialKeyword]);

  // 手动输入 300ms 防抖；回车立即提交（防抖期间按回车不该再等）。
  useEffect(() => {
    if (!open || kw === term) return;
    const t = setTimeout(() => setTerm(kw), 300);
    return () => clearTimeout(t);
  }, [kw, term, open]);

  const trimmed = term.trim();
  const tooShort = trimmed.length > 0 && trimmed.length < 2;
  const q = useUpstreamPriceQuery({ keyword: term, vendor }, open);
  const data = q.data;
  const meta = data?.meta;

  const money = (micro: number | null): string => {
    if (micro == null) return "—";
    return `${defaultSymbol(meta?.currency ?? "CNY")}${formatAmount(micro)}`;
  };

  const priceCell = (row: UpstreamPriceItem, lane: FillLane, micro: number | null, usd: number | null) => {
    if (usd == null || micro == null) return <span className="muted small">未报价</span>;
    // 0 价不提供填入：本地 0 微的语义是"显式免费"，一次误填就把付费模型变成免费。
    const canFill = fillable && !row.deprecated && micro > 0;
    return (
      <div className="price-ref-cell">
        <span className="mono small">{money(micro)}</span>
        {meta && meta.currency !== "USD" ? (
          <span className="muted small">${formatNum(usd, 4)}</span>
        ) : null}
        {canFill ? (
          <button type="button" className="btn btn-sm" onClick={() => onFill(lane, micro, row)}>
            填入
          </button>
        ) : null}
      </div>
    );
  };

  const columns: ColumnDef<UpstreamPriceItem, unknown>[] = [
    {
      id: "name",
      accessorKey: "name",
      meta: { label: "模型", minWidth: 200 },
      cell: ({ row }) => (
        <div>
          <span className="mono">{row.original.name}</span>
          <div className="muted small">
            {row.original.provider_name}
            {row.original.provider_count > 1 ? ` · 另有 ${row.original.provider_count - 1} 家报价` : ""}
          </div>
          <div>
            <ModalityTag modality={row.original.modality} />
            {row.original.deprecated ? <Tag tone="danger">已停售</Tag> : null}
          </div>
        </div>
      ),
    },
    {
      id: "input",
      accessorKey: "input_micro",
      meta: { label: "输入价", align: "right", minWidth: 96 },
      cell: ({ row }) => priceCell(row.original, "input", row.original.input_micro, row.original.input_usd),
    },
    {
      id: "output",
      accessorKey: "output_micro",
      meta: { label: "输出价", align: "right", minWidth: 96 },
      cell: ({ row }) => priceCell(row.original, "output", row.original.output_micro, row.original.output_usd),
    },
    {
      id: "cache",
      accessorKey: "cache_micro",
      meta: { label: "缓存价", align: "right", minWidth: 96 },
      cell: ({ row }) => priceCell(row.original, "cache", row.original.cache_micro, row.original.cache_usd),
    },
  ];

  const body = () => {
    if (trimmed === "") {
      return (
        <EmptyState
          icon="◇"
          title="输入模型名查询参考价"
          desc="填上游的模型标识更准，例如 gpt-4o；本地改过名的模型也能查。"
        />
      );
    }
    if (tooShort) return <EmptyState title="至少输入 2 个字符" />;
    if (q.isPending && !data) {
      return (
        <EmptyState
          title="查询中…"
          desc="首次查询需要先拉取上游价目表（约 7MB），可能需要几秒。"
        />
      );
    }
    if (q.isError && !data) {
      return (
        <EmptyState
          icon="!"
          title="参考价暂时拉不到"
          desc={
            (q.error instanceof ApiError ? q.error.message : "请稍后重试") +
            "。这是查询上游公开价目的临时故障，不影响模型本身的配置与计费。"
          }
        />
      );
    }
    if (!data) return null;
    if (data.items.length === 0) {
      // 三种"没有结果"必须分开说：查不到 / 收录了但没报价 / 关键词本身无效。
      if (data.meta.total > 0) {
        return (
          <EmptyState
            icon="◇"
            title={`找到 ${data.meta.total} 个同名模型，但都没有公开报价`}
            desc="上游收录了这些模型，只是没有挂牌价；本地价仍按你的配置计费。"
          />
        );
      }
      return (
        <EmptyState
          icon="◇"
          title="没有匹配的参考价"
          desc="试试上游的模型标识，或更短的关键词。上游目录没有收录，不代表本地配置有问题。"
        />
      );
    }
    return (
      <>
        <DataTable columns={columns} data={data.items} />
        {data.meta.total > data.items.length ? (
          <div className="small muted mt16">
            共 {data.meta.total} 个匹配，显示前 {data.items.length} 个
          </div>
        ) : null}
      </>
    );
  };

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="上游参考价"
      size="xl"
      portal
      layer={2}
      footer={
        <button type="button" className="btn" onClick={onClose}>
          关闭
        </button>
      }
    >
      <Field label="搜索模型" htmlFor="up-kw">
        <input
          id="up-kw"
          className="input"
          type="text"
          autoComplete="off"
          placeholder="例如 gpt-4o"
          value={kw}
          onChange={(e) => setKw(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") setTerm(e.currentTarget.value);
          }}
        />
      </Field>

      {/* 口径说明必须常驻可读（不能藏进问号：UI 契约门禁对 tip/hint 是只降不升的） */}
      {meta ? (
        <div className="small muted mt16">
          <span>参考价来自 {meta.source} 公开数据，不是你这条渠道的结算价。</span>
          {meta.stale ? <Tag tone="warning">数据已过期</Tag> : null}
          <div>
            更新于 {formatDateTime(meta.fetched_at)}
            {meta.currency !== "USD" ? ` · 1 美元 ≈ ${formatNum(meta.fx_rate, 2)} 元` : ""}
            {" · 仅基础档单价，阶梯价与长上下文价不体现"}
          </div>
          {meta.matched_keyword !== trimmed ? <div>已按 {meta.matched_keyword} 匹配</div> : null}
          {fillable ? null : <div>当前模型按次或按规格计费，参考价仅用于比价，不能填入。</div>}
        </div>
      ) : null}

      <div className="mt16">{body()}</div>

      {q.isError && data ? (
        <div className="mt16">
          <button type="button" className="btn btn-sm" onClick={() => void q.refetch()}>
            重试
          </button>
        </div>
      ) : null}
    </Dialog>
  );
}

export { LANE_CN };

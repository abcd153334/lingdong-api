import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import dayjs from "dayjs";

import { Card } from "@/components/common/Card";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { ErrorState } from "@/components/common/QueryStates";
import { DataTable } from "@/components/common/DataTable";
import { EmptyState } from "@/components/common/EmptyState";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { PageHead } from "@/components/common/PageHead";
import { StatusTag } from "@/components/common/StatusTag";
import { toastErr, toastOk } from "@/components/common/Toaster";
import {
  exportAdminRedeems,
  useAdminConfigQuery,
  useAdminRedeemDeleteMutation,
  useAdminRedeemGenerateMutation,
  useAdminRedeemsQuery,
  type AdminRedeemsParams,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { formatDateTime } from "@/lib/format";
import { microOf, money2, moneyOf, moneyAt } from "@/lib/money";
import { copyText } from "@/lib/clipboard";
import type { RedeemItem } from "@/types";

/** 单批数量上限（与后端 redeem.MaxGenerateCount 一致；越界不裁剪） */
const MAX_COUNT = 1000;

/** 生成结果卡取码的页大小（后端 page_size 上限 100） */
const RESULT_PAGE_SIZE = 100;

/** 表单字段级错误（键与表单字段对齐，错误显示在各自字段下方） */
interface FormErrors {
  amount?: string;
  count?: string;
  expires?: string;
  batch?: string;
}

/** 批量生成结果：码优先取 POST 响应的 `codes`，缺失时回落到按批次查列表 */
interface GenerateResult {
  batch: string;
  /** 单码面额（微） */
  amount: number;
  count: number;
  /** POST 直接返回的明文码；空数组表示老后端/未返回，交给列表兜底 */
  codes: string[];
}

/** 兑换码管理（S16 §4.3）：批量生成 + 列表筛选 + 批量作废 + 结果导出。
 * 状态一律取后端派生值（redeem.StatusOf），页面不做二次推导。
 *
 * **为什么这一页没有「所属应用」列/筛选**（2026-11 明确）：兑换码是平台级营销
 * 工具 —— 核销入账的是 users.balance（用户全局余额），与订单/用量不同源。
 * 同一张码可被任意应用下的任意用户兑换，事后没有客观载体能反推"它属于哪个应用"。
 * 硬加一个 app_id 只会造出「统计上像应用专属、资金上仍是全局」的假维度。
 * 若将来确需，唯一可行是记录核销时命中的密钥（表结构变更，产品需求驱动）。 */
export function AdminRedeemsPage() {
  const cfg = useAdminConfigQuery();
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<Set<number>>(new Set());

  const [amount, setAmount] = useState("1.00");
  const [count, setCount] = useState("10");
  const [expires, setExpires] = useState("");
  const [batch, setBatch] = useState("");
  const [errors, setErrors] = useState<FormErrors>({});
  const [result, setResult] = useState<GenerateResult | null>(null);

  const params: AdminRedeemsParams = useMemo(
    () => ({ page, page_size: DEFAULT_PAGE_SIZE }),
    [page],
  );

  const list = useAdminRedeemsQuery(params);
  const resultQuery = useAdminRedeemsQuery({
    page: 1,
    page_size: RESULT_PAGE_SIZE,
    batch: result?.batch,
  });
  const gen = useAdminRedeemGenerateMutation();
  const del = useAdminRedeemDeleteMutation();

  const rows = list.data?.items ?? [];
  const total = list.data?.total ?? 0;
  /* 单码上限 = 余额上限：面额超过它，用户兑换后会被上限截断，等于白送一张用不完的码。 */
  const cap = cfg.data?.config.balance_limit ?? 0;
  const amountMicro = microOf(amount);
  const todayStr = dayjs().format("YYYY-MM-DD");
  /** 合计（派生自当前输入）：任一字段不合法就留空，避免把错值算成一个"看起来没问题"的总价 */
  const totalText = useMemo(() => {
    if (amountMicro === null || amountMicro <= 0) return "";
    if (cap > 0 && amountMicro > cap) return "";
    const n = Number(count);
    if (!Number.isInteger(n) || n < 1 || n > MAX_COUNT) return "";
    return `共 ${n} 个 · 合计 ${moneyOf(amountMicro * n)}`;
  }, [amountMicro, count, cap]);
  // 优先用生成响应里的码（不依赖列表分页与缓存时序）；没有再回落到列表查询。
  const resultCodes =
    result && result.codes.length
      ? result.codes
      : (resultQuery.data?.items ?? [])
          .filter((r) => r.batch === result?.batch)
          .map((r) => r.code);

  /* ---------------- 批量生成 ---------------- */

  const validate = (): FormErrors => {
    const e: FormErrors = {};
    if (!amount.trim()) e.amount = "请填写面额";
    else if (amountMicro === null) e.amount = "面额格式不正确（只能填数字，最多 6 位小数）";
    else if (amountMicro <= 0) e.amount = "面额必须大于 0";
    else if (cap > 0 && amountMicro > cap) e.amount = `面额不得超过单码上限 ${money2(cap)}`;
    const countNum = Number(count);
    if (!Number.isInteger(countNum) || countNum < 1 || countNum > MAX_COUNT) {
      e.count = `数量取值范围为 1-${MAX_COUNT}`;
    }
    if (!expires) e.expires = "请设置有效期";
    else if (expires < todayStr) e.expires = "有效期不得早于今日";
    if (!batch.trim()) e.batch = "批次名必填";
    else if (batch.trim().length > 64) e.batch = "批次名不超过 64 字符";
    return e;
  };

  const submitGenerate = () => {
    const e = validate();
    setErrors(e);
    if (Object.values(e).some(Boolean)) return;
    const name = batch.trim();
    gen.mutate(
      { batch: name, amount: amountMicro ?? 0, count: Number(count), expires_at: expires },
      {
        onSuccess: (res) => {
          toastOk(`已生成 ${res.created} 个兑换码（批次：${name}）`);
          setResult({
            batch: name, amount: amountMicro ?? 0, count: res.created,
            codes: res.codes ?? [],
          });
          setPage(1);
          setSelected(new Set());
        },
        onError: (err) => toastErr(err instanceof ApiError ? err.message : "生成失败，请稍后重试"),
      },
    );
  };

  const copyResult = async () => {
    if (await copyText(resultCodes.join("\n"))) toastOk("已复制到剪贴板");
    else toastErr("复制失败，请手动复制");
  };

  const exportResult = async () => {
    if (!result) return;
    try {
      await exportAdminRedeems({ batch: result.batch });
      toastOk("已开始下载 CSV");
    } catch (err) {
      toastErr(err instanceof ApiError ? err.message : "导出失败，请稍后重试");
    }
  };

  /* ---------------- 批量作废 ---------------- */

  const pageIDs = rows.map((r) => r.id);
  const allPageSelected = pageIDs.length > 0 && pageIDs.every((id) => selected.has(id));

  const toggleAllPage = () => {
    setSelected((prev) => {
      if (pageIDs.every((id) => prev.has(id))) {
        const next = new Set(prev);
        pageIDs.forEach((id) => next.delete(id));
        return next;
      }
      return new Set([...prev, ...pageIDs]);
    });
  };

  const toggleOne = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const voidSelected = async () => {
    const ids = [...selected];
    if (!ids.length) return;
    const ok = await confirmDialog({
      title: "作废兑换码",
      body: (
        <>
          确定作废选中的 <b>{ids.length}</b> 个兑换码？仅未使用的码会失效；已兑换并入账的额度不回扣。
        </>
      ),
      okText: "确认作废",
      okVariant: "danger",
    });
    if (!ok) return;
    del.mutate(ids, {
      onSuccess: (res) => {
        toastOk(
          res.skipped > 0
            ? `已作废 ${res.deleted} 个，跳过 ${res.skipped} 个（已使用或不存在）`
            : `已作废 ${res.deleted} 个兑换码`,
        );
        setSelected(new Set());
      },
      onError: (err) => toastErr(err instanceof ApiError ? err.message : "作废失败，请稍后重试"),
    });
  };

  const columns = useMemo<ColumnDef<RedeemItem, unknown>[]>(
    () => [
      {
        id: "sel",
        header: "选择",
        meta: { label: "选择" },
        cell: (c) => (
          <input
            type="checkbox"
            aria-label={`选择兑换码 ${c.row.original.code}`}
            checked={selected.has(c.row.original.id)}
            onChange={() => toggleOne(c.row.original.id)}
          />
        ),
      },
      {
        accessorKey: "code",
        header: "兑换码",
        meta: { label: "兑换码" },
        cell: (c) => <span className="mono small">{c.getValue<string>()}</span>,
      },
      {
        accessorKey: "batch",
        header: "批次",
        meta: { label: "批次", priority: 3 },
        cell: (c) => <span className="small">{c.getValue<string>() || "—"}</span>,
      },
      {
        accessorKey: "amount",
        header: "面额",
        meta: { label: "面额" },
        cell: (c) => <span className="mono">{moneyOf(c.getValue<number>())}</span>,
      },
      {
        accessorKey: "status",
        header: "状态",
        meta: { label: "状态" },
        cell: (c) => <StatusTag status={c.getValue<string>()} />,
      },
      {
        accessorKey: "used_by",
        header: "使用者",
        meta: { label: "使用者", priority: 3 },
        cell: (c) => {
          const v = c.getValue<number | null>();
          return v === null ? <span className="muted">—</span> : <span className="mono">#{v}</span>;
        },
      },
      {
        accessorKey: "used_at",
        header: "使用时间",
        meta: { label: "使用时间", priority: 3 },
        cell: (c) => {
          const v = c.getValue<string | null>();
          return v ? <span className="mono small">{formatDateTime(v)}</span> : <span className="muted">—</span>;
        },
      },
      {
        accessorKey: "expires_at",
        header: "到期时间",
        meta: { label: "到期时间", priority: 3 },
        cell: (c) => {
          const v = c.getValue<string | null>();
          return v ? <span className="mono small">{formatDateTime(v)}</span> : <span className="muted">不过期</span>;
        },
      },
      {
        accessorKey: "created_at",
        header: "创建时间",
        meta: { label: "创建时间", priority: 2 },
        cell: (c) => <span className="mono small muted">{formatDateTime(c.getValue<string>())}</span>,
      },
    ],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [selected],
  );

  return (
    <>
      <PageHead title="兑换码" desc="批量生成与核销管理" />

      <Card title="批量生成兑换码">
        <div className="form-grid form-grid--4">
          <Field label="面额" req hint={errors.amount} hintWarn={!!errors.amount}>
            <Input
              inputMode="decimal"
              aria-label="面额"
              placeholder="1.00"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
          </Field>
          <Field label="数量" req hint={errors.count} hintWarn={!!errors.count}>
            <Input
              type="number"
              min={1}
              max={MAX_COUNT}
              aria-label="数量"
              placeholder={`1–${MAX_COUNT}`}
              value={count}
              onChange={(e) => setCount(e.target.value)}
            />
          </Field>
          <Field label="有效期至" req hint={errors.expires} hintWarn={!!errors.expires}>
            <Input
              type="date"
              min={todayStr}
              aria-label="有效期至"
              value={expires}
              onChange={(e) => setExpires(e.target.value)}
            />
          </Field>
          <Field label="批次名" req hint={errors.batch} hintWarn={!!errors.batch}>
            <Input
              aria-label="批次名"
              placeholder="例如：双十一活动"
              value={batch}
              onChange={(e) => setBatch(e.target.value)}
            />
          </Field>
        </div>
        {/* 合计是派生数据，不是说明文案：它防的是「面额 × 数量」量级误输
            （1000 个 × 100 元 = 十万元）。与按钮同一行，不占额外高度。 */}
        <div className="form-foot">
          <span className="form-foot__sum">{totalText}</span>
          <button type="button" className="btn btn-primary" disabled={gen.isPending} onClick={submitGenerate}>
            {gen.isPending ? "生成中…" : "生成兑换码"}
          </button>
        </div>
      </Card>

      {result ? (
        <Card
          title={`批次「${result.batch}」已生成 ${result.count} 个 · 每个 ${moneyOf(result.amount)}`}
          actions={
            <span className="ph-acts">
              <button
                type="button"
                className="btn btn-sm"
                disabled={resultQuery.isPending || resultCodes.length === 0}
                onClick={() => void copyResult()}
              >
                复制全部
              </button>
              <button type="button" className="btn btn-sm" onClick={() => void exportResult()}>
                导出 CSV
              </button>
            </span>
          }
        >
          {resultQuery.isPending ? (
            <EmptyState variant="loading" title="正在加载生成结果…" />
          ) : resultQuery.isError ? (
            <EmptyState
              variant="error"
              title="生成结果加载失败"
              desc={resultQuery.error instanceof ApiError ? resultQuery.error.message : "请稍后重试"}
            />
          ) : resultCodes.length ? (
            <div className="code-block code-list">{resultCodes.join("\n")}</div>
          ) : (
            <EmptyState title="暂无生成结果" desc="兑换码已生成，但列表尚未返回数据" />
          )}
        </Card>
      ) : null}

      <div className="blk-title">
        <span>兑换码</span>
        <span className="muted small">
          共 <b className="mono">{total}</b> 条{selected.size > 0 ? `，已选 ${selected.size} 条` : ""}
        </span>
        <span className="right ph-acts">
          <button type="button" className="btn btn-sm" disabled={!rows.length} onClick={toggleAllPage}>
            {allPageSelected ? "取消全选" : "全选本页"}
          </button>
          <button
            type="button"
            className="btn btn-sm btn-danger"
            disabled={selected.size === 0 || del.isPending}
            onClick={() => void voidSelected()}
          >
            {del.isPending ? "作废中…" : `作废${selected.size > 0 ? `（${selected.size}）` : ""}`}
          </button>
        </span>
      </div>
      <DataTable
        columns={columns}
        data={rows}
        loading={list.isPending}
        error={list.isError ? <ErrorState error={list.error} onRetry={() => void list.refetch()} /> : undefined}
        total={total}
        page={page}
        pageSize={DEFAULT_PAGE_SIZE}
        onPageChange={(next) => {
          setPage(next);
          setSelected(new Set());
        }}
        empty={
          <EmptyState
            variant={list.isPending ? "loading" : "empty"}
            title={list.isPending ? "加载中…" : "当前条件下暂无兑换码"}
          />
        }
      />
    </>
  );
}

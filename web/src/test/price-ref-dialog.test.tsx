import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { PriceRefDialog } from "@/pages/admin/model-service/PriceRefDialog";
import type { UpstreamPriceItem, UpstreamSearchMeta, UpstreamSearchResult } from "@/types";

/**
 * 上游参考价面板。
 *
 * 这里守的是四件容易"看着对、其实错"的事：
 *  1. 打开就用初始词查（不是等防抖）——否则每次都要愣 300ms；
 *  2. 手动输入按 300ms 防抖、连续输入只发最后一次；
 *  3. 只有 Token 计费才给「填入」，且 0 价 / 停售行不给（0 微在本地是"显式免费"）；
 *  4. 空结果分三态说清楚（没收录 / 收录但没报价 / 关键词无效）。
 */

const mockQuery = vi.fn();

vi.mock("@/lib/api", () => ({
  useUpstreamPriceQuery: (p: { keyword: string; vendor?: string }, enabled: boolean) =>
    mockQuery(p, enabled),
}));

const ITEM: UpstreamPriceItem = {
  id: "openai/gpt-4o",
  name: "gpt-4o",
  provider_id: "openai",
  provider_name: "OpenAI",
  modality: "chat",
  deprecated: false,
  provider_count: 3,
  input_usd: 2.5,
  output_usd: 10,
  cache_usd: null,
  input_micro: 18_000_000,
  output_micro: 72_000_000,
  cache_micro: null,
};

const META: UpstreamSearchMeta = {
  source: "models.dev",
  fetched_at: "2026-11-26T02:00:00Z",
  stale: false,
  currency: "CNY",
  fx_rate: 7.2,
  matched_keyword: "gpt-4o",
  total: 1,
  with_price: 1,
};

function result(items: UpstreamPriceItem[], meta: Partial<UpstreamSearchMeta> = {}): UpstreamSearchResult {
  return { items, meta: { ...META, ...meta } };
}

type QueryLike = {
  data?: UpstreamSearchResult;
  isPending: boolean;
  isError: boolean;
  error: unknown;
  refetch: () => void;
};

function stubQuery(q: Partial<QueryLike>): void {
  mockQuery.mockReturnValue({ isPending: false, isError: false, error: null, refetch: vi.fn(), ...q });
}

function renderDialog(props: Partial<React.ComponentProps<typeof PriceRefDialog>> = {}) {
  return render(
    <PriceRefDialog
      open
      onClose={() => {}}
      initialKeyword="gpt-4o"
      vendor="openai"
      fillable
      onFill={() => {}}
      {...props}
    />,
  );
}

afterEach(() => {
  mockQuery.mockReset();
  vi.useRealTimers();
});

describe("PriceRefDialog", () => {
  it("打开即用初始词查询，并把词写进搜索框", () => {
    stubQuery({ data: result([ITEM]) });
    renderDialog();

    expect(screen.getByLabelText("搜索模型")).toHaveValue("gpt-4o");
    // 首次查询不能等防抖：调用时关键字已经是初始词
    expect(mockQuery).toHaveBeenCalledWith({ keyword: "gpt-4o", vendor: "openai" }, true);
    expect(screen.getByText("gpt-4o", { selector: ".mono" })).toBeInTheDocument();
    // 双口径：折算价 + 上游原价
    expect(screen.getByText("¥18")).toBeInTheDocument();
    // 美元原价按 4 位小数格式化，但 formatNum 会去掉末尾的 0
    expect(screen.getByText(/^\$2\.5/)).toBeInTheDocument();
  });

  it("手动输入 300ms 后才查询，连续输入只发最后一次", () => {
    vi.useFakeTimers();
    stubQuery({ data: result([ITEM]) });
    renderDialog();

    const input = screen.getByLabelText("搜索模型");
    fireEvent.change(input, { target: { value: "cl" } });
    fireEvent.change(input, { target: { value: "claude" } });

    act(() => vi.advanceTimersByTime(299));
    expect(mockQuery).toHaveBeenLastCalledWith({ keyword: "gpt-4o", vendor: "openai" }, true);

    act(() => vi.advanceTimersByTime(1));
    expect(mockQuery).toHaveBeenLastCalledWith({ keyword: "claude", vendor: "openai" }, true);
  });

  it("回车立即以当前输入查询（不等防抖）", () => {
    vi.useFakeTimers();
    stubQuery({ data: result([ITEM]) });
    renderDialog();

    const input = screen.getByLabelText("搜索模型");
    fireEvent.change(input, { target: { value: "claude" } });
    fireEvent.keyDown(input, { key: "Enter" });

    expect(mockQuery).toHaveBeenLastCalledWith({ keyword: "claude", vendor: "openai" }, true);
  });

  it("Token 计费：点填入把折算后的微回传，且只对有报价的档给按钮", () => {
    const onFill = vi.fn();
    stubQuery({ data: result([ITEM]) });
    renderDialog({ onFill });

    // 缓存价上游没报 → 该档没有填入按钮；输入/输出各一个
    const buttons = screen.getAllByRole("button", { name: "填入" });
    expect(buttons).toHaveLength(2);

    fireEvent.click(buttons[0]);
    expect(onFill).toHaveBeenCalledWith("input", 18_000_000, ITEM);
  });

  it("非 Token 计费：不渲染填入，并说明仅供比价", () => {
    stubQuery({ data: result([ITEM]) });
    renderDialog({ fillable: false });

    expect(screen.queryByRole("button", { name: "填入" })).toBeNull();
    expect(screen.getByText(/参考价仅用于比价/)).toBeInTheDocument();
  });

  it("停售行不给填入", () => {
    stubQuery({ data: result([{ ...ITEM, deprecated: true }]) });
    renderDialog();
    expect(screen.getByText("已停售")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "填入" })).toBeNull();
  });

  it("0 价不给填入（本地 0 微 = 显式免费，误填会把付费模型变免费）", () => {
    stubQuery({
      data: result([{ ...ITEM, input_usd: 0, input_micro: 0, output_usd: null, output_micro: null }]),
    });
    renderDialog();
    expect(screen.queryByRole("button", { name: "填入" })).toBeNull();
  });

  it("数据过期时给出显式标记", () => {
    stubQuery({ data: result([ITEM], { stale: true }) });
    renderDialog();
    expect(screen.getByText("数据已过期")).toBeInTheDocument();
  });

  it("空结果分三态：没收录 / 收录但没报价 / 关键词太短", () => {
    stubQuery({ data: result([], { total: 0, with_price: 0 }) });
    const { unmount } = renderDialog();
    expect(screen.getByText("没有匹配的参考价")).toBeInTheDocument();
    unmount();
    mockQuery.mockReset();

    stubQuery({ data: result([], { total: 3, with_price: 0 }) });
    const second = renderDialog();
    expect(screen.getByText(/找到 3 个同名模型，但都没有公开报价/)).toBeInTheDocument();
    second.unmount();
    mockQuery.mockReset();

    stubQuery({ isPending: false });
    renderDialog({ initialKeyword: "g" });
    expect(screen.getByText("至少输入 2 个字符")).toBeInTheDocument();
  });

  it("失败时就地说明，且指出不影响模型配置", () => {
    stubQuery({ isError: true, error: new Error("boom") });
    renderDialog();
    expect(screen.getByText("参考价暂时拉不到")).toBeInTheDocument();
    expect(screen.getByText(/不影响模型本身的配置与计费/)).toBeInTheDocument();
  });

  it("关键词回退后说明实际按什么匹配", () => {
    stubQuery({ data: result([ITEM], { matched_keyword: "gpt-4o" }) });
    renderDialog({ initialKeyword: "gpt-4o-2024-08-06" });
    expect(screen.getByText("已按 gpt-4o 匹配")).toBeInTheDocument();
  });
});

import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * 提问卡的三条硬约束：
 *  1. 只有 pending 可交互。终态卡片必须**没有任何**可点按钮 ——
 *     一个点得动却报 410 的卡片比没有卡片更糟。
 *  2. 提交的内容就是运营选的那个 value（多选给全集），不能自己翻译或去重成别的。
 *  3. 跳过走同一个端点，带 skipped:true 且不带选中值（服务端会拒绝"跳过 + 有选项"）。
 */
const h = vi.hoisted(() => ({
  mutate: vi.fn(),
}));

vi.mock("@/lib/api/agent", () => ({
  useAnswerAgentAskMutation: () => ({ mutate: h.mutate, isPending: false, isError: false }),
}));

const { AgentAskCard } = await import("@/components/admin/agent/AgentAskCard");

type AskSeg = Parameters<typeof AgentAskCard>[0]["ask"];

function askOf(over: Partial<AskSeg> = {}): AskSeg {
  return {
    kind: "ask",
    askId: 12,
    question: "这个模型按次还是按规格计费？",
    why: "决定 quota_type 与 spec_prices",
    options: [
      { value: "per_call", label: "按次计费", detail: "每次 0.02" },
      { value: "per_spec", label: "按规格", detail: "按张/按秒" },
    ],
    multi: false,
    allowFree: true,
    status: "pending",
    ...over,
  };
}

function renderCard(ask: AskSeg) {
  const onSettled = vi.fn();
  render(<AgentAskCard ask={ask} sessionId={1} onSettled={onSettled} />);
  return onSettled;
}

beforeEach(() => {
  h.mutate.mockReset();
  // 默认"立刻成功"：组件在 onSuccess 里把终态回传给父级。
  h.mutate.mockImplementation((_args: unknown, opts?: { onSuccess?: () => void }) => {
    opts?.onSuccess?.();
  });
});

afterEach(() => {
  vi.clearAllTimers();
});

describe("AgentAskCard", () => {
  it("pending：画出问题、原因与全部选项", () => {
    renderCard(askOf());
    expect(screen.getByText("这个模型按次还是按规格计费？")).toBeTruthy();
    expect(screen.getByText(/决定 quota_type/)).toBeTruthy();
    expect(screen.getByText("按次计费")).toBeTruthy();
    expect(screen.getByText("按规格")).toBeTruthy();
  });

  it("单选：选中后提交，回传该选项 value 与 answered 终态", () => {
    const onSettled = renderCard(askOf());
    fireEvent.click(screen.getByText("按规格"));
    fireEvent.click(screen.getByTestId("agent-ask-submit"));

    expect(h.mutate).toHaveBeenCalledTimes(1);
    const [args] = h.mutate.mock.calls[0];
    expect(args).toMatchObject({ askId: 12, selected: ["per_spec"], skipped: false });
    expect(onSettled).toHaveBeenCalledWith("answered", "按规格");
  });

  it("单选：再点一次取消选择（选错了不必刷新页面）", () => {
    renderCard(askOf());
    fireEvent.click(screen.getByText("按次计费"));
    fireEvent.click(screen.getByText("按次计费"));
    fireEvent.click(screen.getByTestId("agent-ask-submit"));
    // 没选中也没文字 → 按钮应当一直是 disabled，mutate 一次都不该发生。
    expect(h.mutate).not.toHaveBeenCalled();
  });

  it("多选：回传全部选中值", () => {
    const onSettled = renderCard(askOf({ multi: true }));
    fireEvent.click(screen.getByText("按次计费"));
    fireEvent.click(screen.getByText("按规格"));
    fireEvent.click(screen.getByTestId("agent-ask-submit"));
    const [args] = h.mutate.mock.calls[0];
    expect(args.selected).toEqual(["per_call", "per_spec"]);
    expect(onSettled).toHaveBeenCalledWith("answered", "按次计费；按规格");
  });

  it("只写文字也能提交（选项覆盖不到时的出口）", () => {
    const onSettled = renderCard(askOf());
    fireEvent.change(screen.getByPlaceholderText(/补充说明/), { target: { value: "按 0.05/次" } });
    fireEvent.click(screen.getByTestId("agent-ask-submit"));
    const [args] = h.mutate.mock.calls[0];
    expect(args).toMatchObject({ selected: [], freeText: "按 0.05/次" });
    expect(onSettled).toHaveBeenCalledWith("answered", "按 0.05/次");
  });

  it("跳过：带 skipped:true 且不带选中值", () => {
    const onSettled = renderCard(askOf());
    fireEvent.click(screen.getByText("按规格"));
    fireEvent.click(screen.getByTestId("agent-ask-skip"));
    const [args] = h.mutate.mock.calls[0];
    expect(args).toMatchObject({ skipped: true, selected: [], freeText: "" });
    expect(onSettled).toHaveBeenCalledWith("skipped", "已跳过");
  });

  it("终态：没有任何可点按钮，只留结果文案", () => {
    renderCard(askOf({ status: "answered", answer: "按规格" }));
    expect(screen.queryByTestId("agent-ask-submit")).toBeNull();
    expect(screen.queryByTestId("agent-ask-skip")).toBeNull();
    expect(screen.getByText(/已回答：按规格/)).toBeTruthy();
  });

  it("超时/过期：保留问题原文，方便事后追「为什么当时没定」", () => {
    renderCard(askOf({ status: "timeout" }));
    expect(screen.getByText("这个模型按次还是按规格计费？")).toBeTruthy();
    expect(screen.getByText("已超时（按默认继续）")).toBeTruthy();
    expect(screen.queryByTestId("agent-ask-submit")).toBeNull();
  });
});

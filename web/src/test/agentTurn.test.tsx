import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AgentTurn } from "@/components/admin/agent/AgentTurn";
import type { Turn } from "@/components/admin/agent/turns";

/**
 * 失败轮的两条硬约束：
 *  1. 过程与错误必须同时留在界面上（错误是段，不是"清屏后弹一句"）。
 *  2. 可重试错误给「重试」，配置类错误给「去设置」—— 预算超限点重试永远失败。
 */
function turnOf(over: Partial<Turn> = {}): Turn {
  return {
    key: "live",
    userText: "读一下这份文档",
    segments: [],
    answer: "",
    status: "failed",
    meta: {},
    live: true,
    ...over,
  };
}

const noProposals = new Map();
const noop = () => {};

describe("AgentTurn 错误块", () => {
  it("可重试错误：显示原因与「重试」，点击用原话重发", () => {
    const onRetry = vi.fn();
    render(
      <AgentTurn
        turn={turnOf({
          segments: [
            { kind: "reason", text: "先读文档" },
            { kind: "error", code: "upstream_error", message: "上游连接中断", retryable: true },
          ],
        })}
        index={0}
        proposals={noProposals}
        sessionId={1}
        onAnswerAsk={noop}
        running={false}
        onRetry={onRetry}
      />,
    );
    expect(screen.getByTestId("agent-run-error")).toHaveTextContent("上游连接中断");
    // 思考段仍在（失败不清过程）。
    expect(screen.getByTestId("agent-thinking-0")).toHaveTextContent("思考过程");
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(onRetry).toHaveBeenCalledWith("读一下这份文档");
    expect(screen.queryByRole("button", { name: "去设置" })).toBeNull();
  });

  it("预算类错误：给「去设置」而不是「重试」", () => {
    const onOpenSettings = vi.fn();
    render(
      <AgentTurn
        turn={turnOf({
          segments: [
            { kind: "error", code: "agent_budget_exceeded", message: "今日额度预算已用尽", retryable: false },
          ],
        })}
        index={0}
        proposals={noProposals}
        sessionId={1}
        onAnswerAsk={noop}
        running={false}
        onOpenSettings={onOpenSettings}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "去设置" }));
    expect(onOpenSettings).toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "重试" })).toBeNull();
  });

  // 这两个码的错误文案本身就是「请去运行参数里调大 / 换窗口更大的模型」。
  // 文案指了路、界面不给入口，用户只能自己翻菜单 —— 所以逐码钉住。
  it.each(["agent_budget_too_small", "context_overflow"])(
    "%s：文案指向运行参数，必须给「去设置」",
    (code) => {
      const onOpenSettings = vi.fn();
      render(
        <AgentTurn
          turn={turnOf({
            segments: [{ kind: "error", code, message: "请在运行参数里调大", retryable: false }],
          })}
          index={0}
          proposals={noProposals}
          sessionId={1}
          onAnswerAsk={noop}
          running={false}
          onOpenSettings={onOpenSettings}
        />,
      );
      fireEvent.click(screen.getByRole("button", { name: "去设置" }));
      expect(onOpenSettings).toHaveBeenCalled();
    },
  );

  it("有 error 段时不再重复渲染「这一轮没有完成」", () => {
    render(
      <AgentTurn
        turn={turnOf({
          segments: [{ kind: "error", code: "upstream_error", message: "上游超时", retryable: true }],
        })}
        index={0}
        proposals={noProposals}
        sessionId={1}
        onAnswerAsk={noop}
        running={false}
      />,
    );
    expect(screen.queryByText("这一轮没有完成")).toBeNull();
  });

  it("用户主动停止（无 error 段）仍给已停止提示", () => {
    render(
      <AgentTurn
        turn={turnOf({ status: "aborted", segments: [{ kind: "text", text: "说到一半" }] })}
        index={0}
        proposals={noProposals}
        sessionId={1}
        onAnswerAsk={noop}
        running={false}
      />,
    );
    expect(screen.getByText("已停止，已生成的内容已保留，可以接着说")).toBeTruthy();
  });
});

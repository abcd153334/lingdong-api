import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { AgentProposal } from "@/lib/api/agent";

/**
 * 提案卡的两条关键行为：
 *  1. 骨架模式（流式事件刚到、明细还没到）**不能**给动作按钮 —— 否则运营会在
 *     还没看到改动明细时就点"确认应用"。
 *  2. 应用成功但文档重算失败时，必须单独提示。用成功色会把"文档站还是旧的"
 *     这件事藏起来。
 */
const h = vi.hoisted(() => ({
  applyMutate: vi.fn(),
}));

vi.mock("@/lib/api/agent", () => ({
  useApplyProposalMutation: () => ({ mutate: h.applyMutate, isPending: false }),
  useRollbackProposalMutation: () => ({ mutate: vi.fn(), isPending: false }),
  useRejectProposalMutation: () => ({ mutate: vi.fn(), isPending: false }),
  useAgentProposalQuery: () => ({ data: undefined, isLoading: false }),
}));

vi.mock("@/components/common/ConfirmDialog", () => ({
  confirmDialog: async () => true,
}));

const { AgentProposalCard } = await import("@/components/admin/agent/AgentProposalCard");

function proposal(over: Partial<AgentProposal> = {}): AgentProposal {
  return {
    id: 7,
    session_id: 1,
    message_id: 2,
    kind: "change",
    status: "draft",
    plan: { summary: "接入中转A", operations: [{ op: "create", entity: "channel" }] },
    plan_hash: "h7",
    diff: { stats: { added: 1, changed: 0, removed: 0 }, items: [] },
    created_at: "2026-10-26T02:00:00Z",
    ...over,
  };
}

function renderCard(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe("AgentProposalCard", () => {
  it("骨架模式不渲染任何动作按钮", () => {
    renderCard(<AgentProposalCard proposal={proposal()} preview />);
    expect(screen.queryByTestId("agent-apply-7")).toBeNull();
    expect(screen.queryByTestId("agent-rollback-7")).toBeNull();
    expect(screen.queryByTestId("agent-reject-7")).toBeNull();
  });

  it("草稿态可以应用，应用成功后提示文档重算失败", async () => {
    h.applyMutate.mockImplementation((_vars, opts: { onSuccess: (r: unknown) => void }) => {
      opts.onSuccess({ proposal_id: 7, ops: 2, added: 1, docs_error: "doc_endpoints 表缺失" });
    });
    renderCard(<AgentProposalCard proposal={proposal()} />);
    fireEvent.click(screen.getByTestId("agent-apply-7"));
    await waitFor(() => {
      expect(screen.getByText(/文档站重算失败/)).toBeTruthy();
    });
    // 成功提示仍然要在：配置确实已经落库了。
    expect(screen.getByText(/已生效，共执行 2 个操作/)).toBeTruthy();
  });

  it("应用返回 warnings 时必须展示（否则运营以为万事俱备）", async () => {
    h.applyMutate.mockImplementation((_vars, opts: { onSuccess: (r: unknown) => void }) => {
      opts.onSuccess({
        proposal_id: 7,
        ops: 2,
        added: 1,
        warnings: ["模型 muse-video 在本渠道上没有可承接的异步端点"],
      });
    });
    renderCard(<AgentProposalCard proposal={proposal()} />);
    fireEvent.click(screen.getByTestId("agent-apply-7"));
    await waitFor(() => {
      expect(screen.getByText(/1 条提醒/)).toBeTruthy();
    });
    expect(screen.getByText(/没有可承接的异步端点/)).toBeTruthy();
    // 成功提示仍在：配置确实已经落库。
    expect(screen.getByText(/已生效，共执行 2 个操作/)).toBeTruthy();
  });

  it("stale 时没有动作按钮，但必须说明原因", () => {
    renderCard(<AgentProposalCard proposal={proposal({ status: "stale" })} />);
    expect(screen.queryByTestId("agent-apply-7")).toBeNull();
    expect(screen.getByTestId("agent-stale-7").textContent).toContain("重新生成");
  });
});

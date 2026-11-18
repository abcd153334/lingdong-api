import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { ChannelKeysSection } from "@/components/admin/ChannelKeysSection";
import { ApiError } from "@/lib/apiClient";
import type { ChannelKeyItem, ChannelKeyOp, ChannelKeysResult } from "@/types";

/**
 * 密钥池（平铺在编辑渠道抽屉里）的行为契约。
 *
 * 这里钉死的是「一次编辑同时改别名与明文」「批量添加只发一次 PATCH」
 * 「最后一把可用密钥不可关/不可删」「409 保留草稿」——五条回退任何一条，
 * 运营的实际路径就会退回「删了重加」。
 */
const h = vi.hoisted(() => ({
  data: null as ChannelKeysResult | null,
  patches: [] as { version: number; ops: ChannelKeyOp[] }[],
  refetch: () => {},
  failWith: null as Error | null,
  nextCount: 0,
  copies: [] as string[],
}));

vi.mock("@/lib/api", () => ({
  useAdminChannelKeysQuery: () => ({
    data: h.data,
    isLoading: false,
    isError: false,
    error: null,
    refetch: h.refetch,
    dataUpdatedAt: Date.now(),
  }),
  usePatchAdminChannelKeysMutation: () => ({
    isPending: false,
    mutate: (
      vars: { id: number; body: { version: number; ops: ChannelKeyOp[] } },
      opts?: { onSuccess?: (r: ChannelKeysResult) => void; onError?: (e: unknown) => void },
    ) => {
      h.patches.push(vars.body);
      if (h.failWith) {
        opts?.onError?.(h.failWith);
        return;
      }
      if (h.data) {
        const next: ChannelKeysResult = { ...h.data, key_count: h.nextCount || h.data.key_count };
        h.data = next;
        opts?.onSuccess?.(next);
      }
    },
  }),
}));

const confirmMock = vi.hoisted(() => vi.fn<() => Promise<boolean>>());
vi.mock("@/components/common/ConfirmDialog", () => ({ confirmDialog: confirmMock }));

const toastMock = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }));
vi.mock("sonner", () => ({ toast: toastMock }));

vi.mock("@/lib/clipboard", () => ({
  copyText: async (text: string) => {
    h.copies.push(text);
    return true;
  },
}));

// 冷却倒计时用全站单例 ticker；单测里没必要跑真实定时器。
vi.mock("@/components/common/Elapsed", () => ({ useSecondTick: () => 0 }));

const row = (over: Partial<ChannelKeyItem>): ChannelKeyItem => ({
  index: 0,
  key_id: "kid-1",
  fingerprint: "sk-aaa…1111",
  alias: "",
  enabled: true,
  decrypt_ok: true,
  cooldown_reason: "",
  cooldown_remaining_sec: 0,
  last_error_status: 0,
  last_error_at: "",
  ...over,
});

const twoRows = (): ChannelKeysResult => ({
  version: 3,
  items: [
    row({}),
    row({
      index: 1,
      key_id: "kid-2",
      fingerprint: "sk-bbb…2222",
      alias: "备用",
      cooldown_reason: "auth",
      cooldown_remaining_sec: 42,
      last_error_status: 401,
    }),
  ],
  key_count: 2,
  broken_count: 0,
  key_enabled_count: 2,
});

const renderSection = (channelStatus = 1) =>
  render(<ChannelKeysSection channelId={7} channelStatus={channelStatus} />);

beforeEach(() => {
  h.patches = [];
  h.failWith = null;
  h.nextCount = 0;
  h.copies = [];
  h.refetch = vi.fn();
  h.data = twoRows();
  confirmMock.mockReset();
  confirmMock.mockResolvedValue(true);
  toastMock.success.mockReset();
  toastMock.error.mockReset();
});

describe("ChannelKeysSection", () => {
  it("平铺渲染：计数来自密钥池接口，坏条目没有编辑入口", () => {
    h.data = {
      ...twoRows(),
      items: [row({}), row({ index: 1, key_id: "", fingerprint: "", decrypt_ok: false })],
      broken_count: 1,
      key_enabled_count: 1,
    };
    renderSection();

    expect(screen.getByText(/已存 2 把 · 可用 1 把/)).toBeTruthy();
    expect(screen.getByText(/1 把无法解密/)).toBeTruthy();
    expect(screen.getByText("无法解密")).toBeTruthy();
    // 坏条目算不出 keyID，改不了也开不了，只能删。
    expect(screen.getAllByRole("button", { name: "编辑密钥" })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "删除密钥" })).toHaveLength(2);
  });

  it("批量粘贴只发一次 PATCH，成功后清空输入", async () => {
    h.nextCount = 5;
    renderSection();

    fireEvent.click(screen.getByRole("button", { name: "添加密钥" }));
    fireEvent.change(screen.getByLabelText("新增密钥"), { target: { value: "sk-1\nsk-2\nsk-3" } });
    fireEvent.click(screen.getByRole("button", { name: /^添加$/ }));

    await waitFor(() => expect(h.patches).toHaveLength(1));
    expect(h.patches[0].version).toBe(3);
    expect(h.patches[0].ops).toEqual([
      { op: "add", key: "sk-1" },
      { op: "add", key: "sk-2" },
      { op: "add", key: "sk-3" },
    ]);
    expect(toastMock.success).toHaveBeenCalledWith("已新增 3 把");
    expect((screen.getByLabelText("新增密钥") as HTMLTextAreaElement).value).toBe("");
  });

  it("粘贴脱敏指纹：就地报错且不发请求", async () => {
    renderSection();

    fireEvent.click(screen.getByRole("button", { name: "添加密钥" }));
    fireEvent.change(screen.getByLabelText("新增密钥"), { target: { value: "sk-ok\nsk-abc…wxyz" } });
    fireEvent.click(screen.getByRole("button", { name: /^添加$/ }));

    expect(h.patches).toHaveLength(0);
    expect(await screen.findByText(/第 2 行：这是脱敏后的指纹/)).toBeTruthy();
  });

  it("池内已有 98 把时再粘 3 把：前端拦住，不发请求", async () => {
    h.data = { ...twoRows(), key_count: 98 };
    renderSection();

    fireEvent.click(screen.getByRole("button", { name: "添加密钥" }));
    fireEvent.change(screen.getByLabelText("新增密钥"), { target: { value: "a\nb\nc" } });
    fireEvent.click(screen.getByRole("button", { name: /^添加$/ }));

    expect(h.patches).toHaveLength(0);
    expect(await screen.findByText(/最多 100 把，当前 98 把/)).toBeTruthy();
  });

  it("只改别名：发一条 meta", async () => {
    renderSection();

    fireEvent.click(screen.getAllByRole("button", { name: "编辑密钥" })[0]);
    fireEvent.change(screen.getByLabelText("别名"), { target: { value: "主账号" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => expect(h.patches).toHaveLength(1));
    expect(h.patches[0].ops).toEqual([{ op: "meta", key_id: "kid-1", alias: "主账号" }]);
    expect(confirmMock).not.toHaveBeenCalled();
  });

  it("只改密钥：必须确认；取消不发请求", async () => {
    confirmMock.mockResolvedValue(false);
    renderSection();

    fireEvent.click(screen.getAllByRole("button", { name: "编辑密钥" })[0]);
    fireEvent.change(screen.getByLabelText("新密钥"), { target: { value: "sk-new" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => expect(confirmMock).toHaveBeenCalled());
    expect(h.patches).toHaveLength(0);

    confirmMock.mockResolvedValue(true);
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(h.patches).toHaveLength(1));
    // 必须按 keyID 定位：按下标在并发换序时会改错行。
    expect(h.patches[0].ops).toEqual([{ op: "replace", key_id: "kid-1", key: "sk-new" }]);
  });

  it("同时改别名与密钥：一条 replace 带 alias（不拆两次请求）", async () => {
    renderSection();

    fireEvent.click(screen.getAllByRole("button", { name: "编辑密钥" })[0]);
    fireEvent.change(screen.getByLabelText("别名"), { target: { value: "新名字" } });
    fireEvent.change(screen.getByLabelText("新密钥"), { target: { value: "sk-new" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => expect(h.patches).toHaveLength(1));
    expect(h.patches[0].ops).toEqual([
      { op: "replace", key_id: "kid-1", key: "sk-new", alias: "新名字" },
    ]);
  });

  it("都没改就保存：不发请求，直接收起编辑带", async () => {
    renderSection();

    fireEvent.click(screen.getAllByRole("button", { name: "编辑密钥" })[0]);
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => expect(screen.queryByLabelText("别名")).toBeNull());
    expect(h.patches).toHaveLength(0);
  });

  it("409 时保留草稿并提示刷新", async () => {
    h.failWith = new ApiError(409, "冲突", "channel_conflict");
    renderSection();

    fireEvent.click(screen.getAllByRole("button", { name: "编辑密钥" })[0]);
    fireEvent.change(screen.getByLabelText("别名"), { target: { value: "保留我" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => expect(h.refetch).toHaveBeenCalled());
    expect((screen.getByLabelText("别名") as HTMLInputElement).value).toBe("保留我");
  });

  it("最后一把可用密钥：开关与删除都点不动并给出原因", () => {
    h.data = { ...twoRows(), key_enabled_count: 1 };
    renderSection(1);

    const sw = screen.getByLabelText(/启用密钥 sk-aaa…1111/) as HTMLInputElement;
    expect(sw.disabled).toBe(true);
    const del = screen.getAllByRole("button", { name: "删除密钥" })[0];
    expect((del as HTMLButtonElement).disabled).toBe(true);
    expect(del.getAttribute("title")).toMatch(/最后一把可用密钥/);
  });

  it("渠道停用时不再拦：这是停用渠道清空密钥池的唯一路径", () => {
    h.data = { ...twoRows(), key_enabled_count: 1 };
    renderSection(2);

    const del = screen.getAllByRole("button", { name: "删除密钥" })[0];
    expect((del as HTMLButtonElement).disabled).toBe(false);
  });

  it("冷却行显示原因，重置冷却只对它出现", async () => {
    renderSection();

    // 秒数会随本地 ticker 递减，只断言原因与"约 Ns"的形状。
    expect(screen.getByText(/鉴权失败 · 约 \d+s/)).toBeTruthy();
    const resets = screen.getAllByRole("button", { name: "重置冷却" });
    expect(resets).toHaveLength(1);

    fireEvent.click(resets[0]);
    await waitFor(() => expect(h.patches).toHaveLength(1));
    expect(h.patches[0].ops).toEqual([{ op: "reset_cooldown", key_id: "kid-2" }]);
  });

  it("点指纹复制的是 keyID（调用记录按它搜）", async () => {
    renderSection();

    fireEvent.click(screen.getByRole("button", { name: "sk-aaa…1111" }));
    await waitFor(() => expect(h.copies).toEqual(["kid-1"]));
    expect(toastMock.success).toHaveBeenCalledWith("已复制密钥ID");
  });

  it("整列损坏：只留整池重建，添加被禁用", () => {
    h.data = { version: 1, items: [], key_count: 0, broken_count: 1, key_enabled_count: 0 };
    renderSection();

    const box = screen.getByText("密钥列已损坏").closest(".empty") as HTMLElement;
    expect(box).toBeTruthy();
    expect((screen.getByRole("button", { name: "添加密钥" }) as HTMLButtonElement).disabled).toBe(true);
    // 重建入口必须在：这是坏列唯一的修复路径。
    expect(screen.getByRole("button", { name: "重建密钥池…" })).toBeTruthy();
  });

  it("重建密钥池：确认后发一条独占的 replace_all", async () => {
    renderSection();

    fireEvent.click(screen.getByRole("button", { name: "重建密钥池…" }));
    fireEvent.change(screen.getByLabelText("重建密钥池"), { target: { value: "sk-x\nsk-y" } });
    fireEvent.click(screen.getByRole("button", { name: "确认重建" }));

    await waitFor(() => expect(h.patches).toHaveLength(1));
    expect(h.patches[0].ops).toEqual([{ op: "replace_all", keys: ["sk-x", "sk-y"] }]);
    expect(toastMock.success).toHaveBeenCalledWith("密钥池已重建");
  });
});

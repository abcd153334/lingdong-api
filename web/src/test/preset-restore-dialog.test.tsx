import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { PresetRestoreDialog } from "@/pages/admin/model-service/PresetRestoreDialog";
import type { PresetRestoreResult } from "@/types";

/**
 * 「恢复出厂」弹窗。
 *
 * 守四件事（写错任何一条，运营的配置都会在无声中消失）：
 *  1. 打开只做 dry_run 预览，**绝不**在打开时就写；
 *  2. 没有预览计数之前「确认恢复」不可点（盲点比多一步更危险）；
 *  3. 默认 `keep_enabled=true`（保留当前启停），勾选后才 false；
 *  4. warnings / skipped 必须渲染出来（"少恢复了 3 项"不能只留在日志里）。
 */

type MutationLike = {
  mutate: ReturnType<typeof vi.fn>;
  data?: PresetRestoreResult;
  isPending: boolean;
  isError: boolean;
  error: unknown;
};

let preview: MutationLike;
let apply: MutationLike;
let order = 0;

vi.mock("@/lib/api", () => ({
  // 组件按固定顺序调用两次（先 preview 后 apply），按调用序配对即可。
  useRestorePresetsMutation: () => {
    order = (order + 1) % 2;
    return order === 1 ? preview : apply;
  },
}));

const RESULT = (over: Partial<PresetRestoreResult> = {}): PresetRestoreResult => ({
  dry_run: true,
  groups: [
    {
      group: "builtin.duomiapi",
      label: "多米API",
      channels: { reused: 1, created: 0, skipped: 0 },
      models: { reused: 15, created: 0, skipped: 0 },
      bindings: { reused: 15, created: 0, skipped: 0 },
    },
  ],
  ...over,
});

function renderDialog() {
  return render(<PresetRestoreDialog open onClose={vi.fn()} />);
}

beforeEach(() => {
  order = 0;
  preview = { mutate: vi.fn(), isPending: true, isError: false, error: null };
  apply = { mutate: vi.fn(), isPending: false, isError: false, error: null };
});

describe("恢复出厂弹窗：预览", () => {
  it("打开即 dry_run 预览，且不带 confirm（不写入）", () => {
    renderDialog();
    expect(preview.mutate).toHaveBeenCalledTimes(1);
    const body = preview.mutate.mock.calls[0][0] as Record<string, unknown>;
    expect(body.dry_run).toBe(true);
    expect(body.keep_enabled).toBe(true);
    expect(body.confirm).toBeUndefined();
    expect(apply.mutate).not.toHaveBeenCalled();
  });

  it("没有计数时「确认恢复」禁用", () => {
    renderDialog();
    const btn = screen.getByRole("button", { name: "确认恢复" }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it("预览失败时给重试入口，且仍不写入", () => {
    preview.isPending = false;
    preview.isError = true;
    preview.error = null;
    renderDialog();
    expect(screen.getByText("预览失败")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
    expect(apply.mutate).not.toHaveBeenCalled();
  });

  it("预览结果按组渲染计数与 warnings，并标出「尚未写入」", () => {
    preview.data = RESULT({
      groups: [
        {
          group: "builtin.metaso",
          label: "Metaso 标准通道",
          channels: { reused: 1, created: 0, skipped: 0 },
          models: { reused: 1, created: 0, skipped: 0 },
          bindings: { reused: 0, created: 1, skipped: 0 },
          warnings: ["渠道 #7 恢复失败：名称已存在"],
        },
      ],
    });
    preview.isPending = false;
    const { container } = renderDialog();

    expect(screen.getByText("Metaso 标准通道")).toBeTruthy();
    expect(container.querySelector(".prs-warn")?.textContent).toContain("名称已存在");
    expect(container.querySelector(".prs-sum")?.textContent).toContain("尚未写入");
  });

  it("什么都不会变时（全 0）也不给确认", () => {
    preview.data = RESULT({
      groups: [
        {
          group: "builtin.duomiapi",
          label: "多米API",
          channels: { reused: 0, created: 0, skipped: 0 },
          models: { reused: 0, created: 0, skipped: 0 },
          bindings: { reused: 0, created: 0, skipped: 0 },
        },
      ],
    });
    preview.isPending = false;
    renderDialog();
    const btn = screen.getByRole("button", { name: "确认恢复" }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });
});

describe("恢复出厂弹窗：确认", () => {
  it("确认提交 dry_run=false + confirm=RESTORE，默认保留启停", () => {
    preview.data = RESULT();
    preview.isPending = false;
    renderDialog();

    const btn = screen.getByRole("button", { name: "确认恢复" }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);

    expect(apply.mutate).toHaveBeenCalledTimes(1);
    const body = apply.mutate.mock.calls[0][0] as Record<string, unknown>;
    expect(body.dry_run).toBe(false);
    expect(body.confirm).toBe("RESTORE");
    expect(body.keep_enabled).toBe(true);
  });

  it("勾选「同时恢复为停用」后 keep_enabled=false", () => {
    preview.data = RESULT();
    preview.isPending = false;
    const { container } = renderDialog();

    const box = container.querySelector('input[type="checkbox"]') as HTMLInputElement;
    expect(box.checked).toBe(false); // 默认不勾
    act(() => {
      fireEvent.click(box);
    });
    fireEvent.click(screen.getByRole("button", { name: "确认恢复" }));

    const body = apply.mutate.mock.calls[0][0] as Record<string, unknown>;
    expect(body.keep_enabled).toBe(false);
  });

  it("真跑返回后显示服务端计数并标「已生效」", () => {
    preview.data = RESULT();
    preview.isPending = false;
    apply.data = RESULT({
      dry_run: false,
      groups: [
        {
          group: "builtin.duomiapi",
          label: "多米API",
          channels: { reused: 1, created: 0, skipped: 0 },
          models: { reused: 15, created: 0, skipped: 0 },
          bindings: { reused: 15, created: 0, skipped: 0 },
        },
      ],
    });
    const { container } = renderDialog();
    expect(container.querySelector(".prs-sum")?.textContent).toContain("已生效");
  });
});

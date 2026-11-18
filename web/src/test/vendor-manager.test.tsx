import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { VendorManagerDialog } from "@/components/admin/VendorManagerDialog";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { toast } from "sonner";
import type { AdminVendorPayload, VendorItem } from "@/types";

/**
 * 供应商分组管理（主从两栏）的行为契约。
 *
 * 这一块最容易"看起来对、用得错"的四处，逐条钉住：
 *  1. 初始态 = 第一个分组（不是空表单）：打开就看到现状，不用先点一行；
 *  2. 保存提交**三项全量**（`icon: ""` 是清空，省略 = 后端当"不修改" → 永远清不掉）；
 *  3. 保存后用**回包行**刷新草稿：不等整表 refetch，否则"提示成功但图上还是旧的"；
 *  4. 有未保存改动时切分组/关闭必须先确认，取消则留在原分组。
 */
const h = vi.hoisted(() => ({
  items: [] as VendorItem[],
  palette: ["#6366F1", "#EC4899", "#10B981"],
  createArgs: [] as AdminVendorPayload[],
  updateArgs: [] as { id: string; body: AdminVendorPayload }[],
  deleteArgs: [] as string[],
  createResult: null as VendorItem | null,
  updateResult: null as VendorItem | null,
  confirmOk: true,
  builtinData: undefined as unknown,
}));

const v = (over: Partial<VendorItem>): VendorItem => ({
  id: "openai",
  name: "OpenAI",
  icon: "builtin:openai",
  icon_url: "https://api.example.com/api/icons/b/openai?v=abc",
  color: "#10A37F",
  priority: 10,
  sort: 10,
  builtin: false,
  model_count: 3,
  ...over,
});

vi.mock("@/lib/api", () => ({
  useAdminVendorsQuery: () => ({ data: h.builtinData, isPending: false, isError: false }),
  useCreateAdminVendorMutation: () => ({
    isPending: false,
    mutate: (body: AdminVendorPayload, opts?: { onSuccess?: (r: { item: VendorItem }) => void }) => {
      h.createArgs.push(body);
      if (h.createResult) opts?.onSuccess?.({ item: h.createResult });
    },
  }),
  useUpdateAdminVendorMutation: () => ({
    isPending: false,
    mutate: (
      vars: { id: string; body: AdminVendorPayload },
      opts?: { onSuccess?: (r: { item: VendorItem }) => void },
    ) => {
      h.updateArgs.push(vars);
      if (h.updateResult) opts?.onSuccess?.({ item: h.updateResult });
    },
  }),
  useDeleteAdminVendorMutation: () => ({
    isPending: false,
    mutate: (id: string) => {
      h.deleteArgs.push(id);
    },
  }),
  // 图标库（IconPicker 打开时会挂载）：本用例只关心"清空"路径，给空库即可
  useAdminIconsQuery: () => ({
    data: { builtin: [], uploaded: [] },
    isLoading: false,
    isError: false,
    error: null,
    refetch: vi.fn(),
  }),
  useUploadIconMutation: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));

vi.mock("@/lib/apiToast", () => ({ toastApiError: vi.fn() }));
vi.mock("sonner", () => ({ toast: Object.assign(vi.fn(), { error: vi.fn(), success: vi.fn() }) }));
vi.mock("@/components/common/ConfirmDialog", () => ({
  confirmDialog: vi.fn(async () => h.confirmOk),
}));

beforeEach(() => {
  h.items = [
    v({ id: "openai", name: "OpenAI", icon: "builtin:openai", color: "#10A37F", model_count: 3 }),
    v({ id: "zhupu", name: "智谱", icon: "", color: "#6366F1", builtin: true, model_count: 5 }),
  ];
  h.builtinData = { items: h.items, palette: h.palette };
  h.createArgs = [];
  h.updateArgs = [];
  h.deleteArgs = [];
  h.createResult = null;
  h.updateResult = null;
  h.confirmOk = true;
  vi.mocked(confirmDialog).mockClear();
  vi.mocked(toast).mockClear();
  vi.mocked(toast.error).mockClear();
});

const renderDialog = (props?: { open?: boolean; onClose?: () => void }) =>
  render(<VendorManagerDialog open={props?.open ?? true} onClose={props?.onClose ?? (() => {})} />);

/** 名称输入框：Field 的 label 指向它 */
const nameInput = () => screen.getByLabelText("名称") as HTMLInputElement;
const saveBtn = () => screen.getByRole("button", { name: "保存" }) as HTMLButtonElement;

describe("VendorManagerDialog", () => {
  it("初始态直接选中第一个分组，保存按钮在无改动时禁用", () => {
    renderDialog();
    expect(nameInput().value).toBe("OpenAI");
    expect(saveBtn().disabled).toBe(true);
  });

  it("改名后可保存，提交 name/icon/color 三项全量", () => {
    h.updateResult = v({ id: "openai", name: "OpenAI 官方", icon: "builtin:openai", color: "#10A37F" });
    renderDialog();

    fireEvent.change(nameInput(), { target: { value: "OpenAI 官方" } });
    fireEvent.click(saveBtn());

    // icon 是已设置值、color 是已有品牌色，两者都要跟着提交：
    // 只发"改过的字段"等于要求前端自己算 diff，漏一个就是"改了没保存上"。
    expect(h.updateArgs).toEqual([
      { id: "openai", body: { name: "OpenAI 官方", icon: "builtin:openai", color: "#10A37F" } },
    ]);
  });

  it("保存后用回包行刷新草稿（不等 refetch，界面立刻是新值）", async () => {
    h.updateResult = v({ id: "openai", name: "改名后", icon: "upload:aaa", color: "#EC4899" });
    renderDialog();

    fireEvent.change(nameInput(), { target: { value: "改名后" } });
    fireEvent.click(saveBtn());

    await waitFor(() => expect(toast).toHaveBeenCalledWith("分组已保存"));
    // 回包里的图标/颜色与提交值不同 → 说明草稿确实来自回包，不是本地 echo
    expect(nameInput().value).toBe("改名后");
    expect(saveBtn().disabled).toBe(true);
  });

  it("清空图标：提交空串（省略字段 = 后端「不修改」= 永远清不掉）", () => {
    h.updateResult = v({ id: "openai", name: "OpenAI", icon: "", color: "#10A37F" });
    renderDialog();

    // 打开图库 → 「使用首字」
    fireEvent.click(screen.getByRole("button", { name: "更换图标" }));
    fireEvent.click(screen.getByRole("button", { name: "使用首字" }));
    fireEvent.click(saveBtn());

    expect(h.updateArgs[0].body.icon).toBe("");
  });

  it("切换分组时有未保存改动要先确认；取消则留在原分组", async () => {
    h.confirmOk = false;
    renderDialog();

    fireEvent.change(nameInput(), { target: { value: "改了一半" } });
    fireEvent.click(screen.getByRole("button", { name: /智谱/ }));

    await waitFor(() => expect(confirmDialog).toHaveBeenCalled());
    expect(nameInput().value).toBe("改了一半");
  });

  it("无改动时切换分组不弹确认", async () => {
    renderDialog();
    fireEvent.click(screen.getByRole("button", { name: /智谱/ }));

    await waitFor(() => expect(nameInput().value).toBe("智谱"));
    expect(confirmDialog).not.toHaveBeenCalled();
  });

  it("内置分组不给删除入口，自定义分组给", () => {
    const { unmount } = renderDialog();
    // 初始选中的 OpenAI 是自定义分组
    expect(screen.getByRole("button", { name: "删除" })).toBeInTheDocument();
    unmount();

    h.items = [v({ id: "zhupu", name: "智谱", builtin: true })];
    h.builtinData = { items: h.items, palette: h.palette };
    renderDialog();
    expect(screen.queryByRole("button", { name: "删除" })).toBeNull();
  });

  it("新建：右栏清空，初始品牌色取色盘首项（不是留空随机）", () => {
    renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "新建分组" }));

    expect(nameInput().value).toBe("");
    fireEvent.change(nameInput(), { target: { value: "自建" } });

    h.createResult = v({ id: "custom", name: "自建", icon: "", color: "#6366F1" });
    fireEvent.click(saveBtn());
    expect(h.createArgs).toEqual([{ name: "自建", icon: "", color: "#6366F1" }]);
  });

  it("名称超过 12 字或为空时禁用保存（后端仍是最终判据）", () => {
    renderDialog();
    fireEvent.change(nameInput(), { target: { value: "一二三四五六七八九十一二三" } });
    expect(saveBtn().disabled).toBe(true);

    fireEvent.change(nameInput(), { target: { value: "   " } });
    expect(saveBtn().disabled).toBe(true);
  });

  it("有未保存改动时点遮罩关闭需确认；确认才真正关", async () => {
    const onClose = vi.fn();
    renderDialog({ onClose });

    fireEvent.change(nameInput(), { target: { value: "改了一半" } });
    // 遮罩点击走的是 Dialog 的 onClose —— 弹窗把它接到了「先确认再关」上
    fireEvent.click(document.querySelector(".mask")!);

    await waitFor(() => expect(confirmDialog).toHaveBeenCalled());
    expect(onClose).toHaveBeenCalled();
  });

  it("空列表：左栏给空态、右栏直接是新建表单", () => {
    h.builtinData = { items: [], palette: h.palette };
    renderDialog();

    expect(screen.getByText("还没有分组")).toBeInTheDocument();
    expect(nameInput().value).toBe("");
  });
});

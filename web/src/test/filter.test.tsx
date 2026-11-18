import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FilterBar } from "@/components/common/FilterBar";
import { SegmentedChips } from "@/components/common/SegmentedChips";

const fields = [
  { id: "q", t: "search" as const, label: "关键词", placeholder: "搜索请求 ID" },
  {
    id: "modality",
    t: "select" as const,
    label: "模态",
    allLabel: "全部模态",
    items: [["chat", "对话"]] as [string, string][],
  },
  { id: "go", t: "btn" as const, label: "查询", variant: "primary" as const },
];

describe("FilterBar", () => {
  it("桌面内联渲染字段与动作按钮", () => {
    const { container } = render(
      <FilterBar fields={fields} value={{}} onChange={() => {}} onReset={() => {}} />,
    );
    expect(container.querySelector(".pbar-fields")).toBeTruthy();
    // 桌面内联字段渲染；Sheet 字段仅在 sheetOpen 时渲染（避免同一控件产生 2 份 DOM，
    // 否则 getByLabel/占位符查询会严格模式双命中 —— 见 S0 的 F10 修复）
    expect(screen.getAllByPlaceholderText("搜索请求 ID")).toHaveLength(1);
    expect(container.querySelector(".pbar-fields input")).toHaveAttribute(
      "placeholder",
      "搜索请求 ID",
    );
    expect(screen.getByRole("button", { name: "查询" })).toHaveClass("btn-primary");
    // 无生效条件时动作区不含「重置」（底部 Sheet 的重置按钮属另一上下文）
    const actsText = Array.from(container.querySelectorAll(".pbar .acts button")).map(
      (b) => b.textContent,
    );
    expect(actsText).toEqual(["查询"]);
    expect(container.querySelector(".pbar-wrap")?.className).not.toContain("has-tokens");
  });

  it("已生效条件渲染为可移除 chip，移除后回传默认值", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    const { container } = render(
      <FilterBar
        fields={fields}
        value={{ q: "req-1", modality: "chat" }}
        onChange={onChange}
        onReset={() => {}}
      />,
    );
    expect(container.querySelector(".pbar-wrap")?.className).toContain("has-tokens");
    expect(screen.getByText("req-1")).toBeInTheDocument();
    const chips = container.querySelectorAll(".ptoken");
    expect(chips).toHaveLength(2);
    await user.click(screen.getByRole("button", { name: "移除筛选 关键词" }));
    expect(onChange).toHaveBeenCalledWith("q", "");
  });

  it("清空全部触发重置；移动端折叠为触发条 + 底部 Sheet", async () => {
    const onReset = vi.fn();
    const user = userEvent.setup();
    const { container } = render(
      <FilterBar fields={fields} value={{ q: "req-1" }} onChange={() => {}} onReset={onReset} />,
    );
    await user.click(screen.getByRole("button", { name: "清空全部" }));
    expect(onReset).toHaveBeenCalled();

    // 移动端入口（CSS 在 ≤767px 才显示）：点击打开筛选 Sheet
    await user.click(container.querySelector(".pbar-trigger") as HTMLElement);
    const sheet = container.querySelector(".dialog.filter-sheet");
    expect(sheet?.className).toContain("open");
    expect(sheet?.querySelectorAll(".pfield")).toHaveLength(2);
    expect(sheet?.querySelector(".pfield-lb")?.textContent).toBe("关键词");
  });

  it("chips 槽位：桌面渲染在筛选条下方，Sheet 内同样可见", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <FilterBar
        fields={fields}
        value={{}}
        onChange={() => {}}
        onReset={() => {}}
        chips={[{ label: "模态", node: <span>chip-node</span> }]}
      />,
    );
    expect(container.querySelector(".pbar-chips")?.textContent).toContain("chip-node");
    await user.click(container.querySelector(".pbar-trigger") as HTMLElement);
    const sheet = container.querySelector(".dialog.filter-sheet") as HTMLElement;
    // 桌面副本与 Sheet 副本并存时，Sheet 内以 label 包裹
    expect(sheet.textContent).toContain("模态");
    expect(sheet.textContent).toContain("chip-node");
  });
});

describe("SegmentedChips", () => {
  const items = [
    { key: "a", label: "A" },
    { key: "b", label: "B" },
  ];

  it("单选：active 跟随 value，点击回传 key", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(<SegmentedChips items={items} value="a" onChange={onChange} />);
    expect(screen.getByRole("button", { name: "A" })).toHaveClass("active");
    expect(screen.getByRole("button", { name: "B" })).not.toHaveClass("active");
    await user.click(screen.getByRole("button", { name: "B" }));
    expect(onChange).toHaveBeenCalledWith("b");
  });

  it("多选：按 values 判定，可同时选中多项", () => {
    render(<SegmentedChips items={items} multiple values={["a", "b"]} onChange={() => {}} />);
    expect(screen.getByRole("button", { name: "A" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "B" })).toHaveAttribute("aria-pressed", "true");
  });

  it("renderItem 覆盖默认文案", () => {
    render(
      <SegmentedChips
        items={items}
        value="a"
        onChange={() => {}}
        renderItem={(it) => <span>#{it.key}</span>}
      />,
    );
    expect(screen.getByRole("button", { name: "#a" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "A" })).toBeNull();
  });
});

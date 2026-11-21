import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { EntitySelect, type EntityOption } from "@/components/common/EntitySelect";

const APPS: EntityOption[] = [
  { value: "1", label: "官网门户", hint: "web-portal" },
  { value: "2", label: "灵动画影", hint: "shadow" },
  { value: "3", label: "开放平台", hint: "openapi" },
];

/**
 * EntitySelect 的两个分支必须互不影响：
 *   - 默认（select-only）：触发器是 button，**不能**多出输入框；
 *   - searchable：触发器是输入框，输入即过滤、↑↓ 移动、Enter 提交、Esc 关闭。
 *
 * 第一条是回归线：模型服务/渠道三处调用点都在用默认形态，
 * 任何"顺手统一"都会改变它们的 DOM 与键盘模型。
 */
describe("EntitySelect", () => {
  it("默认形态：触发器是 button，不渲染输入框", () => {
    const { container } = render(<EntitySelect value="" options={APPS} onChange={() => {}} />);
    const trigger = screen.getByRole("combobox");
    expect(trigger.tagName).toBe("BUTTON");
    expect(container.querySelector("input")).toBeNull();
  });

  it("searchable：聚焦即展开，输入即过滤，Enter 提交", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(
      <EntitySelect
        value=""
        options={APPS}
        onChange={onChange}
        searchable
        allLabel="全部应用"
        ariaLabel="应用"
      />,
    );

    const input = screen.getByRole("combobox");
    expect(input.tagName).toBe("INPUT");

    await user.click(input);
    const list = screen.getByRole("listbox");
    expect(list).toBeTruthy();
    // 「全部」项 + 三个应用
    expect(screen.getAllByRole("option")).toHaveLength(4);

    await user.type(input, "影");
    const opts = screen.getAllByRole("option");
    expect(opts).toHaveLength(1);
    expect(opts[0]).toHaveTextContent("灵动画影");

    // 高亮已在第一项（输入后重置），Enter 直接提交它
    await user.keyboard("{Enter}");
    expect(onChange).toHaveBeenCalledWith("2");
  });

  it("searchable：按 hint（slug）也能搜到", async () => {
    const user = userEvent.setup();
    render(<EntitySelect value="" options={APPS} onChange={() => {}} searchable />);
    const input = screen.getByRole("combobox");
    await user.click(input);
    await user.type(input, "openapi");
    const opts = screen.getAllByRole("option");
    expect(opts).toHaveLength(1);
    expect(opts[0]).toHaveTextContent("开放平台");
  });

  it("searchable：Esc 关闭面板且焦点留在输入框", async () => {
    const user = userEvent.setup();
    render(<EntitySelect value="" options={APPS} onChange={() => {}} searchable />);
    const input = screen.getByRole("combobox");
    await user.click(input);
    expect(screen.getByRole("listbox")).toBeTruthy();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(document.activeElement).toBe(input);
  });

  it("searchable：已选值时收起态显示该值的 label", () => {
    render(<EntitySelect value="2" options={APPS} onChange={() => {}} searchable />);
    expect(screen.getByRole("combobox")).toHaveValue("灵动画影");
  });
});

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Select } from "@/components/common/Select";
import { Tabs } from "@/components/common/Tabs";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { ThemeToggle } from "@/components/chrome/ThemeToggle";
import { applyTheme } from "@/stores/theme";

describe("SwitchRow", () => {
  it("单一 DOM 契约：label.switch > input + i，且点击可切换", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    const { container } = render(
      <SwitchRow label="启用渠道" tip="关闭后不参与路由" checked={false} onChange={onChange} />,
    );
    expect(container.querySelector("label.switch > input[type=checkbox]")).toBeTruthy();
    expect(container.querySelector("label.switch > i")).toBeTruthy();
    expect(screen.getByText("启用渠道")).toBeInTheDocument();
    await user.click(screen.getByRole("checkbox"));
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it("禁用态不可交互", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(<SwitchRow label="自动禁用" checked={false} disabled onChange={onChange} />);
    await user.click(screen.getByRole("checkbox"));
    expect(onChange).not.toHaveBeenCalled();
  });
});

describe("Select", () => {
  it("渲染 allLabel 首项与 items（含 [value,label] 元组）", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <Select
        items={[["chat", "对话"], "video"]}
        allLabel="全部模态"
        value=""
        onChange={onChange}
      />,
    );
    expect(screen.getAllByRole("option")).toHaveLength(3);
    await user.selectOptions(screen.getByRole("combobox"), "chat");
    expect(onChange).toHaveBeenCalled();
  });
});

describe("Tabs", () => {
  it("线性选项卡标记 active 并回传 key", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <Tabs
        items={[
          { key: "basic", label: "基础" },
          { key: "billing", label: "计费" },
        ]}
        value="basic"
        onChange={onChange}
      />,
    );
    expect(screen.getByRole("tab", { name: "基础" })).toHaveClass("active");
    await user.click(screen.getByRole("tab", { name: "计费" }));
    expect(onChange).toHaveBeenCalledWith("billing");
  });

  it("胶囊型使用 pill-tabs 容器", () => {
    const { container } = render(
      <Tabs variant="pill" items={[{ key: "all", label: "全部" }]} value="all" onChange={() => {}} />,
    );
    expect(container.querySelector(".pill-tabs .tab.active")).toBeTruthy();
  });

  it("计数徽标渲染为 aria-hidden 的 .tab-count（不进可访问名）", () => {
    render(
      <Tabs
        items={[
          { key: "builtin", label: "内置", count: 3 },
          { key: "spec", label: "规格", count: 7 },
        ]}
        value="spec"
        onChange={() => {}}
      />,
    );
    // 可访问名严格匹配「规格」仍成立（计数徽标不进可访问名）
    expect(screen.getByRole("tab", { name: "规格" })).toBeTruthy();
    const badges = document.querySelectorAll(".tab-count");
    expect(Array.from(badges).map((b) => b.textContent)).toEqual(["3", "7"]);
    badges.forEach((b) => expect(b).toHaveAttribute("aria-hidden", "true"));
  });

  it("停用项不可点击且不进 tab 序列", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <Tabs
        items={[
          { key: "a", label: "甲" },
          { key: "b", label: "乙", disabled: true },
        ]}
        value="a"
        onChange={onChange}
      />,
    );
    const disabled = screen.getByRole("tab", { name: "乙" });
    expect(disabled).toBeDisabled();
    expect(disabled).toHaveAttribute("tabindex", "-1");
    await user.click(disabled);
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.getByRole("tab", { name: "甲" })).toHaveAttribute("tabindex", "0");
  });

  it("方向键切到下一启用项并跳过停用项、焦点跟随", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const items = [
      { key: "a", label: "甲" },
      { key: "b", label: "乙", disabled: true },
      { key: "c", label: "丙" },
    ];
    const { rerender } = render(<Tabs items={items} value="a" onChange={onChange} />);
    const tab = (name: string) => screen.getByRole("tab", { name });

    tab("甲").focus();
    await user.keyboard("{ArrowRight}");
    expect(onChange).toHaveBeenLastCalledWith("c"); // 跳过停用的「乙」
    expect(document.activeElement).toBe(tab("丙"));

    rerender(<Tabs items={items} value="c" onChange={onChange} />);
    await user.keyboard("{ArrowRight}");
    expect(onChange).toHaveBeenLastCalledWith("a"); // 末项右移回到首项

    rerender(<Tabs items={items} value="a" onChange={onChange} />);
    await user.keyboard("{ArrowLeft}");
    expect(onChange).toHaveBeenLastCalledWith("c"); // 首项左移回到末项

    rerender(<Tabs items={items} value="c" onChange={onChange} />);
    await user.keyboard("{Home}");
    expect(onChange).toHaveBeenLastCalledWith("a");

    rerender(<Tabs items={items} value="a" onChange={onChange} />);
    await user.keyboard("{End}");
    expect(onChange).toHaveBeenLastCalledWith("c");
  });
});

describe("SegmentedChips", () => {
  it("胶囊单选：active 随 value 变化，且容器保证不压缩", () => {
    const { container } = render(
      <SegmentedChips
        items={[
          { key: "all", label: "全部" },
          { key: "billing", label: "按量计费" },
        ]}
        value="billing"
        onChange={() => {}}
      />,
    );
    expect(container.querySelector(".chips")).toBeTruthy();
    expect(screen.getByRole("button", { name: "按量计费" })).toHaveClass("active");
  });
});

describe("ThemeToggle / applyTheme", () => {
  it("切换主题时写入 html[data-theme] 与 localStorage", async () => {
    const user = userEvent.setup();
    applyTheme("light");
    render(<ThemeToggle />);
    await user.click(screen.getByRole("button"));
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(localStorage.getItem("ld-theme")).toBe("dark");
    await user.click(screen.getByRole("button"));
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });
});

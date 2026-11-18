import { useState } from "react";
import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { ModelTagsField, type TagFormRow } from "@/components/admin/ModelTagsField";
import type { TagColorOption } from "@/types";

const PALETTE: TagColorOption[] = [
  { key: "red", label: "红", hex: "#E11D48" },
  { key: "teal", label: "蓝绿", hex: "#0D9488" },
  { key: "pink", label: "粉", hex: "#DB2777" },
];

function Harness({
  initial,
  onChange,
}: {
  initial: TagFormRow[];
  onChange?: (v: TagFormRow[]) => void;
}) {
  const [value, setValue] = useState<TagFormRow[]>(initial);
  return (
    <ModelTagsField
      value={value}
      palette={PALETTE}
      onChange={(next) => {
        setValue(next);
        onChange?.(next);
      }}
    />
  );
}

const addBtn = () => screen.getByRole("button", { name: "添加" });

describe("ModelTagsField", () => {
  it("单个表单添加：名称 + 默认主题，日期留空 = 长期有效，加完表单清空", () => {
    let latest: TagFormRow[] = [];
    render(<Harness initial={[]} onChange={(v) => (latest = v)} />);

    fireEvent.change(screen.getByLabelText("标签名称"), { target: { value: " 限时免费 " } });
    fireEvent.click(addBtn());

    expect(latest).toEqual([{ label: "限时免费", color: "red", untilDate: "" }]);
    expect((screen.getByLabelText("标签名称") as HTMLInputElement).value).toBe("");
  });

  it("到期时间可选：填了就原样带上", () => {
    let latest: TagFormRow[] = [];
    render(<Harness initial={[]} onChange={(v) => (latest = v)} />);

    fireEvent.change(screen.getByLabelText("标签名称"), { target: { value: "夜间折扣" } });
    fireEvent.change(screen.getByLabelText("到期日期（可选）"), { target: { value: "2099-01-31" } });
    fireEvent.click(addBtn());

    expect(latest[0]).toEqual({ label: "夜间折扣", color: "red", untilDate: "2099-01-31" });
  });

  it("主题点选写入 key，自定义色写入大写 hex", () => {
    let latest: TagFormRow[] = [];
    render(<Harness initial={[]} onChange={(v) => (latest = v)} />);

    fireEvent.click(screen.getByRole("button", { name: "蓝绿" }));
    fireEvent.change(screen.getByLabelText("标签名称"), { target: { value: "活动" } });
    fireEvent.click(addBtn());
    expect(latest[0].color).toBe("teal");

    fireEvent.change(screen.getByLabelText("自定义颜色"), { target: { value: "#0ea5e9" } });
    fireEvent.change(screen.getByLabelText("标签名称"), { target: { value: "品牌色" } });
    fireEvent.click(addBtn());
    expect(latest[1].color).toBe("#0EA5E9");
  });

  it("已添加的标签与门户同一枚胶囊（所见即所得），点 ✕ 内联删除", () => {
    let latest: TagFormRow[] = [
      { label: "限时免费", color: "red", untilDate: "" },
      { label: "订阅优先", color: "teal", untilDate: "" },
    ];
    const { container } = render(<Harness initial={latest} onChange={(v) => (latest = v)} />);

    const tags = Array.from(container.querySelectorAll(".model-tag")) as HTMLElement[];
    expect(tags.map((n) => n.textContent)).toEqual(["限时免费", "订阅优先"]);
    expect(tags[0].style.getPropertyValue("--model-tag-fg")).toBe("#E11D48");

    fireEvent.click(screen.getByRole("button", { name: "删除标签 限时免费" }));
    expect(latest.map((t) => t.label)).toEqual(["订阅优先"]);
  });

  it("满 3 个时「添加」禁用", () => {
    render(
      <Harness
        initial={[
          { label: "一", color: "red", untilDate: "" },
          { label: "二", color: "teal", untilDate: "" },
          { label: "三", color: "pink", untilDate: "" },
        ]}
      />,
    );
    fireEvent.change(screen.getByLabelText("标签名称"), { target: { value: "四" } });
    expect(addBtn()).toBeDisabled();
  });

  it("重复文案与已过期日期都不让添加（权威判定仍在后端）", () => {
    const { container } = render(
      <Harness initial={[{ label: "限时免费", color: "red", untilDate: "" }]} />,
    );

    fireEvent.change(screen.getByLabelText("标签名称"), { target: { value: "限时免费" } });
    expect(addBtn()).toBeDisabled();
    expect(container.querySelector(".model-tags-warn")?.textContent).toContain("已存在");

    fireEvent.change(screen.getByLabelText("标签名称"), { target: { value: "旧活动" } });
    fireEvent.change(screen.getByLabelText("到期日期（可选）"), { target: { value: "2020-01-01" } });
    expect(addBtn()).toBeDisabled();
    expect(container.querySelector(".model-tags-warn")?.textContent).toContain("已过");
  });
});

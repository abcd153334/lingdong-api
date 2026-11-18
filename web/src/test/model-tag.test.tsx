import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";

import { ModelTag, ModelTagList } from "@/components/common/ModelTag";

/**
 * 标签渲染契约：
 *  - 颜色只认后端下发的 color_hex（前端没有第二张色表）；
 *  - 顺序 = 数组顺序（后端已排好，组件不再排序）；
 *  - 空标签返回 null（不产出空容器），超出 max 折成 +N。
 */
describe("ModelTag", () => {
  it("用 color_hex 覆盖 --model-tag-fg，文案原样", () => {
    const { container } = render(<ModelTag tag={{ label: "限时免费", color_hex: "#E11D48" }} />);
    const el = container.querySelector(".model-tag") as HTMLElement;
    expect(el.textContent).toBe("限时免费");
    expect(el.style.getPropertyValue("--model-tag-fg")).toBe("#E11D48");
    expect(el.classList.contains("is-expired")).toBe(false);
  });

  it("过期项带 is-expired 与说明", () => {
    const { container } = render(
      <ModelTag tag={{ label: "旧活动", color_hex: "#E11D48", expired: true }} />,
    );
    const el = container.querySelector(".model-tag") as HTMLElement;
    expect(el.classList.contains("is-expired")).toBe(true);
    expect(el.getAttribute("title")).toBe("已过期，不会对外展示");
  });
});

describe("ModelTagList", () => {
  const tags = [
    { label: "限时免费", color_hex: "#E11D48" },
    { label: "夜间折扣", color_hex: "#0EA5E9" },
    { label: "订阅优先", color_hex: "#7C3AED" },
  ];

  it("按数组顺序渲染", () => {
    const { container } = render(<ModelTagList tags={tags} />);
    expect(Array.from(container.querySelectorAll(".model-tag")).map((n) => n.textContent)).toEqual([
      "限时免费",
      "夜间折扣",
      "订阅优先",
    ]);
  });

  it("超过 max 折成 +N，剩余文案进 title", () => {
    const { container } = render(<ModelTagList tags={tags} max={2} />);
    expect(container.querySelectorAll(".model-tag")).toHaveLength(2);
    const more = container.querySelector(".model-tag-list__more") as HTMLElement;
    expect(more.textContent).toBe("+1");
    expect(more.getAttribute("title")).toBe("订阅优先");
  });

  it("空数组与 undefined 都不渲染容器", () => {
    expect(render(<ModelTagList tags={[]} />).container.firstChild).toBeNull();
    expect(render(<ModelTagList />).container.firstChild).toBeNull();
  });
});

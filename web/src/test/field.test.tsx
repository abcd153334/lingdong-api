import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Card } from "@/components/common/Card";
import { Field } from "@/components/common/Field";

describe("Field", () => {
  it("渲染 label 与必填标记，并关联子控件 id", () => {
    const { container } = render(
      <Field label="模型名称" req htmlFor="m-name">
        <input id="m-name" />
      </Field>,
    );
    const item = container.querySelector(".form-item") as HTMLElement;
    expect(item).toBeTruthy();
    expect(item.querySelector("label")).toHaveAttribute("for", "m-name");
    expect(item.querySelector(".req")?.textContent).toBe("*");
    expect(container.querySelector("#m-name")).toBeTruthy();
  });

  it("tip 渲染问号提示，hint 渲染底部说明，span 追加跨列类", () => {
    const { container } = render(
      <Field label="价格" tip="按 1K tokens 计价" hint="单位：元" span>
        <input />
      </Field>,
    );
    const item = container.querySelector(".form-item") as HTMLElement;
    expect(item).toHaveClass("span-2");
    expect(item.querySelector(".tip")).toHaveAttribute("data-tip", "按 1K tokens 计价");
    expect(item.querySelector(".hint")?.textContent).toBe("单位：元");
    expect(item.querySelector(".hint")).not.toHaveClass("warn");
  });

  it("hintWarn 使用警告色类", () => {
    const { container } = render(
      <Field label="余额" hint="余额不足" hintWarn>
        <input />
      </Field>,
    );
    expect(container.querySelector(".hint")).toHaveClass("warn");
  });

  it("labelExtra 渲染在 label 内（内联说明/字数计数）", () => {
    const { container } = render(
      <Field label="邮箱" labelExtra={<span className="hint hint-inline">（可选）</span>}>
        <input />
      </Field>,
    );
    expect(container.querySelector("label .hint.hint-inline")?.textContent).toBe("（可选）");
  });

  it("label 文案可见（getByLabel 单命中，不产生重复可访问名）", () => {
    render(
      <Field label="密钥" htmlFor="k">
        <input id="k" />
      </Field>,
    );
    expect(screen.getByLabelText("密钥")).toBeInTheDocument();
  });

  it("tip 存在时把隐藏描述节点接到子控件（aria-describedby）", () => {
    const { container } = render(
      <Field label="APIv3 密钥" tip="32 位，用于解密回调">
        <input />
      </Field>,
    );
    const input = container.querySelector("input") as HTMLInputElement;
    const descId = input.getAttribute("aria-describedby");
    expect(descId).toBeTruthy();
    // 描述节点必须真的存在且内容就是规则文本（读屏靠它，不靠气泡）
    // 用属性选择器：React useId 产出 `:r0:` 这类 id，CSS.escape 在 jsdom 下不稳
    const desc = container.querySelector(`[id="${descId}"]`);
    expect(desc?.textContent).toBe("32 位，用于解密回调");
    // label 的可访问名只多一个固定的「说明」，不会被整段规则污染
    expect(container.querySelector("label")).toHaveAttribute("for", input.id);
  });

  it("子控件已有 id 时不覆盖，只追加描述关联", () => {
    const { container } = render(
      <Field label="商户号" tip="纯数字" htmlFor="mch">
        <input id="keep-me" />
      </Field>,
    );
    const input = container.querySelector("input") as HTMLInputElement;
    expect(input.id).toBe("keep-me");
    expect(input).toHaveAttribute("aria-describedby");
    // label 仍指向 htmlFor（显式传入优先），由调用方保证与子控件一致
    expect(container.querySelector("label")).toHaveAttribute("for", "mch");
  });

  it("无 tip 时不产生多余的 aria-describedby", () => {
    const { container } = render(
      <Field label="PID">
        <input />
      </Field>,
    );
    expect(container.querySelector("input")).not.toHaveAttribute("aria-describedby");
  });
});

describe("Card", () => {
  it("渲染 h3 标题骨架与右侧说明", () => {
    const { container } = render(
      <Card title="渠道健康" sub="熔断态与失败率">
        内容
      </Card>,
    );
    const card = container.querySelector(".card") as HTMLElement;
    expect(card.querySelector("h3")?.textContent).toBe("渠道健康熔断态与失败率");
    expect(card.querySelector("h3 .sub")?.textContent).toBe("熔断态与失败率");
  });

  it("actions 渲染在标题行右端（卡片不携带外边距，块间距由页面列给出）", () => {
    const { container } = render(
      <Card title="近 7 天" actions={<button type="button">切换</button>}>
        内容
      </Card>,
    );
    const card = container.querySelector(".card") as HTMLElement;
    expect(card.querySelector("h3 .right button")?.textContent).toBe("切换");
  });

  it("无 title 时不渲染 h3（裸卡片）", () => {
    const { container } = render(<Card>内容</Card>);
    expect(container.querySelector("h3")).toBeNull();
  });
});

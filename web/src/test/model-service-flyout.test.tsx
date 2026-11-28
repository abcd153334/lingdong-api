import { createRef } from "react";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { ChannelItem } from "@/types";

import { ChannelFlyout } from "@/pages/admin/model-service/ChannelFlyout";

const ch = (id: number, name: string, patch: Partial<ChannelItem> = {}): ChannelItem =>
  ({
    id,
    name,
    adapter_id: "openai",
    status: 1,
    base_url: "https://api.example.com/v1",
    key_count: 3,
    key_enabled_count: 2,
    weight: 1,
    priority: 1,
    bound_models: [{ model: "gpt-4o" }],
    ...patch,
  }) as unknown as ChannelItem;

function renderFly(props: Partial<Parameters<typeof ChannelFlyout>[0]> = {}) {
  const onSelect = vi.fn();
  const onCreate = vi.fn();
  const onClose = vi.fn();
  // 锚点必须真挂到 DOM 上：定位函数拿不到 rect 会保持 visibility:hidden，
  // 而 hidden 的节点在 a11y 树里查不到（queryByRole 会报"没有可访问角色"）。
  const anchorRef = createRef<HTMLDivElement>();
  const alignRef = createRef<HTMLDivElement>();
  const utils = render(
    <>
      <div data-testid="anchor" ref={anchorRef} />
      <div data-testid="align" ref={alignRef} />
      <ChannelFlyout
        open
        adapterKey="openai"
        label="OpenAI"
        channels={[ch(11, "官方"), ch(12, "备用线路", { status: 3, weight: 6, priority: 2 })]}
        activeId={0}
        orphans={false}
        onClose={onClose}
        onSelect={onSelect}
        onCreate={onCreate}
        anchorRef={anchorRef}
        alignRef={alignRef}
        {...props}
      />
    </>,
  );
  return { ...utils, onSelect, onCreate, onClose };
}

describe("渠道浮层", () => {
  it("标题给全名与条数（200px 左栏放不下的部分在这里补回来）", () => {
    renderFly();
    expect(screen.getByRole("dialog", { name: "「OpenAI」的渠道" })).toBeTruthy();
    expect(screen.getByText("OpenAI")).toBeTruthy();
    expect(screen.getByText("2 条渠道")).toBeTruthy();
  });

  it("卡片带元信息：模型数 / 密钥可用数 / 非默认权重与优先级", () => {
    renderFly();
    const card = screen.getByRole("button", { name: /备用线路/ });
    expect(card.textContent).toContain("1 个模型");
    expect(card.textContent).toContain("密钥 2/3");
    expect(card.textContent).toContain("权重 6");
    expect(card.textContent).toContain("优先级 2");
    expect(card.textContent).toContain("自动停用");
    // 默认值不显示，避免每张卡都挂两段噪音
    expect(screen.getByRole("button", { name: /官方/ }).textContent).not.toContain("权重 1");
  });

  it("模型数区分可用/总数：停用的绑定不算可用", () => {
    renderFly({
      channels: [
        ch(11, "官方", {
          bound_models: [
            { model: "a", alias: "a", weight: 1, enabled: true },
            { model: "b", alias: "b", weight: 1, enabled: false },
          ],
        }),
      ],
    });
    // 只数长度会说「2 个模型」：实际只有 1 条在参与选路
    expect(screen.getByRole("button", { name: /官方/ }).textContent).toContain("1/2 个模型");
  });

  it("选中的那张卡带 aria-current，且全列表只有一张", () => {
    const { container } = renderFly({ activeId: 11 });
    const cur = container.ownerDocument.querySelectorAll('.ms-flych[aria-current="true"]');
    expect(cur).toHaveLength(1);
    expect(cur[0].textContent).toContain("官方");
  });

  it("点卡片回报渠道；「新建渠道」全浮层只有一个", () => {
    const { onSelect, onCreate } = renderFly();
    screen.getByRole("button", { name: /官方/ }).click();
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelect.mock.calls[0][0].id).toBe(11);

    const create = screen.getByRole("button", { name: "新建渠道" });
    create.click();
    expect(onCreate).toHaveBeenCalledTimes(1);
    expect(screen.getAllByRole("button", { name: "新建渠道" })).toHaveLength(1);
  });

  it("空态：这个适配器下还没有渠道（底部仍给新建入口）", () => {
    renderFly({ channels: [] });
    expect(screen.getByText("这个适配器下还没有渠道")).toBeTruthy();
    expect(screen.getByRole("button", { name: "新建渠道" })).toBeTruthy();
  });

  it("孤儿桶：底部是改绑说明，不提供新建", () => {
    renderFly({ orphans: true, channels: [ch(13, "失联线路", { status: 3 })] });
    expect(screen.queryByRole("button", { name: "新建渠道" })).toBeNull();
    expect(screen.getByText(/适配器已失效/)).toBeTruthy();
    // 孤儿卡不显示权重/优先级（改绑才是要解决的问题，不是调权重）
    expect(screen.getByRole("button", { name: /失联线路/ }).textContent).not.toContain("权重");
  });
});

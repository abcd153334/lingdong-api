import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { ChannelItem } from "@/types";

import { Rail } from "@/pages/admin/model-service/Rail";
import type { RailModel } from "@/pages/admin/model-service/grouping";

const ch = (id: number, name: string, adapterId: string, patch: Partial<ChannelItem> = {}): ChannelItem =>
  ({
    id,
    name,
    adapter_id: adapterId,
    status: 1,
    base_url: "https://example.com",
    key_count: 1,
    key_enabled_count: 1,
    weight: 1,
    priority: 1,
    bound_models: [],
    ...patch,
  }) as unknown as ChannelItem;

const model: RailModel = {
  groups: [
    {
      id: "spec:my-gw",
      label: "我的网关",
      builtin: false,
      iconUrl: "",
      disabled: false,
      channels: [ch(11, "主线路", "spec:my-gw"), ch(12, "备用线路", "spec:my-gw")],
    },
    {
      id: "openai",
      label: "OpenAI",
      builtin: true,
      iconUrl: "",
      disabled: false,
      channels: [ch(13, "官方", "openai")],
    },
  ],
  orphans: [ch(14, "失联线路", "spec:gone", { status: 3 })],
};

function renderRail(props: Partial<Parameters<typeof Rail>[0]> = {}) {
  const onToggleAdapter = vi.fn();
  const onToggleOrphans = vi.fn();
  const utils = render(
    <Rail
      model={model}
      open=""
      activeChannel={null}
      loading={false}
      error={null}
      onRetry={vi.fn()}
      drawer={false}
      drawerOpen={false}
      onCloseDrawer={vi.fn()}
      onToggleAdapter={onToggleAdapter}
      onToggleOrphans={onToggleOrphans}
      onShowAll={vi.fn()}
      onSelectChannel={vi.fn()}
      onCreateChannel={vi.fn()}
      onCreateAdapter={vi.fn()}
      onDrillBack={vi.fn()}
      onRestorePresets={vi.fn()}
      {...props}
    />,
  );
  return { ...utils, onToggleAdapter, onToggleOrphans };
}

describe("左栏：只有适配器", () => {
  it("任何时刻都不把渠道名写进左栏（渠道属于浮层）", () => {
    const { container } = renderRail();
    const rail = container.querySelector(".ms-rail") as HTMLElement;
    expect(rail.textContent).not.toContain("主线路");
    expect(rail.textContent).not.toContain("失联线路");
  });

  it("「全部模型」在列表之前，且在 loading / error 时仍然可点", () => {
    const { container, rerender } = renderRail({ loading: true });
    const rail = container.querySelector(".ms-rail") as HTMLElement;
    const all = within(rail).getByRole("button", { name: "全部模型" });
    const list = rail.querySelector(".ms-rail__list") as HTMLElement;
    expect(all.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    rerender(
      <Rail
        model={model}
        open=""
        activeChannel={null}
        loading={false}
        error={new Error("boom")}
        onRetry={vi.fn()}
        drawer={false}
        drawerOpen={false}
        onCloseDrawer={vi.fn()}
        onToggleAdapter={vi.fn()}
        onToggleOrphans={vi.fn()}
        onRestorePresets={vi.fn()}
        onShowAll={vi.fn()}
        onSelectChannel={vi.fn()}
        onCreateChannel={vi.fn()}
        onCreateAdapter={vi.fn()}
        onDrillBack={vi.fn()}
      />,
    );
    expect(screen.getByRole("button", { name: "全部模型" })).toBeTruthy();
    expect(screen.getByText("加载失败")).toBeTruthy();
  });

  it("卡片可访问名 = 名称 + 条数，计数本身不进可访问名", () => {
    renderRail();
    // 若把「2 条渠道」留在 a11y 树里，名字会变成「我的网关 2 条渠道 · 内置」
    expect(screen.getByRole("button", { name: "我的网关，2 条渠道" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "OpenAI，1 条渠道" })).toBeTruthy();
  });

  it("data-open 与 aria-current 分别跟随：两张卡可以同时有不同标记", () => {
    const { container } = renderRail({
      open: "spec:my-gw",
      activeChannel: ch(13, "官方", "openai"),
    });
    const openCard = container.querySelector('[data-adapter="spec:my-gw"]') as HTMLElement;
    const inViewCard = container.querySelector('[data-adapter="openai"]') as HTMLElement;
    expect(openCard.hasAttribute("data-open")).toBe(true);
    expect(openCard.getAttribute("aria-current")).toBeNull();
    expect(inViewCard.hasAttribute("data-open")).toBe(false);
    expect(inViewCard.getAttribute("aria-current")).toBe("true");
  });

  it("孤儿卡在适配器卡片之后，且不参与 aria-current", () => {
    const { container } = renderRail({ activeChannel: ch(14, "失联线路", "spec:gone") });
    const cards = [...container.querySelectorAll(".ms-rail__cards .ms-adp")];
    expect(cards).toHaveLength(3);
    expect(cards[2].getAttribute("data-orphan")).toBe("1");
    expect(cards[2].getAttribute("aria-current")).toBeNull();
  });

  it("点卡片把卡片节点一起交出去（浮层要用它做纵向锚点）", () => {
    const { container, onToggleAdapter } = renderRail();
    const card = container.querySelector('[data-adapter="openai"]') as HTMLElement;
    card.click();
    expect(onToggleAdapter).toHaveBeenCalledWith("openai", card);
  });

  it("抽屉形态：点卡片进钻取，底部换成「新建渠道」", () => {
    const { container } = renderRail({ drawer: true, drawerOpen: true, open: "openai" });
    const rail = container.querySelector(".ms-rail") as HTMLElement;
    expect(within(rail).getByRole("button", { name: "适配器" })).toBeTruthy();
    expect(within(rail).getByRole("button", { name: "新建渠道" })).toBeTruthy();
    // 钻取时不再渲染适配器卡片
    expect(rail.querySelector('[data-adapter="openai"]')).toBeNull();
  });
});

describe("左栏：恢复出厂入口", () => {
  const builtinModel: RailModel = {
    groups: [
      {
        id: "duomiapi",
        label: "多米API",
        builtin: false,
        iconUrl: "",
        disabled: false,
        channels: [ch(21, "多米API", "duomiapi", { preset_group: "builtin.duomiapi" })],
      },
    ],
    orphans: [],
  };

  it("存在内置行时渲染入口，位置在「新建适配器」之后", () => {
    const { container } = renderRail({ model: builtinModel });
    const foot = container.querySelector(".ms-rail__foot") as HTMLElement;
    const neu = within(foot).getByRole("button", { name: "新建适配器" });
    const reset = within(foot).getByRole("button", { name: "恢复出厂…" });
    // 维护动作排在新建动作之后：上面那个是本栏的主线，不能被一个批量覆盖动作顶掉
    expect(neu.compareDocumentPosition(reset) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("没有任何内置行时不渲染（关掉内置预设的实例不该看到必然 400 的入口）", () => {
    const { container } = renderRail(); // 默认 model 里渠道都没有 preset_group
    expect(container.querySelector(".ms-rail__foot .ms-reset")).toBeNull();
  });

  it("点击交给页面（页面负责先关抽屉再开弹窗）", () => {
    const onRestorePresets = vi.fn();
    renderRail({ model: builtinModel, onRestorePresets });
    screen.getByRole("button", { name: "恢复出厂…" }).click();
    expect(onRestorePresets).toHaveBeenCalledTimes(1);
  });
});

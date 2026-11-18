import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { ChannelItem } from "@/types";

import { Rail } from "@/pages/admin/model-service/Rail";
import type { RailModel } from "@/pages/admin/model-service/grouping";

const ch = (id: number, name: string, adapterId: string): ChannelItem =>
  ({
    id,
    name,
    adapter_id: adapterId,
    status: 1,
    base_url: "https://example.com",
    key_count: 1,
    key_enabled_count: 1,
    bound_models: [],
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
  ],
  orphans: [ch(13, "失联线路", "spec:gone")],
};

function renderRail(openId: string) {
  return render(
    <Rail
      model={model}
      openId={openId}
      focusId=""
      activeChannel={0}
      loading={false}
      error={null}
      onRetry={vi.fn()}
      drawer={false}
      drawerOpen={false}
      onCloseDrawer={vi.fn()}
      onToggleAdapter={vi.fn()}
      onSelectChannel={vi.fn()}
      onCreateChannel={vi.fn()}
      onCreateAdapter={vi.fn()}
    />,
  );
}

describe("左栏结构", () => {
  it("折叠的组不渲染渠道行（条件渲染，不是 display:none）", () => {
    renderRail("");
    // 组内渠道不在 DOM
    expect(screen.queryByText("主线路")).toBeNull();
    // 孤儿桶永远展开：它的渠道仍然在（否则失联渠道无处可见）
    expect(screen.getByText("失联线路")).toBeTruthy();
  });

  it("组头与行尾「+」是兄弟按钮，且只有组尾虚线槽叫「新建渠道」", () => {
    renderRail("spec:my-gw");
    // 行尾「+」的名字带适配器名：折叠态下多行各有一个，否则无法区分
    expect(screen.getByRole("button", { name: "在「我的网关」下新建渠道" })).toBeTruthy();
    // 组尾虚线槽是全页唯一的「新建渠道」（e2e 用 exact 定位，这里钉住唯一性）
    expect(screen.getAllByRole("button", { name: "新建渠道" })).toHaveLength(1);
  });

  it("计数不进可访问名（读屏只念实体名，不念「我的网关 2」）", () => {
    renderRail("spec:my-gw");
    expect(screen.getByRole("button", { name: "我的网关" })).toBeTruthy();
  });
});

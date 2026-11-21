import { useState } from "react";
import { describe, expect, it } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { PageTabPanels, PageTabs } from "@/components/common/PageTabs";
import type { PageTab } from "@/lib/pageTabs";

type K = "a" | "b" | `plugin:${string}`;

/** 三个页签：两个静态 + 一个运行期（模拟"已安装插件"）。 */
const TABS: readonly PageTab<K>[] = [
  { key: "a", label: "甲" },
  { key: "b", label: "乙" },
  { key: "plugin:x", label: "工单" },
];

/**
 * 面板回收：页签在运行期消失（卸载插件、删最后一篇文档）时，
 * 它的面板必须一起卸载。
 *
 * 为什么必须有这条：`PanelStack.seen` 只增不减是"保活"的实现方式，
 * 没有回收时被移除页签的面板会常驻 —— 继续持有查询、继续占着 DOM id。
 */
describe("PageTabs 面板回收", () => {
  function Harness() {
    const [tabs, setTabs] = useState<readonly PageTab<K>[]>(TABS);
    const [value, setValue] = useState<K>("a");
    return (
      <>
        <PageTabs
          tabs={tabs}
          value={value}
          onChange={setValue}
          idPrefix="probe"
          label="探针"
          keepMounted
        >
          {(k) => <div data-testid={`panel-${k}`}>panel {k}</div>}
        </PageTabs>
        <button
          type="button"
          onClick={() => {
            setTabs(TABS.slice(0, 2)); // 移除 plugin:x
            setValue("a");
          }}
        >
          卸载插件
        </button>
      </>
    );
  }

  it("未访问过的页签不挂载；访问后保活；页签消失即卸载", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    // 首帧只挂当前面板（keepMounted 只保活"已访问过"的）
    expect(screen.getByTestId("panel-a")).toBeInTheDocument();
    expect(screen.queryByTestId("panel-b")).toBeNull();

    await user.click(screen.getByRole("tab", { name: "工单" }));
    expect(screen.getByTestId("panel-plugin:x")).toBeInTheDocument();

    await user.click(screen.getByRole("tab", { name: "乙" }));
    // 切走后仍然保活（长表单不丢草稿的既有语义）
    expect(screen.getByTestId("panel-plugin:x")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "卸载插件" }));
    await waitFor(() => expect(screen.queryByTestId("panel-plugin:x")).toBeNull());
    // 静态页签的面板不受影响
    expect(screen.getByTestId("panel-a")).toBeInTheDocument();
  });
});

/**
 * 不传 `keys` 时行为必须与改造前逐字一致：`/admin/docs` 的编辑页用
 * `PageTabPanels`（页签条在顶栏里，组件看不到 tabs），它依赖"只增不减"的保活。
 */
describe("PageTabPanels 不传 keys 时不受回收影响", () => {
  function Harness() {
    const [value, setValue] = useState<"a" | "b">("a");
    return (
      <>
        <PageTabPanels value={value} idPrefix="probe2" keepMounted>
          {(k) => <div data-testid={`p2-${k}`}>panel {k}</div>}
        </PageTabPanels>
        <button type="button" onClick={() => setValue("b")}>
          去乙
        </button>
        <button type="button" onClick={() => setValue("a")}>
          去甲
        </button>
      </>
    );
  }

  it("来回切换后两个面板都还在", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.click(screen.getByRole("button", { name: "去乙" }));
    expect(screen.getByTestId("p2-b")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "去甲" }));
    expect(screen.getByTestId("p2-a")).toBeInTheDocument();
    expect(screen.getByTestId("p2-b")).toBeInTheDocument();
  });
});

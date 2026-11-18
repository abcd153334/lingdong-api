import { describe, expect, it } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import { usePageTabs, type PageTab } from "@/lib/pageTabs";
import { tabSearch } from "@/lib/searchParams";

/* 独立最小路由树：不依赖 appRouter / 登录态 / 后端桩 */

type ProbeTab = "a" | "b" | "c";

const TABS: readonly PageTab<ProbeTab>[] = [
  { key: "a", label: "甲" },
  { key: "b", label: "乙" },
  { key: "c", label: "丙", disabled: true },
];

function Probe() {
  const { tab, setTab } = usePageTabs(TABS, "a");
  return (
    <div>
      <output data-testid="cur">{tab}</output>
      {TABS.map((t) => (
        <button key={t.key} type="button" onClick={() => setTab(t.key)}>
          {`去 ${t.label}`}
        </button>
      ))}
    </div>
  );
}

function renderProbe(path: string) {
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const probeRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/probe",
    validateSearch: tabSearch,
    component: Probe,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([probeRoute]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(<RouterProvider router={router} />);
  return router;
}

const hrefOf = (router: ReturnType<typeof renderProbe>) => router.state.location.href;

describe("usePageTabs", () => {
  it("默认页签不写 URL 参数", async () => {
    const router = renderProbe("/probe");
    expect(await screen.findByTestId("cur")).toHaveTextContent("a");
    expect(hrefOf(router)).not.toContain("tab=");
  });

  it("切页签写入 ?tab=，且保留页面其它 search 参数", async () => {
    const user = userEvent.setup();
    const router = renderProbe("/probe?keep=1");
    expect(await screen.findByTestId("cur")).toHaveTextContent("a");

    await user.click(screen.getByRole("button", { name: "去 乙" }));
    await waitFor(() => expect(hrefOf(router)).toContain("tab=b"));
    expect(hrefOf(router)).toContain("keep=1");
    expect(screen.getByTestId("cur")).toHaveTextContent("b");
  });

  it("切回默认页签时删除参数（规范 URL 收敛）", async () => {
    const user = userEvent.setup();
    const router = renderProbe("/probe?tab=b&keep=1");
    expect(await screen.findByTestId("cur")).toHaveTextContent("b");

    await user.click(screen.getByRole("button", { name: "去 甲" }));
    await waitFor(() => expect(hrefOf(router)).not.toContain("tab="));
    expect(hrefOf(router)).toContain("keep=1");
  });

  it("非法值回落默认页签并纠偏（replace，不残留参数）", async () => {
    const router = renderProbe("/probe?tab=nope&keep=1");
    expect(await screen.findByTestId("cur")).toHaveTextContent("a");
    await waitFor(() => expect(hrefOf(router)).not.toContain("tab="));
    expect(hrefOf(router)).toContain("keep=1");
  });

  it("disabled 页签不响应点击（不导航、不改选中项）", async () => {
    const user = userEvent.setup();
    const router = renderProbe("/probe");
    expect(await screen.findByTestId("cur")).toHaveTextContent("a");

    await user.click(screen.getByRole("button", { name: "去 丙" }));
    expect(screen.getByTestId("cur")).toHaveTextContent("a");
    expect(hrefOf(router)).not.toContain("tab=");
  });

  it("URL 落在 disabled 页签上时回落默认值并纠偏", async () => {
    const router = renderProbe("/probe?tab=c");
    expect(await screen.findByTestId("cur")).toHaveTextContent("a");
    await waitFor(() => expect(hrefOf(router)).not.toContain("tab="));
  });

  it("数字型参数（?tab=7）被归一为字符串且不误命中", async () => {
    const router = renderProbe("/probe?tab=7");
    expect(await screen.findByTestId("cur")).toHaveTextContent("a");
    await waitFor(() => expect(hrefOf(router)).not.toContain("tab="));
  });
});

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RouteError, RoutePending } from "@/components/common/RouteError";

// 全仓此前没有 ErrorBoundary：渲染异常会落到 TanStack 的英文默认页。
// 这条守住三件事：有中文标题、有可点的重试、有回首页的出口。
describe("RouteError", () => {
  it("渲染中文标题与错误原文，并提供重试与回首页", async () => {
    const reset = vi.fn();
    render(<RouteError error={new Error("Cannot read properties of null")} reset={reset} />);

    expect(screen.getByTestId("route-error")).toBeInTheDocument();
    expect(screen.getByText("页面加载失败")).toBeInTheDocument();
    expect(screen.getByText("Cannot read properties of null")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(reset).toHaveBeenCalledTimes(1);

    expect(screen.getByRole("link", { name: "返回首页" })).toHaveAttribute("href", "/home");
    // 不再出现英文兜底文案
    expect(screen.queryByText(/Something went wrong/)).toBeNull();
  });

  it("未知错误也有兜底文案（不显示 undefined）", () => {
    render(<RouteError error={"boom" as unknown as Error} reset={() => {}} />);
    expect(screen.getByText("页面渲染时发生未知错误")).toBeInTheDocument();
  });
});

describe("RoutePending", () => {
  it("给出加载态而不是空白", () => {
    render(<RoutePending />);
    expect(screen.getByText("正在加载…")).toBeInTheDocument();
  });
});

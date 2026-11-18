import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ColumnDef } from "@tanstack/react-table";
import { DataTable } from "@/components/common/DataTable";
import { Pagination } from "@/components/common/Pagination";
import {
  orderColumns,
  usageColumns,
  type OrderRowLike,
  type UsageRowLike,
} from "@/lib/table";

interface Row {
  id: string;
  name: string;
  status: number;
}

const columns: ColumnDef<Row, unknown>[] = [
  { accessorKey: "id", header: "请求 ID", meta: { label: "请求 ID" } },
  { accessorKey: "name", header: "模型", meta: { label: "模型" } },
  { accessorKey: "status", header: "状态", meta: { label: "状态" } },
];

const rows: Row[] = [
  { id: "req-1", name: "gpt-4o", status: 200 },
  { id: "req-2", name: "wan2.1", status: 429 },
];

describe("DataTable", () => {
  it("渲染表头与数据行，并为移动端卡片化输出 data-label", () => {
    const { container } = render(<DataTable columns={columns} data={rows} />);
    expect(screen.getByText("模型")).toBeInTheDocument();
    expect(container.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(container.querySelector("tbody td")?.getAttribute("data-label")).toBe("请求 ID");
  });

  it("空数据显示空态（可自定义）", () => {
    render(
      <DataTable
        columns={columns}
        data={[]}
        empty={<div data-testid="empty">暂无调用记录</div>}
      />,
    );
    expect(screen.getByTestId("empty")).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("服务端分页：传 total 与 onPageChange 时渲染分页条并回传页码", async () => {
    const onPageChange = vi.fn();
    const user = userEvent.setup();
    render(
      <DataTable columns={columns} data={rows} total={87} page={2} onPageChange={onPageChange} />,
    );
    expect(screen.getByText("共 87 条 · 每页 20 条")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "3" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "3" }));
    expect(onPageChange).toHaveBeenCalledWith(3);
  });

  it("列 meta.label 同时驱动表头排序点击", async () => {
    const user = userEvent.setup();
    const { container } = render(<DataTable columns={columns} data={rows} />);
    await user.click(screen.getByText("模型"));
    expect(container.querySelector("th")?.textContent ?? "").toContain("请求 ID");
  });
});

describe("Pagination", () => {
  it("页码窗口折叠、首末页可达、边界按钮禁用", async () => {
    const onPageChange = vi.fn();
    const user = userEvent.setup();
    render(<Pagination total={400} page={1} onPageChange={onPageChange} />);
    expect(screen.getByRole("button", { name: "上一页" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "20" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "下一页" }));
    expect(onPageChange).toHaveBeenCalledWith(2);
  });

  it("单页时不渲染页码按钮", () => {
    render(<Pagination total={5} page={1} onPageChange={() => {}} />);
    expect(screen.queryByRole("button", { name: "下一页" })).toBeNull();
  });
});

describe("列工厂", () => {
  const labels = (cols: { meta?: unknown }[]) =>
    cols.map((c) => (c.meta as { label: string }).label);

  it("orderColumns：基础列顺序固定，扩展位插入到约定位置", () => {
    expect(labels(orderColumns<OrderRowLike>())).toEqual([
      "订单号",
      "金额",
      "到账金额",
      "状态",
      "创建时间",
    ]);
    expect(
      labels(
        orderColumns<OrderRowLike>({
          user: () => null,
          payChannel: () => null,
          actions: () => null,
        }),
      ),
    ).toEqual(["订单号", "用户", "金额", "到账金额", "状态", "支付渠道", "创建时间", "操作"]);
  });

  it("usageColumns：默认列与可选密钥/耗时列", () => {
    expect(labels(usageColumns<UsageRowLike>())).toEqual([
      "时间",
      "模型",
      "类型",
      "Token",
      "费用",
      "状态",
    ]);
    expect(
      labels(usageColumns<UsageRowLike>({ keyName: (id) => `#${id}`, withLatency: true })),
    ).toEqual(["时间", "模型", "类型", "密钥", "Token", "费用", "耗时", "状态"]);
  });

  it("订单金额按「分」转「元」展示", () => {
    const row: OrderRowLike = {
      out_trade_no: "T20261020001",
      amount_cent: 5000,
      credited: 250000,
      status: "paid",
      created_at: "2026-10-20T10:00:00Z",
    };
    render(<DataTable columns={orderColumns<OrderRowLike>()} data={[row]} />);
    expect(screen.getByText("¥50.00")).toBeInTheDocument();
  });
});

// 2026-10-29 审计 A-04：列表请求失败时必须显示「加载失败」，不能落进「暂无数据」。
// 没有这三条的话，任何一次 500 都会被渲染成"这个用户没有记录"，用户会去别处找。
describe("DataTable 状态优先级", () => {
  it("失败态优先于空态（失败不等于没有数据）", () => {
    render(
      <DataTable
        columns={columns}
        data={[]}
        error={<div data-testid="err">加载失败</div>}
        empty={<div data-testid="empty">暂无数据</div>}
      />,
    );
    expect(screen.getByTestId("err")).toBeInTheDocument();
    expect(screen.queryByTestId("empty")).not.toBeInTheDocument();
  });

  it("加载态优先于空态，且失败态优先于加载态", () => {
    const { rerender } = render(
      <DataTable columns={columns} data={[]} loading empty={<div data-testid="empty">空</div>} />,
    );
    expect(screen.queryByTestId("empty")).not.toBeInTheDocument();

    rerender(
      <DataTable
        columns={columns}
        data={[]}
        loading
        error={<div data-testid="err">失败</div>}
        empty={<div data-testid="empty">空</div>}
      />,
    );
    expect(screen.getByTestId("err")).toBeInTheDocument();
  });

  it("不传新 prop 时行为不变（有数据就渲染表格）", () => {
    render(<DataTable columns={columns} data={[{ id: "r1", name: "m", status: 1 }]} />);
    expect(screen.getByText("r1")).toBeInTheDocument();
  });
});

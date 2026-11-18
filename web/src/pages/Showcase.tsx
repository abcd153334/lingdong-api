import { useState } from "react";
import { Link } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { toast } from "sonner";
import { PageHead } from "@/components/common/PageHead";
import { Field } from "@/components/common/Field";
import { Card } from "@/components/common/Card";
import { StatCard } from "@/components/common/StatCard";
import { EmptyState } from "@/components/common/EmptyState";
import { StatusTag, ModalityTag } from "@/components/common/StatusTag";
import { AmountText, MoneyText, QuotaUnit } from "@/components/common/MoneyText";
import { Tabs } from "@/components/common/Tabs";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Select } from "@/components/common/Select";
import { FilterBar } from "@/components/common/FilterBar";
import { DataTable } from "@/components/common/DataTable";
import { Pagination } from "@/components/common/Pagination";
import { Tip } from "@/components/common/Tip";
import { Icon, ICON_MAP, type IconName } from "@/components/common/Icon";
import { Dialog, Sheet } from "@/components/common/Dialog";
import { Drawer } from "@/components/common/Drawer";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { TrendChart, BarsChart, RankBars } from "@/components/common/Charts";
import { SiteLogo } from "@/components/common/SiteLogo";
import { ThemeToggle } from "@/components/chrome/ThemeToggle";

interface DemoRow {
  id: string;
  name: string;
  modality: string;
  status: number;
  cost: number;
}

const demoRows: DemoRow[] = [
  { id: "req-1", name: "gpt-4o", modality: "chat", status: 200, cost: 12500 },
  { id: "req-2", name: "wan2.1-t2v", modality: "video", status: 429, cost: 320000 },
  { id: "req-3", name: "suno-v3", modality: "audio", status: 500, cost: 48000 },
];

const demoColumns: ColumnDef<DemoRow, unknown>[] = [
  { accessorKey: "id", header: "请求 ID", meta: { label: "请求 ID" } },
  {
    accessorKey: "name",
    header: "模型",
    meta: { label: "模型" },
    cell: (ctx) => <span className="mono">{String(ctx.getValue())}</span>,
  },
  {
    accessorKey: "modality",
    header: "模态",
    meta: { label: "模态" },
    cell: (ctx) => <ModalityTag modality={String(ctx.getValue())} />,
  },
  {
    accessorKey: "status",
    header: "状态",
    meta: { label: "状态" },
    cell: (ctx) => <StatusTag status={ctx.getValue() as number} />,
  },
  {
    accessorKey: "cost",
    header: "费用",
    meta: { label: "费用" },
    cell: (ctx) => <QuotaUnit micro={ctx.getValue() as number} />,
  },
];

const trendData = [
  { day: "10-13", calls: 820, token: 410 },
  { day: "10-14", calls: 960, token: 520 },
  { day: "10-15", calls: 1180, token: 690 },
  { day: "10-16", calls: 1040, token: 610 },
  { day: "10-17", calls: 1420, token: 880 },
];

const rankData = [
  { label: "gpt-4o", value: 4820 },
  { label: "claude-3.7", value: 3310 },
  { label: "wan2.1-t2v", value: 2140 },
  { label: "suno-v3", value: 980 },
];

/** 设计系统可视化验证台（/__showcase）：S1 全部通用组件的真实渲染样例 */
export function Showcase() {
  const [tab, setTab] = useState("basic");
  const [chip, setChip] = useState("all");
  const [sw1, setSw1] = useState(true);
  const [sw2, setSw2] = useState(false);
  const [sel, setSel] = useState("chat");
  const [filter, setFilter] = useState<Record<string, string>>({ q: "", modality: "" });
  const [page, setPage] = useState(2);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [sheetOpen, setSheetOpen] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);

  return (
    <div className="p-main">
      <PageHead
        title="设计系统展示台"
        desc="S1 通用组件真实渲染验证（/__showcase）"
        actions={[
          { type: "btn", label: "主要动作", variant: "primary", icon: "plus" },
          { type: "link", label: "返回门户", href: "/home", icon: "external" },
        ]}
      />
      <div className="row-acts">
        <ThemeToggle />
        <SiteLogo />
        <div className="muted small row-acts">
          <span>图标：</span>
          {Object.keys(ICON_MAP).map((k) => (
            <Icon key={k} name={k as IconName} size={15} />
          ))}
        </div>
      </div>

      <Card title="统计卡与徽标" sub="StatCard / StatusTag / ModalityTag / Tip" actions={<Tip text="问号提示支持多行文案；触屏点击展开，桌面 hover 呈现。" />}>
        <div className="grid grid-4">
          <StatCard icon="wallet" label="余额" value="$12.4800" sub="≈ ¥89.86" />
          <StatCard icon="rows" label="今日调用" value="1,420" sub={<span className="up">+18%</span>} />
          <StatCard icon="coin" label="今日消耗" value="¥36.20" sub="token 12.4M" />
          <StatCard icon="key" label="活跃密钥" value="4" sub="1 个已自动禁用" />
        </div>
        <div className="row-acts mt16">
          <StatusTag status={1} />
          <StatusTag status={0} />
          <StatusTag status={3} />
          <StatusTag status="paid" />
          <StatusTag status="pending" />
          <StatusTag status="expired" />
          <StatusTag status="running" />
          <StatusTag status="failed" />
          <StatusTag status={200} />
          <StatusTag status={429} />
          <StatusTag status={502} />
          <ModalityTag modality="chat" />
          <ModalityTag modality="image" />
          <ModalityTag modality="audio" />
          <ModalityTag modality="video" />
          <ModalityTag modality="embedding" />
        </div>
        <div className="row-acts mt16">
          <MoneyText micro={125000} />
          <MoneyText micro={6400000} signed />
          <AmountText micro={125000} />
          <QuotaUnit micro={320000} />
          <span className="tooltip" data-tip="悬停说明（数据项）">
            数据项悬停
          </span>
        </div>
      </Card>

      <Card title="选项卡与胶囊" sub="Tabs / SegmentedChips">
        <Tabs
          items={[
            { key: "basic", label: "基础" },
            { key: "billing", label: "计费" },
            { key: "notice", label: "通知" },
            { key: "storage", label: "云存储" },
            { key: "sms", label: "短信" },
            { key: "brand", label: "品牌" },
            { key: "security", label: "安全" },
          ]}
          value={tab}
          onChange={setTab}
        />
        <Tabs
          variant="pill"
          items={[
            { key: "all", label: "全部" },
            { key: "chat", label: "对话" },
            { key: "image", label: "图像" },
            { key: "video", label: "视频" },
          ]}
          value={chip}
          onChange={setChip}
        />
        <div className="mt16">
          <SegmentedChips
            items={[
              { key: "all", label: "全部" },
              { key: "billing", label: "按量计费" },
              { key: "fixed", label: "按次计费" },
            ]}
            value={chip}
            onChange={setChip}
          />
        </div>
      </Card>

      <Card title="表单原语" sub="SwitchRow / Select">
        <SwitchRow label="启用渠道" checked={sw1} onChange={setSw1} tip="关闭后该渠道不参与路由。" />
        <SwitchRow label="允许自动禁用" checked={sw2} onChange={setSw2} />
        <Field label="计费分组" htmlFor="showcase-sel">
          <Select
            id="showcase-sel"
            items={[
              ["chat", "对话计费组"],
              ["media", "媒体计费组"],
            ]}
            allLabel="全部计费组"
            value={sel}
            onChange={(e) => setSel(e.target.value)}
          />
        </Field>
      </Card>

      <Card title="头部筛选条" sub="FilterBar（桌面内联 / ≤767px 折叠为底部 Sheet）">
        <FilterBar
          fields={[
            { id: "q", t: "search", label: "关键词", placeholder: "搜索请求 ID / 模型" },
            {
              id: "modality",
              t: "select",
              label: "模态",
              allLabel: "全部模态",
              items: [
                ["chat", "对话"],
                ["video", "视频"],
              ],
            },
            { id: "go", t: "btn", label: "查询", variant: "primary", icon: "search" },
          ]}
          value={filter}
          onChange={(id, v) => setFilter((prev) => ({ ...prev, [id]: v }))}
          onReset={() => setFilter({})}
          onAction={() => toast.success("已查询")}
        />
        <DataTable columns={demoColumns} data={demoRows} />
        <DataTable
          className="mt16"
          columns={demoColumns}
          data={[]}
          empty={<EmptyState icon="◇" title="暂无调用记录" desc="调整筛选条件后重试" />}
        />
        <Pagination total={87} page={page} pageSize={20} onPageChange={setPage} />
      </Card>

      <Card title="浮层" sub="Dialog / Sheet / Drawer / confirmDialog">
        <div className="row-acts">
          <button type="button" className="btn" onClick={() => setDialogOpen(true)}>
            打开对话框
          </button>
          <button type="button" className="btn" onClick={() => setSheetOpen(true)}>
            打开底部 Sheet
          </button>
          <button type="button" className="btn" onClick={() => setDrawerOpen(true)}>
            打开抽屉
          </button>
          <button
            type="button"
            className="btn btn-danger"
            onClick={() => {
              void confirmDialog({
                title: "删除渠道",
                body: "删除后该渠道下密钥池一并失效，确认删除？",
                okText: "删除",
                okVariant: "danger",
              }).then((ok) => (ok ? toast.success("已删除") : toast("已取消")));
            }}
          >
            确认对话框
          </button>
        </div>
        <Dialog
          open={dialogOpen}
          onClose={() => setDialogOpen(false)}
          title="对话框标题"
          actions={[
            { label: "取消", onClick: () => setDialogOpen(false) },
            { label: "确认", variant: "primary", onClick: () => setDialogOpen(false) },
          ]}
        >
          <Field label="名称">
            <input type="text" placeholder="请输入名称" />
          </Field>
        </Dialog>
        <Sheet
          open={sheetOpen}
          onClose={() => setSheetOpen(false)}
          title="底部 Sheet"
          actions={[{ label: "完成", variant: "primary", onClick: () => setSheetOpen(false) }]}
        >
          <Field label="说明">
            <input type="text" placeholder="移动端表单" />
          </Field>
        </Sheet>
        <Drawer
          open={drawerOpen}
          onClose={() => setDrawerOpen(false)}
          title="详情抽屉"
          actions={[{ label: "保存", variant: "primary", onClick: () => setDrawerOpen(false) }]}
        >
          <div className="kv-grid">
            <div className="kv">
              <div className="k">名称</div>
              <div className="v">gpt-4o</div>
            </div>
            <div className="kv">
              <div className="k">模态</div>
              <div className="v">chat</div>
            </div>
            <div className="kv">
              <div className="k">余额</div>
              <div className="v">$12.4800</div>
            </div>
          </div>
        </Drawer>
      </Card>

      <Card title="图表" sub="TrendChart / BarsChart / RankBars">
        <TrendChart
          data={trendData}
          xKey="day"
          series={[
            { key: "calls", name: "调用次数" },
            { key: "token", name: "Token（K）" },
          ]}
        />
        <BarsChart
          data={trendData}
          xKey="day"
          series={[{ key: "calls", name: "调用次数" }]}
          height={180}
        />
        <RankBars data={rankData} unit=" 次" />
      </Card>

      <div className="row-acts">
        <Link to="/home" className="btn">
          <Icon name="home" size={15} />
          门户主页
        </Link>
        <Link to="/admin/dashboard" className="btn">
          <Icon name="grid" size={15} />
          管理台仪表盘
        </Link>
      </div>
    </div>
  );
}

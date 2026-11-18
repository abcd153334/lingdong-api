import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { Icon, ICON_MAP } from "@/components/common/Icon";
import { Tip } from "@/components/common/Tip";
import { PageHead } from "@/components/common/PageHead";
import { StatCard } from "@/components/common/StatCard";
import { EmptyState } from "@/components/common/EmptyState";
import { ModalityTag, StatusTag, Tag, LogKindTag, fileTypeKey, tagOf } from "@/components/common/StatusTag";
import {
  AmountText,
  MoneyText,
  QuotaUnit,
  setSecondaryCurrencyEnabled,
} from "@/components/common/MoneyText";
import { resetMoneyCtx, setMoneyCtx } from "@/lib/money";
import { SiteLogo } from "@/components/common/SiteLogo";

// SiteLogo 的品牌来源是 useSite()（真实实现依赖 QueryClient）；其余用例都不碰站点配置，
// 这里只替换这一个 hook，避免为了一个断言把整份单测拖进 provider 依赖。
vi.mock("@/lib/site", () => ({
  useSite: () => ({ name: "灵动 API", logoImage: "", favicon: "" }),
}));

describe("Icon", () => {
  it("按原型语义键渲染图标，并覆盖全部图标键", () => {
    render(
      <div>
        <Icon name="home" />
        {Object.keys(ICON_MAP).map((k) => (
          <Icon key={k} name={k as keyof typeof ICON_MAP} />
        ))}
      </div>,
    );
    expect(document.querySelectorAll("svg").length).toBe(Object.keys(ICON_MAP).length + 1);
  });
});

describe("Tip", () => {
  it("渲染问号提示并携带多行文案", () => {
    render(<Tip text={"第一行\n第二行"} />);
    const el = screen.getByRole("button");
    expect(el).toHaveClass("tip");
    expect(el).toHaveAttribute("data-tip", "第一行\n第二行");
  });

  it("可访问名固定为「说明」，规则文本放进被引用的隐藏节点", () => {
    render(<Tip text="32 位，用于解密回调" />);
    const el = screen.getByRole("button");
    // aria-label 不能用整段规则：否则外层 label 的可访问名会被污染
    expect(el).toHaveAttribute("aria-label", "说明");
    const descId = el.getAttribute("aria-describedby");
    expect(descId).toBeTruthy();
    expect(document.getElementById(descId as string)?.textContent).toBe("32 位，用于解密回调");
  });

  it("键盘 Enter/Space 能开合，Esc 能关闭", () => {
    render(<Tip text="规则说明" />);
    const el = screen.getByRole("button");
    expect(el).not.toHaveClass("open");
    fireEvent.keyDown(el, { key: "Enter" });
    expect(el).toHaveClass("open");
    fireEvent.keyDown(el, { key: "Escape" });
    expect(el).not.toHaveClass("open");
    fireEvent.keyDown(el, { key: " " });
    expect(el).toHaveClass("open");
  });
});

describe("PageHead", () => {
  it("渲染标题/描述/动作，并输出 aria 锚点 id", () => {
    render(
      <PageHead
        title="渠道管理"
        desc="上游渠道与密钥池"
        actions={[
          { type: "btn", label: "新建渠道", variant: "primary" },
          { type: "link", label: "文档", href: "https://example.com" },
        ]}
      />,
    );
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("渠道管理");
    expect(document.getElementById("pageTitle")).toBeTruthy();
    expect(document.getElementById("pageDesc")).toHaveTextContent("上游渠道与密钥池");
    expect(screen.getByRole("button", { name: /新建渠道/ })).toHaveClass("btn-primary");
    expect(screen.getByRole("link", { name: "文档" })).toHaveAttribute("href", "https://example.com");
  });
});

describe("StatCard", () => {
  it("渲染指标标签、数值与副文案", () => {
    render(<StatCard icon="wallet" label="余额" value="$12.4800" sub="≈ ¥89.86" />);
    expect(screen.getByText("余额")).toBeInTheDocument();
    expect(screen.getByText("$12.4800")).toHaveClass("value");
    expect(screen.getByText("≈ ¥89.86")).toHaveClass("sub");
  });

  it("loading 显示 — 而非传入的值，并标记 aria-busy", () => {
    // 传入 0 是刻意的：加载中显示 0 等于替接口断言「今天没有调用」
    render(<StatCard label="今日请求" value="0" status="loading" />);
    const card = document.querySelector(".stat-card") as HTMLElement;
    expect(card).toHaveAttribute("aria-busy", "true");
    expect(card.querySelector(".value")).toHaveTextContent("—");
    expect(card).not.toHaveAttribute("role");
  });

  it("error 显示 — 与错误文案、可重试，并作为 alert 播报", () => {
    const onRetry = vi.fn();
    render(<StatCard label="今日请求" value="0" status="error" onRetry={onRetry} />);
    const card = screen.getByRole("alert");
    expect(card).toHaveClass("stat-card", "stat-card--error");
    expect(card.querySelector(".value")).toHaveTextContent("—");
    expect(screen.getByText("加载失败")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
    // error 时不再渲染 sub：失败的情况下副文案里的数字没有来源
    expect(card.querySelector(".sub")).toHaveTextContent(/^加载失败/);
  });

  it("ready 缺省：既有调用点不传 status 也照旧渲染", () => {
    render(<StatCard label="渠道数" value="12" />);
    const card = document.querySelector(".stat-card") as HTMLElement;
    expect(card).not.toHaveAttribute("role");
    expect(card.querySelector(".value")).toHaveTextContent("12");
    // ready 时不显示错误文案，也不需要重试按钮
    expect(screen.queryByText("加载失败")).toBeNull();
    expect(screen.queryByRole("button", { name: "重试" })).toBeNull();
  });
});

describe("EmptyState", () => {
  it("渲染标题、说明与动作，并支持自定义内边距", () => {
    render(
      <EmptyState
        icon="◇"
        title="暂无数据"
        desc="调整筛选后重试"
        pad="24px"
        action={<button type="button">刷新</button>}
      />,
    );
    expect(screen.getByText("暂无数据")).toHaveClass("title");
    expect(screen.getByText("调整筛选后重试")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "刷新" })).toBeInTheDocument();
    expect(document.querySelector(".empty")).toHaveStyle({ padding: "24px" });
  });

  it("variant 提供默认图标与语义：loading / error / denied", () => {
    const { unmount } = render(<EmptyState variant="loading" title="加载中…" />);
    expect(document.querySelector(".empty .icon")?.textContent).toBe("…");
    expect(document.querySelector(".empty")).toHaveAttribute("aria-busy", "true");
    unmount();

    render(<EmptyState variant="error" title="加载失败" />);
    expect(document.querySelector(".empty .icon")?.textContent).toBe("⚠");
    expect(screen.getByRole("alert")).toBeInTheDocument();
  });

  it("显式 icon={null} 时不渲染图标位", () => {
    render(<EmptyState variant="empty" icon={null} title="暂无数据" />);
    expect(document.querySelector(".empty .icon")).toBeNull();
  });
});

describe("StatusTag / ModalityTag", () => {
  it("实体状态映射到集中色彩与文案", () => {
    render(
      <div>
        <StatusTag status={1} />
        <StatusTag status={0} />
        <StatusTag status={3} />
        <StatusTag status="paid" />
        <StatusTag status="running" />
      </div>,
    );
    expect(screen.getByText("启用")).toHaveClass("badge-success");
    expect(screen.getByText("停用")).toHaveClass("badge-gray");
    expect(screen.getByText("自动禁用")).toHaveClass("badge-danger");
    expect(screen.getByText("已支付")).toHaveClass("badge-success");
    expect(screen.getByText("运行中").querySelector(".pulse")).toBeTruthy();
  });

  it("HTTP 状态码映射：2xx 成功 / 429 警告 / ≥400 危险", () => {
    render(
      <div>
        <StatusTag status={200} />
        <StatusTag status={429} />
        <StatusTag status={502} />
      </div>,
    );
    expect(screen.getByText("200")).toHaveClass("badge-success");
    expect(screen.getByText("429")).toHaveClass("badge-warning");
    expect(screen.getByText("502")).toHaveClass("badge-danger");
  });

  it("模态徽标覆盖媒体类模态", () => {
    render(
      <div>
        <ModalityTag modality="image" />
        <ModalityTag modality="video" />
      </div>,
    );
    expect(screen.getByText("图像")).toHaveClass("badge-purple");
    expect(screen.getByText("视频")).toHaveClass("badge-danger");
  });

  it("日志事件使用独立口径（pending=冻结中，不与订单待支付混淆）", () => {
    render(
      <div>
        <LogKindTag kind="consume" />
        <LogKindTag kind="pending" />
        <LogKindTag kind="upstream_error" />
      </div>,
    );
    expect(screen.getByText("消耗")).toHaveClass("badge-success");
    expect(screen.getByText("冻结中")).toHaveClass("badge-info");
    expect(screen.getByText("上游错误")).toHaveClass("badge-danger");
  });

  it("Tag / tagOf / fileTypeKey：文件类型与未知键降级", () => {
    render(<Tag tone="warning">待处理</Tag>);
    expect(screen.getByText("待处理")).toHaveClass("badge-warning");
    expect(tagOf("image")).toEqual(["info", "图片"]);
    expect(tagOf("unknown-kind")).toEqual(["gray", "unknown-kind"]);
    expect(fileTypeKey("image/png")).toBe("image");
    expect(fileTypeKey("audio/mpeg")).toBe("audio");
    expect(fileTypeKey("video/mp4")).toBe("video");
    expect(fileTypeKey("application/pdf")).toBe("doc");
    expect(fileTypeKey("application/octet-stream")).toBe("other");
  });
});

describe("MoneyText / AmountText / QuotaUnit", () => {
  // 金额口径是全局可变的（结算币种），每个用例必须自己声明
  // "我在什么币种下断言"，否则测试结果会跟着部署配置漂移。
  afterEach(() => {
    resetMoneyCtx();
    setSecondaryCurrencyEnabled(false);
  });

  it("人民币口径下按 scale 换算，空值降级为占位", () => {
    setMoneyCtx({ currency: "CNY", scale: 1_000_000, otherCurrency: "USD", cnyPerUsd: 7.2 });
    render(
      <div>
        <MoneyText micro={7_200_000} />
        <AmountText micro={7_200_000} />
        <QuotaUnit micro={1_800_000} />
        <MoneyText micro={null} />
      </div>,
    );
    // 7200000 微 = ¥7.2；AmountText 不带符号；QuotaUnit 同金额口径
    expect(screen.getByText("¥7.2")).toHaveClass("mono");
    expect(screen.getByText("7.2")).toBeInTheDocument();
    expect(screen.getByText("¥1.8")).toBeInTheDocument();
    expect(screen.getByText("—")).toHaveClass("muted");
  });

  it("美元口径下只换符号，不换数值（金额已是绝对量）", () => {
    setMoneyCtx({ currency: "USD", scale: 1_000_000, otherCurrency: "CNY", cnyPerUsd: 7.2 });
    render(
      <div>
        <MoneyText micro={1_000_000} />
        <AmountText micro={250_000} />
        <QuotaUnit micro={250_000} />
      </div>,
    );
    expect(screen.getByText("$1")).toBeInTheDocument();
    expect(screen.getByText("0.25")).toBeInTheDocument();
    expect(screen.getByText("$0.25")).toBeInTheDocument();
    // 次行默认关闭，不该出现按汇率折算的第二行金额
    expect(screen.queryByText("¥1.80")).toBeNull();
  });

  it("打开次行后展示另一币种参考金额", () => {
    setMoneyCtx({ currency: "USD", scale: 1_000_000, otherCurrency: "CNY", cnyPerUsd: 7.2 });
    setSecondaryCurrencyEnabled(true);
    render(<QuotaUnit micro={250_000} />);
    expect(screen.getByText("$0.25")).toBeInTheDocument();
    expect(screen.getByText("¥1.80")).toBeInTheDocument();
  });

  it("历史行按行内币种判定符号：美元时代的老记录必须显示 $", () => {
    // 当前是人民币口径，但这条记录产生于美元时代。
    // 若按当前币种选符号，用户会看到 ¥0.25 而不是 $0.25 —— 历史账单被静默换单位。
    setMoneyCtx({ currency: "CNY", scale: 1_000_000, otherCurrency: "USD", cnyPerUsd: 7.2 });
    render(<MoneyText micro={250_000} currency="USD" />);
    expect(screen.getByText("$0.25")).toBeInTheDocument();
  });
});

describe("SiteLogo", () => {
  it("无图片时降级为站名首字，有图片时渲染 img", () => {
    // 品牌来源统一走 useSite()（真实实现要挂 QueryClient）；本用例只验证
    // 「有图渲 img / 无图渲首字」这两个 DOM 分支，兜底链由 test/site.test.ts 覆盖。
    const { unmount } = render(<SiteLogo name="灵动" />);
    expect(screen.getByText("灵")).toHaveClass("dot");
    unmount();
    render(<SiteLogo image="data:image/png;base64,AAAA" />);
    expect(screen.getByAltText("灵动 API")).toHaveClass("logo-img");
  });
});

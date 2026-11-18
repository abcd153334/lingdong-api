import { useState } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { EntityIcon } from "@/components/common/EntityIcon";
import { IconLibrary } from "@/components/common/IconLibrary";
import { IconPicker } from "@/components/common/IconPicker";
import { toast } from "sonner";

/**
 * 图标渲染与选择器的行为契约。
 *
 * 这几条最容易在"看起来对"的时候错：
 *  1. 地址**原样**进 `<img src>`：前端不做任何拼接/改写（规则只在后端 iconref）；
 *  2. 没有地址 → 首字，而不是空方块；
 *  3. 图片加载失败 → 回落首字（坏图会让整张表格看起来"坏了"）；
 *  4. 选中后**必须关面板并把焦点交还砖**：弹层一卸载焦点就掉到 body，
 *     键盘用户下一次 Tab 从整页开头重新走。
 */
const h = vi.hoisted(() => ({
  builtin: [
    {
      key: "openai-gpt",
      name: "OpenAI",
      group: "llm",
      aliases: ["gpt", "开放"],
      url: "https://cdn.test/api/icons/b/openai-gpt?v=abc",
    },
    {
      key: "midjourney",
      name: "Midjourney",
      group: "media",
      aliases: [],
      url: "https://cdn.test/api/icons/b/midjourney?v=abc",
    },
  ],
  uploaded: [] as { id: string; icon: string; url: string; size: number; created_at: number }[],
  // 上传通道的唯一出口：断言"传的是哪个 File"与"回包 token 是否回填"
  upload: vi.fn(),
  isError: false,
  errorMessage: "",
  refetch: vi.fn(),
}));

vi.mock("@/lib/api", () => ({
  useAdminIconsQuery: () => ({
    data: { builtin: h.builtin, uploaded: h.uploaded },
    isLoading: false,
    isError: h.isError,
    error: h.isError ? new Error(h.errorMessage) : null,
    refetch: h.refetch,
  }),
  useUploadIconMutation: () => ({ mutateAsync: h.upload, isPending: false }),
}));

// 上传失败的提示必须走 toast（图库里没有错误位），否则失败是"点了一点反应都没有"
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

beforeEach(() => {
  h.uploaded = [];
  h.upload.mockReset();
  h.isError = false;
  h.errorMessage = "";
  h.refetch.mockReset();
  vi.mocked(toast.error).mockClear();
  vi.mocked(toast.success).mockClear();
});

describe("EntityIcon", () => {
  const URL_BUILTIN = "https://cdn.test/api/icons/b/openai-gpt?v=abc";

  it("地址原样渲染为 img（不做任何前端拼接）", () => {
    render(<EntityIcon url={URL_BUILTIN} name="OpenAI" />);
    const img = document.querySelector("img");
    expect(img).not.toBeNull();
    expect(img!.getAttribute("src")).toBe(URL_BUILTIN);
    // 装饰性图标：alt 为空 + 容器 aria-hidden，读屏应读实体名而不是重复一遍
    expect(img!.getAttribute("alt")).toBe("");
  });

  it("无地址时显示首字占位", () => {
    render(<EntityIcon name="智谱" />);
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByText("智")).toBeInTheDocument();
  });

  it("空串与 undefined 等价（后端对没配图标的实体下发空串）", () => {
    render(<EntityIcon url="" name="智谱" />);
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByText("智")).toBeInTheDocument();
  });

  it("图片加载失败后回落首字", () => {
    render(<EntityIcon url={URL_BUILTIN} name="OpenAI" />);
    const img = document.querySelector("img")!;
    fireEvent.error(img);
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByText("O")).toBeInTheDocument();
  });

  it("品牌色作为占位块底色（CSS 变量），图片模式下不残留", () => {
    const { rerender } = render(<EntityIcon name="kling" color="#F59E0B" />);
    const span = screen.getByText("K");
    expect(span.style.getPropertyValue("--ent-icon-bg")).toBe("#F59E0B");

    rerender(<EntityIcon url={URL_BUILTIN} name="kling" color="#F59E0B" />);
    expect(document.querySelector(".ent-icon--text")).toBeNull();
  });
});

describe("IconPicker", () => {
  const noop = () => {};

  /** 触发器：整块砖是一个按钮，可访问名自带动作（见 IconPicker 注释） */
  const trigger = () => screen.getByRole("button", { name: "更换图标" });

  const URL_INHERITED = "https://cdn.test/api/icons/b/openai-gpt?v=abc";

  it("未设置且有继承值：砖预览继承图标，来源进 title", () => {
    render(
      <IconPicker
        value=""
        onChange={noop}
        fallback="builtin:openai-gpt"
        fallbackUrl={URL_INHERITED}
        name="GPT-4o"
      />,
    );
    // 预览用的是继承值（否则运营看不出"现在实际长什么样"）
    expect(document.querySelector("img")!.getAttribute("src")).toBe(URL_INHERITED);
    // 没有 caption 时来源只在 title 里（名字同一行塞不下小字）
    expect(trigger().getAttribute("title")).toBe("沿用供应商");
    expect(screen.queryByText("沿用供应商")).toBeNull();
  });

  it("caption 为真时来源小字可见", () => {
    render(
      <IconPicker
        value=""
        onChange={noop}
        fallback="builtin:openai-gpt"
        fallbackUrl={URL_INHERITED}
        name="x"
        caption
      />,
    );
    expect(screen.getByText("沿用供应商")).toBeInTheDocument();
  });

  it("未设置且无继承值 → 使用首字", () => {
    render(<IconPicker value="" onChange={noop} name="智谱" caption />);
    expect(screen.getByText("使用首字")).toBeInTheDocument();
  });

  it("已设置时来源显示为自定义图片", () => {
    render(
      <IconPicker value="upload:01j9x8k2m4n6p8r0t2v4w6y8z0" onChange={noop} name="x" caption />,
    );
    expect(screen.getByText("自定义图片")).toBeInTheDocument();
  });

  it("面板默认收起，点砖展开图库；再点收起", () => {
    render(<IconPicker value="" onChange={noop} name="x" />);
    expect(screen.queryByPlaceholderText("搜索图标")).toBeNull();
    fireEvent.click(trigger());
    expect(screen.getByPlaceholderText("搜索图标")).toBeInTheDocument();
    expect(screen.getByText("大模型厂商")).toBeInTheDocument();
    expect(screen.getByText("图像 / 视频 / 音频")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "收起图标库" }));
    expect(screen.queryByPlaceholderText("搜索图标")).toBeNull();
  });

  it("Esc 关面板并把焦点交回砖（不能冒泡去关掉外层弹窗）", () => {
    render(<IconPicker value="" onChange={noop} name="x" />);
    fireEvent.click(trigger());
    const search = screen.getByPlaceholderText("搜索图标");
    const stop = vi.fn();
    // 模拟外层 Dialog 的 document 级监听：Esc 必须在弹层里被吃掉
    document.addEventListener("keydown", stop);
    fireEvent.keyDown(search, { key: "Escape" });
    document.removeEventListener("keydown", stop);

    expect(screen.queryByPlaceholderText("搜索图标")).toBeNull();
    expect(stop).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(trigger());
  });

  it("点选图标：回传 builtin token、立刻有预览、关面板、焦点回砖", () => {
    const onChange = vi.fn();
    // 受控宿主：真实表单会在 onChange 里回写 value，预览必须靠这条链路活下来
    function Harness(): React.ReactElement {
      const [v, setV] = useState("");
      return (
        <IconPicker
          value={v}
          onChange={(t) => {
            onChange(t);
            setV(t);
          }}
          name="x"
        />
      );
    }
    render(<Harness />);
    fireEvent.click(trigger());
    fireEvent.click(screen.getByRole("button", { name: /OpenAI/ }));

    // 表单只收令牌（地址是服务端的事）；地址由选择器自己记下来做预览
    expect(onChange).toHaveBeenCalledWith("builtin:openai-gpt");
    // 「刚选中、还没保存」这一帧必须立刻有图：该令牌还没经过任何实体的读投影
    expect(document.querySelector("img")!.getAttribute("src")).toBe(
      "https://cdn.test/api/icons/b/openai-gpt?v=abc",
    );
    expect(screen.queryByPlaceholderText("搜索图标")).toBeNull();
    expect(document.activeElement).toBe(trigger());
  });

  it("「使用首字」提交空串（清除只能走这里，不再单独放一个清除按钮）", () => {
    const onChange = vi.fn();
    render(<IconPicker value="builtin:openai-gpt" onChange={onChange} name="x" />);
    fireEvent.click(trigger());
    fireEvent.click(screen.getByRole("button", { name: "使用首字" }));
    expect(onChange).toHaveBeenCalledWith("");
  });
});

describe("IconLibrary", () => {
  const noop = () => {};

  it("搜索按 key / 名称 / 别名过滤", () => {
    render(<IconLibrary value="" onPick={noop} onClear={noop} />);
    fireEvent.change(screen.getByPlaceholderText("搜索图标"), { target: { value: "开放" } });
    expect(screen.getByText("OpenAI")).toBeInTheDocument();
    expect(screen.queryByText("Midjourney")).toBeNull();
  });

  it("搜索无结果才提示无匹配（空搜索词下列表本来就是全量）", () => {
    render(<IconLibrary value="" onPick={noop} onClear={noop} />);
    expect(screen.queryByText("没有匹配的图标")).toBeNull();
    fireEvent.change(screen.getByPlaceholderText("搜索图标"), { target: { value: "zzz" } });
    expect(screen.getByText("没有匹配的图标")).toBeInTheDocument();
  });

  it("方向键在网格内移动焦点（固定 4 列）", () => {
    render(<IconLibrary value="" onPick={noop} onClear={noop} />);
    const search = screen.getByPlaceholderText("搜索图标");
    fireEvent.keyDown(search, { key: "ArrowDown" });
    const cells = screen.getAllByRole("button", { pressed: false });
    // 第一格（OpenAI）拿到焦点
    expect(document.activeElement).toBe(cells[0]);
    fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
    expect(document.activeElement).toBe(cells[1]);
    // 首行向上回到搜索框（闭环，不会卡在网格里出不去）
    fireEvent.keyDown(document.activeElement!, { key: "ArrowUp" });
    expect(document.activeElement).toBe(search);
  });

  it("已上传分组用上传日期区分（四张一模一样的「自定义图片」读屏分不清）", () => {
    h.uploaded = [
      {
        id: "0123456789abcdefghjkmnpqrs",
        icon: "upload:0123456789abcdefghjkmnpqrs",
        url: "https://cdn.test/api/icons/u/0123456789abcdefghjkmnpqrs",
        size: 3,
        created_at: 1700000000,
      },
    ];
    render(<IconLibrary value="" onPick={noop} onClear={noop} />);
    expect(screen.getByText("已上传")).toBeInTheDocument();
    // 只显示 MM-DD 两段，完整时间进 title
    const cell = screen.getByRole("button", { name: /11-1/ });
    expect(cell.getAttribute("title")).toMatch(/^自定义图片 · \d{4}-\d{2}-\d{2}/);
  });

  it("上传：文件过接口后回填 upload token（不自己拼 data URI）", async () => {
    const id = "0123456789abcdefghjkmnpqrs";
    h.upload.mockResolvedValueOnce({
      id,
      icon: `upload:${id}`,
      url: `https://cdn.test/api/icons/u/${id}`,
      mime: "image/png",
      size: 3,
      created_at: 1700000000,
    });
    const onPick = vi.fn();
    render(<IconLibrary value="" onPick={onPick} onClear={noop} />);

    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    const file = new File([new Uint8Array([1, 2, 3])], "a.png", { type: "image/png" });
    fireEvent.change(input, { target: { files: [file] } });

    await waitFor(() =>
      expect(onPick).toHaveBeenCalledWith(`upload:${id}`, `https://cdn.test/api/icons/u/${id}`),
    );
    // 传上去的必须是原 File（后端靠内容嗅探类型，改包一层 multipart 细节会丢文件名）
    expect(h.upload).toHaveBeenCalledWith(file);
    expect(toast.success).toHaveBeenCalledWith("图标已上传");
  });

  it("上传失败：把后端原话显示出来，且不改动当前图标", async () => {
    // 类型/体积判断只在后端一份（前端预检迟早和后端给出不同答案）
    h.upload.mockRejectedValueOnce(new Error("图标不能超过 512KB"));
    const onPick = vi.fn();
    render(<IconLibrary value="builtin:openai-gpt" onPick={onPick} onClear={noop} />);

    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [new File(["x"], "big.png", { type: "image/png" })] } });

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("图标不能超过 512KB"));
    expect(onPick).not.toHaveBeenCalled();
  });

  it("加载失败：显示后端原话与「重试」，不是笼统的「加载失败」", () => {
    h.isError = true;
    h.errorMessage = "存储服务暂时不可用";
    render(<IconLibrary value="" onPick={noop} onClear={noop} />);
    expect(screen.getByText("存储服务暂时不可用")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(h.refetch).toHaveBeenCalled();
  });
});

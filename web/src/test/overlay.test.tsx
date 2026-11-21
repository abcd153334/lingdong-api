import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Dialog, Sheet } from "@/components/common/Dialog";
import { Drawer } from "@/components/common/Drawer";
import { ConfirmDialogHost, confirmDialog } from "@/components/common/ConfirmDialog";

describe("Dialog / Sheet", () => {
  it("打开态渲染标题/内容/底部动作，关闭回调由遮罩触发", async () => {
    const onClose = vi.fn();
    const user = userEvent.setup();
    const { container } = render(
      <Dialog
        open
        onClose={onClose}
        title="对话框标题"
        footer={<button type="button">确认</button>}
      >
        内容体
      </Dialog>,
    );
    expect(screen.getByRole("dialog")).toHaveClass("open");
    expect(screen.getByText("对话框标题")).toBeInTheDocument();
    expect(screen.getByText("内容体")).toBeInTheDocument();
    await user.click(container.querySelector(".mask") as HTMLElement);
    expect(onClose).toHaveBeenCalled();
  });

  it("headExtra 落在标题与关闭之间，且不传时顶栏结构不变", () => {
    const plain = render(
      <Dialog open onClose={() => {}} title="无附加" closeButton>
        内容体
      </Dialog>,
    );
    // 旧结构：只有标题 + 关闭，没有附加容器
    expect(plain.container.querySelector(".dl-head__extra")).toBeNull();
    expect(
      Array.from(plain.container.querySelectorAll(".dl-head > *")).map((el) => el.className),
    ).toEqual(["dl-title", "icon-btn dl-close"]);
    plain.unmount();

    const withExtra = render(
      <Dialog
        open
        onClose={() => {}}
        title="带页签"
        closeButton
        headExtra={<div role="tablist" aria-label="页签" />}
      >
        内容体
      </Dialog>,
    );
    const head = withExtra.container.querySelector(".dl-head") as HTMLElement;
    // 顺序必须是 标题 → 附加 → 关闭：页签在关闭按钮左边，键盘顺序也才合理
    expect(Array.from(head.children).map((el) => el.className)).toEqual([
      "dl-title",
      "dl-head__extra",
      "icon-btn dl-close",
    ]);
    expect(screen.getByRole("tablist")).toBeInTheDocument();
  });

  it("actions 按顺序渲染为页脚按钮", async () => {
    const onOk = vi.fn();
    const user = userEvent.setup();
    const { container } = render(
      <Dialog
        open
        onClose={() => {}}
        title="动作"
        actions={[
          { label: "取消" },
          { label: "保存", variant: "primary", onClick: onOk },
        ]}
      >
        内容体
      </Dialog>,
    );
    const btns = Array.from(container.querySelectorAll(".dl-foot button")) as HTMLElement[];
    expect(btns.map((b) => b.textContent)).toEqual(["取消", "保存"]);
    expect(btns[1]).toHaveClass("btn-primary");
    await user.click(btns[1]);
    expect(onOk).toHaveBeenCalled();
  });

  it("Esc 关闭，Tab 焦点在弹窗内闭合", () => {
    const onClose = vi.fn();
    const { container } = render(
      <Dialog
        open
        onClose={onClose}
        title="键盘"
        actions={[{ label: "取消" }, { label: "确认", variant: "primary" }]}
      >
        内容体
      </Dialog>,
    );
    const btns = Array.from(container.querySelectorAll(".dl-foot button")) as HTMLElement[];
    // 初始聚焦首个可聚焦元素
    expect(btns[0]).toHaveFocus();
    // 末位 Tab → 回到首位
    btns[1].focus();
    fireEvent.keyDown(document, { key: "Tab" });
    expect(btns[0]).toHaveFocus();
    // 首位 Shift+Tab → 回到末位
    fireEvent.keyDown(document, { key: "Tab", shiftKey: true });
    expect(btns[1]).toHaveFocus();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).toHaveBeenCalled();
  });

  it("关闭后焦点归还触发器", () => {
    const trigger = document.createElement("button");
    trigger.textContent = "触发";
    document.body.appendChild(trigger);
    trigger.focus();

    const { rerender } = render(
      <Dialog open onClose={() => {}} title="焦点">
        内容体
      </Dialog>,
    );
    rerender(
      <Dialog open={false} onClose={() => {}} title="焦点">
        内容体
      </Dialog>,
    );
    expect(trigger).toHaveFocus();
    trigger.remove();
  });

  it("Sheet 复用 Dialog 契约并带 filter-sheet 语义类", () => {
    const { container } = render(
      <Sheet open onClose={() => {}} title="筛选" className="filter-sheet">
        <div className="pfield">字段</div>
      </Sheet>,
    );
    expect(container.querySelector(".dialog.filter-sheet.open")).toBeTruthy();
  });
});

describe("Drawer", () => {
  it("打开态使用 .drawer.open 并为关闭按钮提供 aria-label", async () => {
    const onClose = vi.fn();
    const user = userEvent.setup();
    const { container } = render(
      <Drawer open onClose={onClose} title="详情抽屉">
        抽屉内容
      </Drawer>,
    );
    expect(container.querySelector(".drawer.open")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "关闭" }));
    expect(onClose).toHaveBeenCalled();
  });
});

describe("confirmDialog", () => {
  it("确认返回 true、取消返回 false", async () => {
    const user = userEvent.setup();
    render(<ConfirmDialogHost />);

    const p1 = confirmDialog({ title: "删除渠道", body: "确认删除？" });
    expect(await screen.findByText("删除渠道")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "确认" }));
    await expect(p1).resolves.toBe(true);

    const p2 = confirmDialog({ title: "重置额度", cancelText: "再想想" });
    await user.click(await screen.findByRole("button", { name: "再想想" }));
    await expect(p2).resolves.toBe(false);
  });

  it("重复调用时前一个以取消结算（无悬挂 Promise）", async () => {
    const user = userEvent.setup();
    render(<ConfirmDialogHost />);

    const first = confirmDialog({ title: "第一问" });
    const second = confirmDialog({ title: "第二问" });
    await expect(first).resolves.toBe(false);
    expect(await screen.findByText("第二问")).toBeInTheDocument();
    expect(screen.queryByText("第一问")).toBeNull();
    await user.click(screen.getByRole("button", { name: "确认" }));
    await expect(second).resolves.toBe(true);
  });

  it("Escape 视为取消", async () => {
    const user = userEvent.setup();
    render(<ConfirmDialogHost />);
    const p = confirmDialog({ title: "Escape 关闭" });
    await screen.findByText("Escape 关闭");
    await user.keyboard("{Escape}");
    await expect(p).resolves.toBe(false);
  });
});

describe("弹层栈", () => {
  /**
   * 两层同时打开时（账户弹窗里再开「修改密码」抽屉），两次 `useOverlayBehavior`
   * 都在 document 上监听 keydown。没有栈的话一次 Esc 会把两层一起关掉，
   * 而且两个 focus trap 会互相把焦点拽回去。
   */
  it("Esc 只关最上层，不连带关掉下层", () => {
    const closeOuter = vi.fn();
    const closeInner = vi.fn();
    const tree = (innerOpen: boolean) => (
      <>
        <Dialog open onClose={closeOuter} title="外层">
          <button type="button">外层动作</button>
        </Dialog>
        <Dialog open={innerOpen} onClose={closeInner} title="内层">
          <button type="button">内层动作</button>
        </Dialog>
      </>
    );

    const { rerender } = render(tree(false));
    rerender(tree(true));

    fireEvent.keyDown(document, { key: "Escape" });
    expect(closeInner).toHaveBeenCalledTimes(1);
    expect(closeOuter).not.toHaveBeenCalled();

    // 上层关闭后，Esc 重新轮到下层。
    rerender(tree(false));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(closeOuter).toHaveBeenCalledTimes(1);
  });
});

import { useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { cn } from "@/lib/utils";
import { Icon } from "./Icon";
import { OverlayOpenContext } from "./overlayContext";
import { btnClass, type BtnVariant } from "./PageHead";
import { useOverlayBehavior } from "./useOverlayBehavior";

/** 弹窗页脚动作（统一「取消 + 主按钮」写法，替代页面层手写页脚） */
export interface DialogAction {
  label: string;
  variant?: BtnVariant;
  onClick?: () => void;
  disabled?: boolean;
}

export interface DialogProps {
  open: boolean;
  onClose: () => void;
  title?: ReactNode;
  /**
   * 对话框的可访问名（`aria-label` 的唯一来源优先级最高）。
   *
   * 只在**顶栏不显示标题**时用：没有可见标题的弹窗仍是对话，必须有个稳定的名字，
   * 否则读屏与 `getByRole("dialog", { name })` 一律退化成「对话框」。
   * 不传则沿用 `title` 字符串（旧行为逐字不变）。
   */
  ariaLabel?: string;
  /**
   * 顶栏附加内容（渲染在标题与关闭按钮**之间**）。
   *
   * 用途：把导航放进顶栏而不是单独占一行（`/admin/docs` 的页签、应用管理弹窗的页签）。
   * 不传 = 顶栏只有「标题 + 关闭」，与旧行为逐字一致。
   *
   * 注意宽度分配：本节点是 `flex:1 1 auto; min-width:0`，里面的东西必须自己能收缩
   * 或滚动 —— 否则会顶破 `.dialog` 的 `max-width`（`.dialog` 没有 `overflow`）。
   */
  headExtra?: ReactNode;
  children: ReactNode;
  /** 页脚自定义节点（复杂页脚：含合计/多动作说明等；标准按钮对请用 actions） */
  footer?: ReactNode;
  /** 页脚动作（按数组顺序右对齐渲染） */
  actions?: DialogAction[];
  /** md（默认 440px）/ lg（500px）/ wide（560px）/ xl（760px，主从两栏用） */
  size?: "md" | "lg" | "wide" | "xl";
  className?: string;
  /**
   * 右上角关闭按钮（默认不渲染）。
   *
   * 为什么默认关：现有弹窗都能靠遮罩与 Esc 关闭，凭空多一个「关闭」按钮会让
   * `getByRole("button", { name: "关闭" })` 在某些页面上命中两个。需要显式关闭入口的
   * 长弹窗（账户设置）单独打开。
   */
  closeButton?: boolean;
  /**
   * 挂到 `document.body`（默认 false）。
   *
   * 必要场景：弹窗里再开弹窗。`.dialog` 带 `transform`（居中），它是 `position:fixed`
   * 后代的**包含块** —— 内层弹窗会被定位到外层弹窗的盒子里，并被 `.dl-body` 的
   * `overflow-y:auto` 裁掉。升到 body 才能相对视口定位。
   */
  portal?: boolean;
  /**
   * 层级：1（默认）或 2（弹窗里的弹窗）。
   *
   * 只影响遮罩与对话框的 z-index（65/70），不改任何布局。第一层是 50/60。
   */
  layer?: 1 | 2;
}

/** 对话框（桌面居中；≤767px 由 components.css 转为底部 Sheet） */
export function Dialog({
  open,
  onClose,
  title,
  ariaLabel,
  headExtra,
  children,
  footer,
  actions,
  size = "md",
  className,
  closeButton,
  portal = false,
  layer = 1,
}: DialogProps) {
  const ref = useRef<HTMLDivElement>(null);
  useOverlayBehavior(open, onClose, ref);

  const foot = actions?.length ? (
    <>
      {actions.map((a) => (
        <button
          key={a.label}
          type="button"
          className={btnClass(a.variant ?? "default")}
          disabled={a.disabled}
          onClick={a.onClick}
        >
          {a.label}
        </button>
      ))}
    </>
  ) : (
    footer
  );

  const node = (
    <>
      <div
        className={cn("mask", layer === 2 && "mask--l2", open ? "open" : "hidden")}
        onClick={onClose}
      />
      <div
        ref={ref}
        className={cn(
          "dialog",
          size === "lg" && "dialog--lg",
          size === "wide" && "dialog--wide",
          size === "xl" && "dialog--xl",
          layer === 2 && "dl--l2",
          open && "open",
          className,
        )}
        role="dialog"
        aria-modal="true"
        aria-label={ariaLabel ?? (typeof title === "string" ? title : "对话框")}
      >
        {title || headExtra ? (
          <div className="dl-head">
            {title ? <span className="dl-title">{title}</span> : null}
            {headExtra ? <div className="dl-head__extra">{headExtra}</div> : null}
            {closeButton ? (
              <button
                type="button"
                className="icon-btn dl-close"
                aria-label="关闭"
                onClick={onClose}
              >
                <Icon name="close" size={16} />
              </button>
            ) : null}
          </div>
        ) : null}
        <div className="dl-body">{children}</div>
        {foot ? <div className="dl-foot">{foot}</div> : null}
      </div>
    </>
  );

  return (
    <OverlayOpenContext.Provider value={open}>
      {portal && typeof document !== "undefined" ? createPortal(node, document.body) : node}
    </OverlayOpenContext.Provider>
  );
}

export interface SheetProps extends Omit<DialogProps, "size"> {}

/** 底部表单 Sheet（移动端筛选/快捷表单；复用 Dialog 契约） */
export function Sheet({ className, ...rest }: SheetProps) {
  return <Dialog {...rest} className={cn("sheet", className)} />;
}

import { useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { cn } from "@/lib/utils";
import { EntityIcon } from "./EntityIcon";
import { Icon } from "./Icon";
import { OverlayOpenContext } from "./overlayContext";
import { btnClass } from "./PageHead";
import { useOverlayBehavior } from "./useOverlayBehavior";
import type { DialogAction } from "./Dialog";

export interface DrawerProps {
  open: boolean;
  onClose: () => void;
  title?: ReactNode;
  /**
   * 标题前的实体图标（模型 / 渠道等）。
   *
   * 刻意做成独立参数而不是让调用方把 `EntityIcon` 塞进 `title`：`aria-label` 取的是
   * `title` 是字符串时的值，一旦传 ReactNode 整条抽屉的可访问名就退化成"详情抽屉"
   * （e2e 与读屏都靠这个名字定位）。`name` 只用于"无图标时的首字"，不进标题文本。
   */
  titleIcon?: { url?: string; color?: string; name: string };
  children: ReactNode;
  /** 页脚自定义节点（复杂页脚；标准按钮对请用 actions） */
  footer?: ReactNode;
  /** 页脚动作（按数组顺序右对齐渲染） */
  actions?: DialogAction[];
  className?: string;
  /**
   * 挂到 `document.body`。
   *
   * 只有一种情况需要：抽屉被渲染在 `<Dialog>` / 另一个 `<Drawer>` 内部（例如账户弹窗里的
   * 「退出全部设备」确认）。`.dialog` 桌面有 `transform`、移动端有 `translate`，两者都会给
   * `position:fixed` 后代建立包含块，且 `.dl-body` 是 `overflow-y:auto` —— 不 portal 的话
   * 抽屉会被钉在弹窗盒子里并被裁掉。默认 false：普通页面上的抽屉留在原位，避免无谓改动。
   */
  portal?: boolean;
}

/** 抽屉（桌面右侧；≤767px 由 components.css 转为底部 Sheet） */
export function Drawer({
  open,
  onClose,
  title,
  titleIcon,
  children,
  footer,
  actions,
  className,
  portal,
}: DrawerProps) {
  const ref = useRef<HTMLElement>(null);
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
      <div className={cn("mask", open ? "open" : "hidden")} onClick={onClose} />
      <aside
        ref={ref}
        className={cn("drawer", open && "open", className)}
        role="dialog"
        aria-modal="true"
        aria-label={typeof title === "string" ? title : "详情抽屉"}
      >
        <div className="dr-head">
          <span className="dr-title">
            {titleIcon ? (
              <EntityIcon
                url={titleIcon.url}
                name={titleIcon.name}
                color={titleIcon.color}
                size="sm"
              />
            ) : null}
            <span className="dr-title-t">{title}</span>
          </span>
          <button type="button" className="icon-btn" aria-label="关闭" onClick={onClose}>
            <Icon name="close" size={18} />
          </button>
        </div>
        <div className="dr-body">{children}</div>
        {foot ? <div className="dr-foot">{foot}</div> : null}
      </aside>
    </>
  );

  return (
    <OverlayOpenContext.Provider value={open}>
      {portal && typeof document !== "undefined" ? createPortal(node, document.body) : node}
    </OverlayOpenContext.Provider>
  );
}

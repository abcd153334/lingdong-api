import { useRef, useState } from "react";

import { Icon } from "@/components/common/Icon";
import { Popover } from "@/components/common/Popover";
import { Tag } from "@/components/common/StatusTag";

/**
 * 右栏顶栏：**一行**。左「是什么」，右「能对它做什么」。
 *
 * 四条决定：
 *  1. 页签（编辑/预览）由调用方渲染后作为 `tabs` 传进来，落在右簇第一位 ——
 *     它切的是"这块内容的看法"，与动作同属"操作这块内容"，所以同簇、
 *     用一根竖线隔开，但不另起一行（另起一行 = 常驻 56px，白吃屏高）。
 *  2. **只有一个主按钮**。发布是主色；保存是常规按钮（保存不等于上线）。
 *  3. 有未保存改动时，发布按钮**禁用**并说明原因（先保存）：允许直接发布会让人
 *     以为屏幕上的草稿已经生效，而实际发出去的是库里旧内容。`未保存` 紧挨保存。
 *  4. 「更多」里只有两个低频动作（打开线上 / 删除），用锚定弹层而不是常驻按钮。
 */
export function TopBar({
  icon,
  title,
  meta,
  status,
  dirty,
  busy,
  published,
  canPreview,
  tabs,
  railButton,
  onSave,
  onToggleStatus,
  onOpenPublic,
  onDelete,
}: {
  icon: "grid" | "doc" | "rows";
  title: string;
  /** 方法 + 路径 / 板块英文名之类的身份行 */
  meta: string;
  status: "draft" | "published";
  dirty: boolean;
  busy: boolean;
  published: boolean;
  canPreview: boolean;
  /** 页签节点（`<Tabs variant="pill">`）：渲染在右簇第一位 */
  tabs?: React.ReactNode;
  /** ≤1023 的抽屉开关：渲染在身份块最前 */
  railButton?: React.ReactNode;
  onSave: () => void;
  onToggleStatus: () => void;
  onOpenPublic?: () => void;
  onDelete: () => void;
}) {
  const moreRef = useRef<HTMLButtonElement | null>(null);
  const [moreOpen, setMoreOpen] = useState(false);

  return (
    <header className="adp-top">
      <div className="adp-top__id">
        {railButton}
        <span className="adp-top__ico">
          <Icon name={icon} size={16} />
        </span>
        <div className="adp-top__txt">
          <div className="adp-top__title">{title || "未命名"}</div>
          {meta ? <div className="adp-top__meta">{meta}</div> : null}
        </div>
        <Tag tone={status === "published" ? "success" : "warning"}>
          {status === "published" ? "已发布" : "草稿"}
        </Tag>
      </div>

      <div className="adp-top__end">
        {tabs ? <div className="adp-top__tabs">{tabs}</div> : null}
        {tabs ? <span className="adp-top__sep" aria-hidden="true" /> : null}
        <div className="adp-top__acts">
          {dirty ? <span className="adp-dirty">未保存</span> : null}
          <button
            type="button"
            className="btn btn-sm"
            aria-label="保存"
            title="保存这一篇"
            disabled={!dirty || busy}
            onClick={onSave}
          >
            <Icon name="check" size={14} />
            <span className="btn__txt">保存</span>
          </button>
          <button
            type="button"
            className={published ? "btn btn-sm" : "btn btn-primary btn-sm"}
            aria-label={published ? "转草稿" : "发布"}
            disabled={busy || dirty}
            onClick={onToggleStatus}
            title={dirty ? "先保存再发布：发布的是库里的内容" : published ? "下线这篇（读者立刻看不到）" : "发布上线"}
          >
            <Icon name={published ? "eyeOff" : "upload"} size={14} />
            <span className="btn__txt">{published ? "转草稿" : "发布"}</span>
          </button>
          <button
            ref={moreRef}
            type="button"
            className="icon-btn"
            aria-label="更多操作"
            aria-expanded={moreOpen}
            onClick={() => setMoreOpen((v) => !v)}
          >
            <Icon name="more" size={16} />
          </button>
          <Popover
            open={moreOpen}
            onClose={() => setMoreOpen(false)}
            anchorRef={moreRef}
            className="adp-menu"
            ariaLabel="更多操作"
            prefer="below"
            gap={6}
          >
            <button
              type="button"
              className="adp-menu__item"
              disabled={!canPreview || !onOpenPublic}
              onClick={() => {
                setMoreOpen(false);
                onOpenPublic?.();
              }}
            >
              <Icon name="external" size={14} />
              打开线上页面
            </button>
            <button
              type="button"
              className="adp-menu__item adp-danger"
              onClick={() => {
                setMoreOpen(false);
                onDelete();
              }}
            >
              <Icon name="trash" size={14} />
              删除
            </button>
          </Popover>
        </div>
      </div>
    </header>
  );
}

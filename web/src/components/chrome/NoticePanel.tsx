import { useEffect, useRef, useState, type ReactNode } from "react";

import { EmptyState } from "@/components/common/EmptyState";
import { Icon } from "@/components/common/Icon";
import { ErrorState, LoadingState } from "@/components/common/QueryStates";
import { useAnnouncementsQuery, useMarkAnnouncementsReadMutation } from "@/lib/api";
import { renderInline, renderMarkdown } from "@/lib/markdown";
import { noticeTime, typeMeta, unreadOf } from "@/lib/notice";
import { cn } from "@/lib/utils";
import { useAuthStore } from "@/stores/auth";
import type { PortalAnnouncementItem } from "@/types";

/**
 * 单条公告（手风琴：收起=两行摘要，展开=完整 Markdown，摘要隐藏避免正文重复）。
 *
 * 用真 `<button>` 而不是 `div[role=button]` + 手写 Enter/Space：
 * 键盘语义交给浏览器，少一处会写漏的分支。
 */
function NoticeRow({
  item,
  index,
  highlight,
}: {
  item: PortalAnnouncementItem;
  index: number;
  /** 本次打开弹层前就存在的未读条目（打开瞬间的快照，纯 UI 状态） */
  highlight: boolean;
}) {
  const [open, setOpen] = useState(false);
  const meta = typeMeta(item.type);
  const bodyId = `notice-body-${item.id || `idx-${index}`}`;

  return (
    <article className={cn("notice-item", open && "open", highlight && "is-new")}>
      <button
        type="button"
        className="notice-head"
        aria-expanded={open}
        aria-controls={bodyId}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="notice-dot" style={{ background: meta.color }} />
        <span className="notice-main">
          <span
            className="notice-tx"
            dangerouslySetInnerHTML={{ __html: renderInline(item.content) }}
          />
          <span className="notice-meta">
            <span className="nt-tag" style={{ color: meta.color, borderColor: meta.color }}>
              {meta.label}
            </span>
            <time>{noticeTime(item.publishDate)}</time>
          </span>
        </span>
        <Icon name="chevronRight" size={16} className="notice-caret" />
      </button>

      {open ? (
        <div className="notice-body" id={bodyId}>
          <div
            className="nt-md"
            dangerouslySetInnerHTML={{ __html: renderMarkdown(item.content) }}
          />
          {item.extra ? (
            <div className="notice-extra">
              <b>补充说明</b>
              <div
                className="nt-md"
                dangerouslySetInnerHTML={{ __html: renderMarkdown(item.extra) }}
              />
            </div>
          ) : null}
        </div>
      ) : null}
    </article>
  );
}

/**
 * 公告弹层内容 —— **门户与管理台唯一的公告展示实现**（两端顶栏铃铛共用）。
 *
 * 为什么不是「一个整页 + 一个弹层」：两处都要维护加载/错误/空态、「展开态摘要隐藏」、
 * 「补充说明」与已读上报，任何一处改动都会漏掉另一边（历史上管理台整页就是照抄门户页
 * 再慢慢分叉的）。现在只有一个 `NoticeBell`，形态自然统一。
 *
 * 三条口径：
 *  1. 未读由 `items[].read` 派生，接口不再下发 `unread`；
 *  2. 打开即整组已读：挂载后最多提交一次，成功后用响应整份替换缓存；
 *  3. 「新」标记来自**打开瞬间的未读快照** —— 若直接读 cache，标记会在
 *     已读请求成功那一帧消失，用户根本看不到哪几条是新的。
 *     快照只活在本次打开期间，不落存储、不参与未读计算。
 */
export function NoticePanel({ onClose }: { onClose: () => void }) {
  const loggedIn = useAuthStore((s) => s.user !== null);
  const query = useAnnouncementsQuery(loggedIn);
  const markRead = useMarkAnnouncementsReadMutation();
  // mutate 的引用每次渲染都变，塞进 ref 才不会让 effect 反复触发
  const mutateRef = useRef(markRead.mutate);
  mutateRef.current = markRead.mutate;
  const sentRef = useRef(false);
  const snapshotRef = useRef<number[] | null>(null);

  const data = query.data;
  // 首次拿到数据时定格「哪些是未读」，之后（已读成功后）不再变
  if (snapshotRef.current === null && data) {
    snapshotRef.current = data.items.filter((item) => !item.read).map((item) => item.id);
  }

  useEffect(() => {
    if (sentRef.current || !data) return;
    if (!data.items.some((item) => !item.read)) return;
    sentRef.current = true;
    mutateRef.current();
  }, [data]);

  const items = data?.enabled ? data.items : [];
  const unread = unreadOf(items);
  const sub = !data ? "" : unread > 0 ? `${unread} 条未读 · 共 ${items.length} 条` : `共 ${items.length} 条`;
  const marks =
    snapshotRef.current && snapshotRef.current.length > 0 ? new Set(snapshotRef.current) : null;

  let body: ReactNode;
  // 只有「没有数据可显示」时才整块报错：已有内容时刷新失败，显示旧列表比换成一堵
  // 错误墙有用（弹层是顺手看一眼，不是账目核对）。
  if (query.isError && !data) {
    body = (
      <ErrorState
        error={query.error}
        title="公告加载失败"
        onRetry={() => {
          void query.refetch();
        }}
      />
    );
  } else if (!data) {
    body = <LoadingState />;
  } else if (items.length === 0) {
    // 未启用与「已启用但没有公告」共用一句文案：不向用户暴露「管理员没开」这种内情
    body = <EmptyState variant="empty" title="暂无公告" desc="有新的站点通知时会显示在这里" />;
  } else {
    body = (
      <div className="notice-list">
        {items.map((item, index) => (
          <NoticeRow
            key={item.id || `idx-${index}`}
            item={item}
            index={index}
            highlight={!!marks?.has(item.id)}
          />
        ))}
      </div>
    );
  }

  return (
    <>
      <div className="ntp-head">
        <span className="ntp-ico" aria-hidden="true">
          <Icon name="bell" size={18} />
        </span>
        <span className="ntp-tt">
          <b>站点公告</b>
          {sub ? <span className="ntp-sub">{sub}</span> : null}
        </span>
        <button type="button" className="icon-btn" aria-label="关闭公告" onClick={onClose}>
          <Icon name="close" size={16} />
        </button>
      </div>
      <div className="ntp-list">{body}</div>
    </>
  );
}

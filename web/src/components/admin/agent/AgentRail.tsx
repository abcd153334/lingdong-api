import { useMemo, useRef, useState } from "react";
import dayjs from "dayjs";
import { Icon } from "@/components/common/Icon";
import { EmptyState } from "@/components/common/EmptyState";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { ApiError } from "@/lib/apiClient";
import type { AgentSession } from "@/lib/api/agent";
import { cn } from "@/lib/utils";

export interface AgentRailProps {
  sessions: AgentSession[];
  activeId: number;
  loading: boolean;
  error: unknown;
  creating: boolean;
  /** 今日已用估算额度 */
  todaySpend?: number;
  /** 每日上限（0 = 不限） */
  budget?: number;
  onCreate: () => void;
  onSelect: (id: number) => void;
  onDelete: (id: number) => void;
  onRename: (id: number, title: string) => void;
  onRetry: () => void;
  onOpenSettings: () => void;
  /** ≤899px 时抽屉是否展开 */
  open: boolean;
  onClose: () => void;
}

/** 会话按时间分组（今天 / 昨天 / 近 7 天 / 更早）。 */
function groupOf(iso: string): string {
  if (!iso) return "更早";
  const d = dayjs(iso);
  const today = dayjs().startOf("day");
  if (d.isAfter(today.subtract(1, "second"))) return "今天";
  if (d.isAfter(today.subtract(1, "day"))) return "昨天";
  if (d.isAfter(today.subtract(7, "day"))) return "近 7 天";
  return "更早";
}

const GROUP_ORDER = ["今天", "昨天", "近 7 天", "更早"];

/**
 * 左侧会话栏。
 *
 * 聊天界面的侧栏就是导航：标题必须一眼能分辨、操作必须就地完成（重命名/删除
 * 不跳页），否则运营会退化成"只用一个会话，所有需求挤在一起"。
 */
export function AgentRail({
  sessions,
  activeId,
  loading,
  error,
  creating,
  todaySpend,
  budget,
  onCreate,
  onSelect,
  onDelete,
  onRename,
  onRetry,
  onOpenSettings,
  open,
  onClose,
}: AgentRailProps) {
  const [q, setQ] = useState("");
  const [renaming, setRenaming] = useState(0);
  const [draft, setDraft] = useState("");
  const inputRef = useRef<HTMLInputElement | null>(null);

  const groups = useMemo(() => {
    const kw = q.trim().toLowerCase();
    const hit = kw
      ? sessions.filter(
          (s) =>
            (s.title || "").toLowerCase().includes(kw) ||
            String(s.id).includes(kw),
        )
      : sessions;
    const map = new Map<string, AgentSession[]>();
    for (const s of hit) {
      const g = groupOf(s.updated_at);
      const arr = map.get(g) ?? [];
      arr.push(s);
      map.set(g, arr);
    }
    return GROUP_ORDER.filter((g) => map.has(g)).map((g) => ({ name: g, items: map.get(g)! }));
  }, [q, sessions]);

  const startRename = (s: AgentSession) => {
    setRenaming(s.id);
    setDraft(s.title || "");
    window.setTimeout(() => inputRef.current?.select(), 0);
  };

  const commitRename = (id: number) => {
    const t = draft.trim();
    setRenaming(0);
    if (t && t !== sessions.find((s) => s.id === id)?.title) onRename(id, t);
  };

  const askDelete = async (s: AgentSession) => {
    const ok = await confirmDialog({
      title: "删除这个会话？",
      body: `「${s.title || "新对话"}」的对话与提案记录会一并删除，已生效的配置不受影响。`,
      okText: "删除",
      okVariant: "danger",
    });
    if (ok) onDelete(s.id);
  };

  const spent = todaySpend ?? 0;
  const ratio = budget && budget > 0 ? Math.min(100, Math.round((spent / budget) * 100)) : 0;

  return (
    <aside className="agent-rail" data-open={open ? "1" : undefined} aria-label="会话列表">
      <div className="agent-rail__head">
        <button
          type="button"
          className="agent-new"
          onClick={onCreate}
          disabled={creating}
          data-testid="agent-new-session"
        >
          <Icon name="message" size={16} />
          <span>{creating ? "创建中…" : "新对话"}</span>
        </button>
        <button
          type="button"
          className="agent-rail__close icon-btn"
          aria-label="收起会话列表"
          onClick={onClose}
        >
          <Icon name="close" size={16} />
        </button>
      </div>

      <label className="agent-rail__search">
        <Icon name="search" size={14} />
        <input
          type="text"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="搜索会话"
          aria-label="搜索会话"
          data-testid="agent-rail-search"
        />
      </label>

      <nav className="agent-rail__list" data-testid="agent-sessions">
        {loading ? (
          <div className="agent-rail__hint">正在加载会话…</div>
        ) : error ? (
          <EmptyState
            variant="error"
            title="会话加载失败"
            desc={error instanceof ApiError ? error.message : "请稍后重试"}
            action={
              <button type="button" className="btn btn-primary btn-sm" onClick={onRetry}>
                重试
              </button>
            }
          />
        ) : groups.length === 0 ? (
          <div className="agent-rail__hint">
            {q ? "没有匹配的会话" : "还没有会话，点「新对话」开始描述你的接入需求"}
          </div>
        ) : (
          groups.map((g) => (
            <div key={g.name} className="agent-rail__group">
              <div className="agent-rail__groupname">{g.name}</div>
              <ul>
                {g.items.map((s) => (
                  <li
                    key={s.id}
                    className={cn("agent-rail__item", s.id === activeId && "is-active")}
                    data-testid={`agent-session-${s.id}`}
                  >
                    {renaming === s.id ? (
                      <input
                        ref={inputRef}
                        type="text"
                        className="agent-rail__rename"
                        value={draft}
                        maxLength={128}
                        aria-label="会话名称"
                        data-testid="agent-rename-input"
                        onChange={(e) => setDraft(e.target.value)}
                        onBlur={() => commitRename(s.id)}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") {
                            e.preventDefault();
                            commitRename(s.id);
                          } else if (e.key === "Escape") {
                            e.preventDefault();
                            setRenaming(0);
                          }
                        }}
                      />
                    ) : (
                      <button
                        type="button"
                        className="agent-rail__btn"
                        onClick={() => {
                          onSelect(s.id);
                          onClose();
                        }}
                        onDoubleClick={() => startRename(s)}
                        title={s.title || "新对话"}
                      >
                        <span className="agent-rail__name">{s.title || "新对话"}</span>
                        <span className="agent-rail__meta">
                          {s.status === 2 ? "已归档" : `${s.last_seq} 条`}
                        </span>
                      </button>
                    )}
                    <span className="agent-rail__acts">
                      <button
                        type="button"
                        className="agent-rail__act"
                        aria-label={`重命名 ${s.title || "新对话"}`}
                        onClick={() => startRename(s)}
                      >
                        <Icon name="edit" size={13} />
                      </button>
                      <button
                        type="button"
                        className="agent-rail__act agent-rail__act--danger"
                        aria-label={`删除 ${s.title || "新对话"}`}
                        onClick={() => void askDelete(s)}
                      >
                        <Icon name="trash" size={13} />
                      </button>
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          ))
        )}
      </nav>

      <button type="button" className="agent-rail__foot" onClick={onOpenSettings}>
        <span className="agent-rail__foot-l">
          <Icon name="coin" size={14} />
          今日已用
        </span>
        <span className="agent-rail__foot-r">
          {spent.toLocaleString()}
          {budget && budget > 0 ? ` / ${budget.toLocaleString()}` : " / 不限"}
        </span>
        {budget && budget > 0 ? (
          <span className="agent-rail__bar" aria-hidden="true">
            <i style={{ width: `${ratio}%` }} data-warn={ratio >= 90 ? "1" : undefined} />
          </span>
        ) : null}
      </button>
    </aside>
  );
}

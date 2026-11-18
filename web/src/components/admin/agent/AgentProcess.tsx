import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { Icon } from "@/components/common/Icon";
import { cn } from "@/lib/utils";
import { describeToolArgs, describeToolResult, toolMetaOf } from "./toolMeta";
import type { Segment } from "./turns";

type ToolSegment = Extract<Segment, { kind: "tool" }>;

/** 思考块展开后的可视高度上限（px）。超过即裁切 + 「展开全部」。 */
const THINK_CLAMP_PX = 200;

/**
 * 折叠块的开合逻辑（思考块与工具组共用）。
 *
 * 规则：**流式中默认展开、结束后默认收起**，但一旦用户手动点过，
 * 就再也不自动改 —— 否则每来一帧都把人家展开的块收回去，等于和用户抢开关。
 *
 * `full` 与 `open` 分开：open 只表示"块展开了"，full 表示"越过限高看全文"。
 * 流式中自动展开但只给 200px（长思考不遮住正文），要点开全部得用户自己点。
 */
function useAutoFold(running: boolean, initial: boolean) {
  const [open, setOpen] = useState(initial);
  const [full, setFull] = useState(false);
  const touched = useRef(false);
  useEffect(() => {
    if (touched.current) return;
    setOpen(running);
    if (!running) setFull(false);
  }, [running]);
  const toggle = () => {
    touched.current = true;
    setOpen((v) => !v);
  };
  const expandAll = () => {
    touched.current = true;
    setFull(true);
    setOpen(true);
  };
  return { open, full, toggle, expandAll };
}

/** 思考块：模型的自言自语，默认收起（它是过程，不是答案）。 */
export function AgentThinking({
  text,
  running,
  index,
}: {
  text: string;
  running: boolean;
  index: number;
}) {
  const { open, full, toggle, expandAll } = useAutoFold(running, running);
  const bodyId = useId();
  const bodyRef = useRef<HTMLDivElement | null>(null);
  const [capped, setCapped] = useState(false);

  // 实测而不是猜字数：字体、换行、宽度都会影响高度。
  // 被裁切后 scrollHeight 仍是完整内容高度，所以可以在流式增长时反复判定。
  useLayoutEffect(() => {
    if (!open) return;
    const el = bodyRef.current;
    if (!el) return;
    setCapped(el.scrollHeight > THINK_CLAMP_PX + 1);
  }, [open, text, full]);

  const preview = text.replace(/\s+/g, " ").slice(0, 48);
  const clamped = !full && capped;
  return (
    <div className="agent-think" data-testid={`agent-thinking-${index}`}>
      <button
        type="button"
        className="agent-think__head"
        aria-expanded={open}
        aria-controls={bodyId}
        onClick={toggle}
      >
        <Icon name="brain" size={14} />
        <span className={cn("agent-think__title", running && "is-live")}>
          {running ? "正在思考…" : "思考过程"}
        </span>
        {!open && preview ? <span className="agent-think__preview">{preview}</span> : null}
        <Icon name="chevronDown" size={13} className={cn("agent-fold", open && "is-open")} />
      </button>
      {open ? (
        <>
          <div className="agent-think__wrap" data-clamp={clamped ? "on" : undefined}>
            <div className="agent-think__body" id={bodyId} ref={bodyRef}>
              {text}
            </div>
          </div>
          {clamped ? (
            <button
              type="button"
              className="agent-think__more"
              aria-label="展开全部思考过程"
              onClick={expandAll}
            >
              展开全部
            </button>
          ) : null}
        </>
      ) : null}
    </div>
  );
}

/** 单个工具卡：默认只显示"做了什么 + 成没成"，展开才看参数与结果。 */
function AgentToolCard({ item, index }: { item: ToolSegment; index: number }) {
  const [open, setOpen] = useState(false);
  const meta = toolMetaOf(item.name);
  const args = describeToolArgs(item.name, item.args);
  const result = describeToolResult(item.name, item.brief, item.ok);
  const state = item.ok === undefined ? "run" : item.ok ? "ok" : "bad";
  return (
    <li className="agent-tool" data-state={state} data-testid={`agent-tool-${index}`}>
      <button
        type="button"
        className="agent-tool__row"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="agent-tool__icon">
          <Icon name={meta.icon} size={13} />
        </span>
        <span className="agent-tool__name">{meta.label}</span>
        {args ? <span className="agent-tool__args">{args}</span> : null}
        <span className="agent-tool__state">
          {state === "run" ? (
            <span className="agent-spin" aria-label="进行中" />
          ) : state === "ok" ? (
            <Icon name="check" size={13} />
          ) : (
            <Icon name="alert" size={13} />
          )}
          {typeof item.ms === "number" && item.ms > 0 ? (
            <span className="agent-tool__ms">{item.ms < 1000 ? `${item.ms}ms` : `${(item.ms / 1000).toFixed(1)}s`}</span>
          ) : null}
        </span>
      </button>
      {result.text ? <div className="agent-tool__result" data-tone={result.tone}>{result.text}</div> : null}
      {open ? (
        <div className="agent-tool__detail">
          {item.args ? (
            <div className="agent-tool__kv">
              <span>参数</span>
              <code>{item.args}</code>
            </div>
          ) : null}
          {item.brief ? (
            <div className="agent-tool__kv">
              <span>结果</span>
              <code>{item.brief}</code>
            </div>
          ) : null}
          {!item.args && !item.brief ? <div className="agent-tool__kv">没有更多细节</div> : null}
        </div>
      ) : null}
    </li>
  );
}

/**
 * 工具组：连续的工具调用合并成一卡。
 *
 * 为什么合并：一次接入常常连查 6 张表，逐张卡片会把整段答复顶到几屏之外。
 * 合并后收起时只留一行"执行过程 · 6 个操作 · 全部成功"，需要时再展开。
 */
export function AgentToolGroup({
  items,
  running,
  index,
}: {
  items: ToolSegment[];
  running: boolean;
  index: number;
}) {
  const { open, toggle } = useAutoFold(running, running);
  const failed = items.filter((i) => i.ok === false).length;
  const labels = items.map((i) => toolMetaOf(i.name).label);
  const uniq = Array.from(new Set(labels));
  const summary = uniq.slice(0, 3).join("、") + (uniq.length > 3 ? ` 等 ${uniq.length} 项` : "");
  return (
    <div className="agent-tools" data-testid="agent-tools">
      <button type="button" className="agent-tools__head" aria-expanded={open} onClick={toggle}>
        <Icon name="zap" size={14} />
        <span className="agent-tools__title">
          执行过程 · {items.length} 个操作
          {failed > 0 ? (
            <span className="agent-tools__bad"> · {failed} 个失败</span>
          ) : running ? (
            <span className="agent-tools__live"> · 进行中</span>
          ) : (
            <span className="agent-tools__ok"> · 全部成功</span>
          )}
        </span>
        {!open ? <span className="agent-tools__summary">{summary}</span> : null}
        <Icon name="chevronDown" size={13} className={cn("agent-fold", open && "is-open")} />
      </button>
      {open ? (
        <ul className="agent-tools__list">
          {items.map((it, i) => (
            <AgentToolCard key={it.key} item={it} index={i} />
          ))}
        </ul>
      ) : null}
    </div>
  );
}

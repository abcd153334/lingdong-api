import { useEffect, useRef } from "react";
import { Icon } from "@/components/common/Icon";
import type { AgentDocItem } from "@/lib/api/agent";
import { cn } from "@/lib/utils";

export interface AgentComposerProps {
  value: string;
  onChange: (v: string) => void;
  onSend: () => void;
  onStop: () => void;
  running: boolean;
  /** 总闸/模型不可用时禁用输入（并说明原因） */
  disabled: boolean;
  disabledReason?: string;
  docs: AgentDocItem[];
  quoted: number[];
  onUnquote: (id: number) => void;
  modelLabel: string;
  modelId: string;
  /**
   * 本轮上下文占用（token）。只有实时轮有：落库轮不存 run 计量，
   * 拿"消息 token 之和"冒充占用会显示成 3–5 倍。
   */
  contextTokens?: number;
  contextWindow?: number;
  /** 被降级成摘要的轮数（>0 时提示模型只看到摘要） */
  summarizedTurns?: number;
  onOpenDocs: () => void;
  onOpenModel: () => void;
  onOpenSettings: () => void;
}

/** token 计数的可读形式：几十万的原始数字在工具条里没法扫读。 */
function ctxLabel(used: number, total: number): string {
  const f = (n: number) => (n >= 1000 ? `${(n / 1000).toFixed(0)}k` : String(n));
  return `上下文 ${f(used)}/${f(total)}`;
}

/**
 * 输入区：文本框 + 底部工具栏。
 *
 * 三个必须做对的细节（少一个就像"网页表单"而不是"聊天窗口"）：
 *  1. **回车发送、Shift+Enter 换行，且中文输入法回车不发送**：不判 `isComposing`
 *     的话，用输入法选词按回车会把半句话发出去，这是最伤人的一类 bug。
 *  2. **自适应高度**：1–8 行，超出内部滚动，不把对话区挤没。
 *  3. **运行中不禁用输入框**：只把发送换成停止。人可以先把下一句写好。
 */
export function AgentComposer({
  value,
  onChange,
  onSend,
  onStop,
  running,
  disabled,
  disabledReason,
  docs,
  quoted,
  onUnquote,
  modelLabel,
  modelId,
  contextTokens,
  contextWindow,
  summarizedTurns,
  onOpenDocs,
  onOpenModel,
  onOpenSettings,
}: AgentComposerProps) {
  const taRef = useRef<HTMLTextAreaElement | null>(null);

  // 自适应高度：先归零再按 scrollHeight 撑开，否则删字时高度不会回落。
  useEffect(() => {
    const ta = taRef.current;
    if (!ta) return;
    ta.style.height = "auto";
    ta.style.height = `${Math.min(ta.scrollHeight, 180)}px`;
  }, [value]);

  useEffect(() => {
    if (!running) taRef.current?.focus();
  }, [running]);

  const canSend = !disabled && !running && value.trim().length > 0;
  const quotedDocs = docs.filter((d) => quoted.includes(d.id));

  return (
    <div className="agent-composer-wrap">
      <div className={cn("agent-composer", running && "is-running")} data-testid="agent-composer">
        {quotedDocs.length > 0 ? (
          <div className="agent-quotes">
            {quotedDocs.map((d) => (
              <span key={d.id} className="agent-quote" data-testid={`agent-quote-${d.id}`}>
                <Icon name="doc" size={12} />
                <span>{d.title}</span>
                <button
                  type="button"
                  aria-label={`不再引用 ${d.title}`}
                  onClick={() => onUnquote(d.id)}
                >
                  <Icon name="close" size={11} />
                </button>
              </span>
            ))}
          </div>
        ) : null}

        <textarea
          ref={taRef}
          className="agent-composer__input"
          value={value}
          rows={1}
          placeholder={disabled ? disabledReason || "当前不可用" : "描述你想接入的厂商，或直接把文档贴进来…"}
          disabled={disabled}
          aria-label="需求描述"
          data-testid="agent-input"
          onChange={(e) => onChange(e.target.value)}
          onKeyDown={(e) => {
            if (e.key !== "Enter" || e.shiftKey) return;
            // 输入法组合期（中文选词）的回车是"确认候选词"，不是"发送"。
            if (e.nativeEvent.isComposing) return;
            e.preventDefault();
            if (canSend) onSend();
          }}
        />

        <div className="agent-bar" data-testid="agent-composer-toolbar">
          <button
            type="button"
            className="agent-toolbtn"
            onClick={onOpenDocs}
            data-testid="agent-docs-open"
          >
            <Icon name="attach" size={15} />
            <span>素材</span>
            {docs.length > 0 ? <span className="agent-toolbtn__badge">{docs.length}</span> : null}
          </button>

          <button
            type="button"
            className="agent-toolbtn agent-toolbtn--model"
            onClick={onOpenModel}
            data-testid="agent-model-chip"
            title={modelId || "跟随系统默认"}
          >
            <Icon name="spark" size={14} />
            <span>{modelLabel}</span>
            <Icon name="chevronDown" size={12} />
          </button>

          <span className="agent-bar__spacer" />

          {/* 上下文占用：只显示"用了多少 / 有多少"。
              不显示的话，"模型忘事 / 答得敷衍"永远无法归因到窗口满了。 */}
          {contextTokens && contextWindow ? (
            <span className="agent-ctxmeter" data-testid="agent-ctxmeter">
              {ctxLabel(contextTokens, contextWindow)}
              {summarizedTurns ? ` · 摘要 ${summarizedTurns} 轮` : ""}
            </span>
          ) : null}

          <button
            type="button"
            className="agent-iconbtn"
            onClick={onOpenSettings}
            aria-label="运行参数"
            data-testid="agent-params-open"
          >
            <Icon name="gear" size={16} />
          </button>

          {running ? (
            <button
              type="button"
              className="agent-send agent-send--stop"
              onClick={onStop}
              aria-label="停止"
              data-testid="agent-stop"
            >
              <Icon name="stop" size={15} />
            </button>
          ) : (
            <button
              type="button"
              className="agent-send"
              onClick={onSend}
              disabled={!canSend}
              aria-label="发送"
              data-testid="agent-send"
            >
              <Icon name="send" size={16} />
            </button>
          )}
        </div>
      </div>
      <div className="agent-composer__foot">
        助手只会生成提案，写入必须由你确认 · Enter 发送 / Shift+Enter 换行
      </div>
    </div>
  );
}

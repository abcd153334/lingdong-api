import { useEffect, useMemo, useState } from "react";
import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { useAnswerAgentAskMutation } from "@/lib/api/agent";
import type { Segment } from "./turns";

/**
 * 助手向运营提问的卡片。
 *
 * 三条设计约束：
 *  1. **只有 pending 可交互**。answered / timeout / expired / aborted 一律只读留痕：
 *     一个"点得动但点了报错"的卡片比没有卡片更糟（它的 run 早就不在了）。
 *  2. **倒计时只是提示**，不参与任何判定：真正的过期由服务端按 expires_at 决定，
 *     前端自己判过期会在时钟不准时把还能答的卡片锁死。
 *  3. 作答走独立端点（POST /agent/asks/:id/answer），**不占用输入框**：
 *     输入框仍然可以发新消息（会得到"还在等你回答"的 409 提示）。
 */
export function AgentAskCard({
  ask,
  sessionId,
  onSettled,
}: {
  ask: Extract<Segment, { kind: "ask" }>;
  sessionId: number;
  /** 作答成功后通知父级把本地段切成已答态（不重取消息，避免顶掉流式轮） */
  onSettled: (status: string, answer?: string) => void;
}) {
  const answer = useAnswerAgentAskMutation(sessionId);
  const [picked, setPicked] = useState<string[]>([]);
  const [free, setFree] = useState("");

  const pending = ask.status === "pending";
  const left = useAskCountdown(ask.expiresIn ?? 0, pending);
  const multi = ask.multi;
  const options = ask.options;

  const toggle = (value: string) => {
    if (!pending) return;
    setPicked((cur) => {
      if (multi) return cur.includes(value) ? cur.filter((v) => v !== value) : [...cur, value];
      // 单选再点一次 = 取消选择（否则选错了就只能刷新）
      return cur.includes(value) ? [] : [value];
    });
  };

  const canSubmit = useMemo(() => {
    if (!pending || answer.isPending) return false;
    if (picked.length > 0) return true;
    return ask.allowFree && free.trim() !== "";
  }, [pending, answer.isPending, picked.length, ask.allowFree, free]);

  const submit = (skipped = false) => {
    if (!pending || answer.isPending) return;
    answer.mutate(
      { askId: ask.askId, selected: skipped ? [] : picked, freeText: skipped ? "" : free.trim(), skipped },
      {
        onSuccess: () => {
          // 摘要用 label 不用 value：卡片上之后显示的是这句摘要，
          // 而 "per_spec" 这种内部值回看时没人认得出是哪个按钮。
          const labelOf = (v: string) => options.find((o) => o.value === v)?.label ?? v;
          const brief = skipped
            ? "已跳过"
            : [...picked.map(labelOf), free.trim()].filter(Boolean).join("；");
          onSettled(skipped ? "skipped" : "answered", brief);
        },
      },
    );
  };

  // 服务端的过期是唯一判据：这里只做文案，不改状态。
  // pending 时显示倒计时（60 秒很短，运营得知道还剩多少），终态时显示结果文案。
  const stateText = pending ? (ask.expiresIn ? `剩余 ${left}s` : "") : stateLabel(ask.status);

  return (
    <div className="agent-ask" data-testid="agent-ask" data-state={ask.status}>
      <div className="agent-ask__head">
        <Icon name="help" size={14} />
        <span className="agent-ask__q">{ask.question}</span>
        {stateText ? <span className="agent-ask__state">{stateText}</span> : null}
      </div>
      {ask.why ? <div className="agent-ask__why">问这个是因为：{ask.why}</div> : null}

      {options.length > 0 ? (
        <div className="agent-ask__opts" role={multi ? "group" : "radiogroup"}>
          {options.map((o) => {
            const on = picked.includes(o.value);
            return (
              <button
                key={o.value}
                type="button"
                className={`agent-ask__opt${on ? " is-on" : ""}`}
                aria-pressed={on}
                disabled={!pending}
                onClick={() => toggle(o.value)}
                title={o.detail}
              >
                <span className="agent-ask__label">{o.label}</span>
                {o.detail ? <span className="agent-ask__detail">{o.detail}</span> : null}
              </button>
            );
          })}
        </div>
      ) : null}

      {pending && ask.allowFree ? (
        <Input
          className="agent-ask__free"
          value={free}
          maxLength={2000}
          placeholder="补充说明（可只填这个）"
          disabled={answer.isPending}
          onChange={(e) => setFree(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && canSubmit) submit();
          }}
        />
      ) : null}

      {ask.status === "answered" && ask.answer ? (
        <div className="agent-ask__ans">已回答：{ask.answer}</div>
      ) : null}

      {pending ? (
        <div className="agent-ask__acts">
          <button
            type="button"
            className="btn btn-primary btn-sm"
            data-testid="agent-ask-submit"
            disabled={!canSubmit}
            onClick={() => submit()}
          >
            {answer.isPending ? "提交中…" : "提交"}
          </button>
          <button
            type="button"
            className="agent-linkbtn"
            data-testid="agent-ask-skip"
            disabled={answer.isPending}
            onClick={() => submit(true)}
          >
            跳过
          </button>
          {answer.isError ? <span className="agent-ask__err">提交失败，请重试</span> : null}
        </div>
      ) : null}
    </div>
  );
}

/** 终态文案：只有"不在等人答"时才显示，避免 pending 时多一行噪音。 */
function stateLabel(status: string): string {
  switch (status) {
    case "pending":
      return "";
    case "answered":
      return "已回答";
    case "skipped":
      return "已跳过";
    case "timeout":
      return "已超时（按默认继续）";
    case "expired":
      return "已随本轮结束";
    case "aborted":
      return "本轮中断";
    case "denied":
      return "本轮提问次数已用完";
    case "no_time":
      return "剩余时间不足，未等待";
    default:
      return "未提问";
  }
}

/**
 * 倒计时交给卡片内部：它只是"还剩多少秒"的提示。
 *
 * 抽成 hook 而不是写在组件里，是因为它需要一个 1s 的 interval，
 * 而组件本身重渲染很频繁（流式内容在长高）—— 把 interval 独立出来，
 * 卸载时能被准确清掉（否则每个提问卡都留一个泄漏的定时器）。
 */
function useAskCountdown(expiresIn: number, active: boolean): number {
  const [left, setLeft] = useState(expiresIn);
  useEffect(() => {
    if (!active) return;
    setLeft(expiresIn);
    const t = window.setInterval(() => setLeft((n) => (n > 0 ? n - 1 : 0)), 1000);
    return () => window.clearInterval(t);
  }, [expiresIn, active]);
  return left;
}

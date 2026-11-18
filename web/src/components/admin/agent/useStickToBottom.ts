import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

/** 距底多少像素内算"贴着底"：留一点余量，避免滚动条余数让跟随永久失效。 */
const NEAR_BOTTOM = 80;
/** scrollTop 回退超过这个像素才算"人往上翻了"，用来和"内容变高"区分开。 */
const UP_THRESHOLD = 8;

export interface StickToBottom {
  /** 挂到滚动容器（`.agent-thread`） */
  ref: React.RefObject<HTMLDivElement | null>;
  /** 是否处于跟随态 */
  stick: boolean;
  /** 未跟随时是否来了新内容（用于浮出跳底按钮） */
  hasNew: boolean;
  scrollToBottom: (smooth?: boolean) => void;
  onScroll: () => void;
}

/**
 * 对话区的"贴底跟随"。
 *
 * 为什么不用依赖数组监听内容：内容高度变化的来源远不止 React 状态引用变化 ——
 * Markdown 渲染、代码块/图片、提案卡异步加载、思考块展开收起，都会在 effect
 * 跑完之后才把高度撑起来。只按 `[turns.length, segments]` 触发，滚动会停在半截。
 *
 * 为什么"距底超过阈值"不能作为退出跟随的判据：流式输出时内容每帧都在长，
 * 我们上一帧贴的底在下一帧就变成了"离底 N 像素"，据此退出跟随会永久停住（内容
 * 一变长就退订，再也不会重新跟上）。真正区分"人在回看"与"内容在长"的信号是
 * **scrollTop 有没有往后退**：内容增长不动 scrollTop，人往上翻才会把它减小。
 */
export function useStickToBottom(sessionKey: number): StickToBottom {
  const ref = useRef<HTMLDivElement | null>(null);
  const [stick, setStick] = useState(true);
  const [hasNew, setHasNew] = useState(false);
  // 回调里读最新值：ResizeObserver 的回调注册一次，不能闭包住首帧的 state。
  const stickRef = useRef(true);
  const topRef = useRef(0);
  const frameRef = useRef(0);

  const pin = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
    topRef.current = el.scrollTop;
  }, []);

  const schedulePin = useCallback(() => {
    if (frameRef.current) return; // 一帧只滚一次：流式帧率远高于布局频率
    frameRef.current = requestAnimationFrame(() => {
      frameRef.current = 0;
      if (!stickRef.current) return;
      pin();
    });
  }, [pin]);

  const scrollToBottom = useCallback(
    (smooth = false) => {
      if (frameRef.current) {
        cancelAnimationFrame(frameRef.current);
        frameRef.current = 0;
      }
      const el = ref.current;
      if (el) {
        el.scrollTo({ top: el.scrollHeight, behavior: smooth ? "smooth" : "auto" });
        topRef.current = el.scrollTop;
      }
      stickRef.current = true;
      setStick(true);
      setHasNew(false);
    },
    [],
  );

  const onScroll = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    const top = el.scrollTop;
    const prev = topRef.current;
    topRef.current = top;

    if (el.scrollHeight - top - el.clientHeight <= NEAR_BOTTOM) {
      stickRef.current = true;
      setStick(true);
      setHasNew(false);
      return;
    }
    if (top < prev - UP_THRESHOLD) {
      // 人主动往上翻：交出滚动控制权，只提示有新内容。
      stickRef.current = false;
      setStick(false);
      return;
    }
    // 既没贴底、scrollTop 也没后退 → 是内容长高把底推远了，继续跟。
    if (stickRef.current) schedulePin();
  }, [schedulePin]);

  // 切会话 / 首次挂载：重置跟随态并立即贴底。
  // 不能只靠 messagesLoading 的 true→false：react-query 缓存命中时 isLoading 恒 false，
  // 切回旧会话就永远停在顶部。
  useLayoutEffect(() => {
    stickRef.current = true;
    setStick(true);
    setHasNew(false);
    pin();
  }, [sessionKey, pin]);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const content = el.firstElementChild as HTMLElement | null;
    if (!content || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => {
      if (!stickRef.current) {
        setHasNew(true);
        return;
      }
      schedulePin();
    });
    ro.observe(content);
    return () => {
      ro.disconnect();
      if (frameRef.current) {
        cancelAnimationFrame(frameRef.current);
        frameRef.current = 0;
      }
    };
  }, [sessionKey, schedulePin]);

  return { ref, stick, hasNew, scrollToBottom, onScroll };
}

import { useEffect, useState } from "react";

/**
 * 发码倒计时（秒）。
 *
 * 注册 / 找回 / 换绑三处原本各写一份**完全相同**的 `useState + setInterval`，
 * 文案统一之后没有理由再留三份状态机 —— 改一处总会漏掉另两处。
 *
 * 语义：`left > 0` 即在倒计时；到 0 自动停表。`setLeft` 由调用方在发码成功
 * 后设成服务端的 `expires_in`（默认 60）。
 */
export function useCountdown() {
  const [left, setLeft] = useState(0);

  useEffect(() => {
    if (left <= 0) return;
    // 依赖是 `left > 0` 而不是 `left`：每减一秒都重建定时器会漂移，
    // 但"从 0 变正数"必须重启一次。
    const timer = window.setInterval(() => setLeft((v) => (v <= 1 ? 0 : v - 1)), 1000);
    return () => window.clearInterval(timer);
  }, [left > 0]);

  return { left, setLeft };
}

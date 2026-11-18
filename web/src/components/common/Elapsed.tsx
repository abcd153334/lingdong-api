import { useEffect, useState, useSyncExternalStore } from "react";
import { cn } from "@/lib/utils";

/**
 * 进行中耗时展示（单例 ticker）。
 *
 * 为什么不是每行一个 `setInterval`：调用记录页一屏 20 行、管理台 50 行，
 * 每行一个定时器意味着 50 个并发回调在同一个帧里改 state，滚动直接掉帧；
 * 而且它们永远不同步，秒数会一行一行跳。
 *
 * 为什么用 `performance.now()` 而不是 `Date.now()`：
 * - 单调时钟不会被系统时间调整影响（用户改时区、NTP 校时都不会让耗时往回跳）；
 * - 标签页休眠后 `Date.now()` 的差值是"真实过去的时间"，而耗时应该只按服务端基准走 ——
 *   两者在休眠场景下会出现几十秒的跳变，用户会以为卡住了。
 *
 * 计数基准来自服务端 `elapsed_ms`：所以刷新页面能接着涨，且不受客户端时钟影响。
 */

const subscribers = new Set<() => void>();
let timer: ReturnType<typeof setInterval> | null = null;
let counter = 0;

/** 单例订阅：所有耗时单元格共享一个 1 秒定时器。 */
function subscribe(onChange: () => void): () => void {
  subscribers.add(onChange);
  if (timer === null) {
    timer = setInterval(() => {
      counter += 1;
      subscribers.forEach((fn) => fn());
    }, 1000);
  }
  return () => {
    subscribers.delete(onChange);
    if (subscribers.size === 0 && timer !== null) {
      clearInterval(timer);
      timer = null;
    }
  };
}

function getCounter(): number {
  return counter;
}

/**
 * 每秒触发一次重渲染（全站共用一个 ticker）。
 *
 * 只给**需要每秒刷新**的叶子节点订阅：列表/表单级组件订阅会让整块内容每秒重渲染，
 * 100 行密钥的抽屉里那就是每秒 100 个 DOM diff。
 */
export function useSecondTick(): number {
  return useSyncExternalStore(subscribe, getCounter, getCounter);
}

/** 单调时钟读数（不可用环境回落 0，不影响展示）。 */
function monotonic(): number {
  if (typeof performance !== "undefined" && typeof performance.now === "function") {
    return performance.now();
  }
  return 0;
}

/** 把毫秒格式化成人类可读的耗时。 */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const totalSec = ms / 1000;
  if (totalSec < 60) return `${totalSec.toFixed(2)}s`;
  const sec = Math.floor(totalSec);
  if (sec < 3600) {
    const m = Math.floor(sec / 60);
    const s = sec % 60;
    return s === 0 ? `${m}m` : `${m}m${String(s).padStart(2, "0")}s`;
  }
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return `${h}h${String(m).padStart(2, "0")}m`;
}

/** 超过该时长仍在进行中，提示"可能已中断"（回收任务 10 分钟兜底，这里只做提示）。 */
const STALE_PENDING_MS = 15 * 60 * 1000;

export interface ElapsedProps {
  /** 服务端算的已进行毫秒（终态时等于总耗时） */
  elapsedMs: number;
  /** 记录状态：pending 才需要每秒递增 */
  status: string;
  /** 异常标记（含 reclaimed 时显示 ≥，表示耗时只是下界） */
  anomaly?: string;
  className?: string;
}

/**
 * 耗时单元格。
 *
 * - 进行中：`进行中 · 12s`，每秒上涨；超过 15 分钟追加「可能已中断」。
 * - 终态：`1.24s`；若 anomaly 含 `reclaimed`（进程崩溃后被回收任务补账），
 *   显示为 `≥ 10m` —— 那种情况下的耗时刻度是"回收时刻"，不是真实结束时刻。
 */
export function Elapsed({ elapsedMs, status, anomaly, className }: ElapsedProps) {
  // 订阅单例 ticker：值本身不重要，重要的是每秒触发一次重渲染。
  useSecondTick();

  const [anchor, setAnchor] = useState<{ base: number; at: number; key: string } | null>(null);
  const key = `${status}:${elapsedMs}`;

  useEffect(() => {
    setAnchor({ base: elapsedMs, at: monotonic(), key });
  }, [key, elapsedMs]);

  const pending = status === "pending";
  const live =
    pending && anchor && anchor.key === key
      ? anchor.base + Math.max(0, monotonic() - anchor.at)
      : elapsedMs;

  if (pending) {
    const stale = live > STALE_PENDING_MS;
    return (
      <span className={cn("num small", className)}>
        进行中 · {formatDuration(live)}
        {stale ? <span className="muted"> 可能已中断</span> : null}
      </span>
    );
  }

  const reclaimed = (anomaly ?? "").split(",").includes("reclaimed");
  return (
    <span className={cn("num small", className)}>
      {reclaimed ? "≥ " : ""}
      {formatDuration(live)}
    </span>
  );
}

import type { ReactNode } from "react";
import { copyText } from "@/lib/clipboard";

/**
 * 详情面板共用的最小展示件（调用详情与任务详情同一套 DOM/类名）。
 *
 * 为什么单独成文件而不是从 CallDetailDrawer 导出：TaskInfoSection 被
 * CallDetailDrawer 引用，反向 import 会成环。
 */

/** 键值行（`.cd-kv` 的直接子项）。 */
export function KV({ k, children }: { k: string; children: ReactNode }) {
  return (
    <div className="cd-kv-row">
      <span className="k">{k}</span>
      <span className="v">{children}</span>
    </div>
  );
}

/** 长标识：中间省略 + 悬停看全文 + 点击复制。 */
export function LongID({ value }: { value: string }) {
  const short = value.length > 32 ? `${value.slice(0, 10)}…${value.slice(-6)}` : value;
  return (
    <span
      className="mono small cd-longid"
      title={value}
      role="button"
      tabIndex={0}
      onClick={() => void copyText(value)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          void copyText(value);
        }
      }}
    >
      {short}
    </span>
  );
}

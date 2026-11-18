import { useEffect, useRef, useState } from "react";

import { toastErr } from "@/components/common/Toaster";
import { copyText } from "@/lib/clipboard";

/**
 * 复制按钮（文档站唯一实现）。
 *
 * 反馈放在按钮自己身上（1.5s 后复原），而不是全局 toast：一页上有 6 个复制按钮，
 * toast 会把读者刚看的代码挤下去；失败则必须 toast —— 静默失败会让人以为复制
 * 成功、粘贴出一个空串。
 *
 * `text` 支持函数：Markdown 要先走后端接口取正文，不能提前拼。
 */
export function CopyBtn({
  text,
  label = "复制",
  className = "d-iconbtn",
  title,
  disabled,
}: {
  text: string | (() => Promise<string>);
  label?: string;
  className?: string;
  title?: string;
  disabled?: boolean;
}) {
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);
  const timer = useRef<number | null>(null);

  useEffect(() => () => { if (timer.current) window.clearTimeout(timer.current); }, []);

  const run = async () => {
    if (busy) return;
    setBusy(true);
    try {
      const value = typeof text === "function" ? await text() : text;
      if (!value) {
        toastErr("这里还没有内容可复制");
        return;
      }
      if (await copyText(value)) {
        setDone(true);
        if (timer.current) window.clearTimeout(timer.current);
        timer.current = window.setTimeout(() => setDone(false), 1500);
      } else {
        toastErr("复制失败，请手动选中");
      }
    } catch {
      toastErr("获取内容失败，请稍后重试");
    } finally {
      setBusy(false);
    }
  };

  return (
    <button
      type="button"
      className={className}
      title={title}
      disabled={disabled || busy}
      data-copied={done ? "1" : undefined}
      onClick={() => void run()}
    >
      {busy ? "…" : done ? "已复制" : label}
    </button>
  );
}

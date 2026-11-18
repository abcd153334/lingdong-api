/**
 * 剪贴板写入的唯一实现。
 *
 * 此前有 5 份平行实现（docsClient 一份带 execCommand 兜底、DetailBits 一份静默版、
 * 门户三个页面各一份「静默 + toast」）：兜底能力只存在于其中一份，用户在
 * 非安全上下文（内网 http）点「复制」有时成功有时静默失败，取决于点的按钮在哪个页面。
 *
 * 两层：
 *   - `copyText` 只回答「成不成」，调用方自己决定怎么反馈（文档站要在按钮上变色）；
 *   - `copyToast` 是门户的口径（统一一句话提示），页面直接用。
 */
import { toast } from "sonner";

/** 复制文本；失败时用 execCommand 兜底（旧浏览器 / 非 HTTPS 下 clipboard 不可用）。 */
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand("copy");
      ta.remove();
      return ok;
    } catch {
      return false;
    }
  }
}

/** 复制并给出结果提示（门户统一文案；失败必须说出口，否则用户以为复制成功了）。 */
export async function copyToast(text: string): Promise<void> {
  if (await copyText(text)) toast.success("已复制到剪贴板");
  else toast.error("复制失败，请手动复制");
}

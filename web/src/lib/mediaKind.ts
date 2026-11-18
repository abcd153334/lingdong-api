/**
 * 媒体类型判定（前端唯一实现）。
 *
 * 判定顺序与后端 `internal/model/media.go` 的 `MediaKind` **完全一致**：
 *   MIME → URL 扩展名 → 后端给的 kind 提示 → file
 *
 * 为什么不能直接用后端给的 `kind`：那个值来自端点模态（video 端点回一张 jpg 封面
 * 时是 "video"），拿它选渲染器会变成「用 <video> 渲染一张图」——黑屏 + 控制条，
 * 用户看到的就是"预览坏了"。后端已按同一规则判过一次，这里是二次对齐
 * （管理台直接预览上游直链时，只有前端这一层）。
 */

export type MediaKind = "image" | "video" | "audio" | "text" | "file";

const EXT_MIME: Record<string, string> = {
  png: "image/png",
  jpg: "image/jpeg",
  jpeg: "image/jpeg",
  webp: "image/webp",
  gif: "image/gif",
  avif: "image/avif",
  bmp: "image/bmp",
  svg: "image/svg+xml",
  mp4: "video/mp4",
  webm: "video/webm",
  mov: "video/quicktime",
  m4v: "video/x-m4v",
  mkv: "video/x-matroska",
  mp3: "audio/mpeg",
  wav: "audio/wav",
  m4a: "audio/mp4",
  aac: "audio/aac",
  ogg: "audio/ogg",
  flac: "audio/flac",
  txt: "text/plain",
  json: "application/json",
  csv: "text/csv",
  md: "text/markdown",
  pdf: "application/pdf",
};

/** 从 URL 路径取 MIME；只看 path，不看 query（签名链接里常有假后缀）。 */
export function mimeOfUrl(url?: string): string {
  if (!url || /^data:/i.test(url)) return "";
  const path = url.split(/[?#]/)[0];
  const dot = path.lastIndexOf(".");
  if (dot < 0 || dot === path.length - 1) return "";
  return EXT_MIME[path.slice(dot + 1).toLowerCase()] ?? "";
}

/** MIME → 渲染档；空串表示"不认识这个类型"。 */
export function kindOfMime(mime?: string): MediaKind | "" {
  const ct = (mime ?? "").split(";")[0].trim().toLowerCase();
  if (!ct) return "";
  // SVG 可内嵌 <script>：与后端同样按"文件"处理（不内联渲染）。
  if (ct === "image/svg+xml") return "file";
  if (ct.startsWith("image/")) return "image";
  if (ct.startsWith("video/")) return "video";
  if (ct.startsWith("audio/")) return "audio";
  if (ct.startsWith("text/")) return "text";
  if (ct === "application/json" || ct === "application/xml" || ct === "application/csv") return "text";
  return "";
}

/** 综合判定：MIME 优先，其次扩展名，最后用调用方给的提示兜底。 */
export function resolveMediaKind(kind?: string, mime?: string, url?: string): MediaKind {
  const byMime = kindOfMime(mime);
  if (byMime) return byMime;
  const byUrl = kindOfMime(mimeOfUrl(url));
  if (byUrl) return byUrl;
  if (kind === "image" || kind === "video" || kind === "audio" || kind === "text") return kind;
  return "file";
}

/** 能否在页面内直接渲染（其余一律只给下载）。 */
export function canRender(kind: MediaKind): boolean {
  return kind === "image" || kind === "video" || kind === "audio" || kind === "text";
}

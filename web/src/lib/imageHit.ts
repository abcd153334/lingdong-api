/** 流式出图的一帧里能用来预览的产物。 */
export type MediaHit = { url: string; kind: "image" | "video" };

/**
 * 从一帧 SSE 数据里识别「这是一张图」。
 *
 * 三层判据缺一不可，任何一层漏掉都是**静默**失败：
 *  - 只看 url：`response_format=b64_json` 的流预览区一张图都看不到；
 *  - 不看 type：任意带 url 的帧（错误帧里的文档链接）都会被当成产物；
 *  - 解析失败就抛：心跳帧会让整条调试台报错。
 */
export function imageHitOf(raw: string): MediaHit | null {
  const text = (raw ?? "").trim();
  if (!text) return null;
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch {
    return null;
  }
  if (!v || typeof v !== "object") return null;
  const o = v as Record<string, unknown>;
  const type = typeof o.type === "string" ? o.type : "";
  if (!type.startsWith("image_generation.")) return null;
  if (typeof o.url === "string" && /^https?:\/\//i.test(o.url)) {
    return { url: o.url, kind: "image" };
  }
  if (typeof o.b64_json === "string" && o.b64_json) {
    return { url: `data:image/png;base64,${o.b64_json}`, kind: "image" };
  }
  return null;
}

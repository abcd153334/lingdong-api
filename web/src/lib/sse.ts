import { apiUrl } from "@/lib/apiBase";
import { ApiError } from "@/lib/apiClient";
import { csrfHeaders } from "@/lib/csrf";

/**
 * 一条 SSE 消息（`data:` 载荷已解出，未做 JSON 解析）。
 *
 * 为什么不用浏览器的 EventSource：它只支持 GET，而"发一条消息跑一轮编排"
 * 必须是 POST（要带正文与文档引用）。凭证虽然是 Cookie（EventSource 反而能带），
 * 但 POST 这一条就足以让它出局。
 */
export interface SSEEvent {
  /** 事件类型（后端放在 data 的 type 字段里，而不是 SSE 的 event: 行） */
  type: string;
  /** 已解析的载荷 */
  payload: Record<string, unknown>;
}

export interface StreamOptions {
  /** 外部中止（组件卸载 / 用户点「停止」） */
  signal?: AbortSignal;
  /** 每收到一条消息回调一次；抛错即中断流 */
  onEvent: (ev: SSEEvent) => void;
}

/**
 * 以 POST 方式消费 SSE 流。
 *
 * 三条实现要点：
 * 1. 用 `fetch` + `ReadableStream` 而不是 EventSource（理由见上）；
 * 2. **按 `\n\n` 切帧**并保留残余缓冲：一次 read 可能拿到半帧，
 *    也可能一次拿到好几帧。用 `split("\n\n")` 后丢掉最后一段是错的——
 *    那一段正是"下一帧的开头"，丢了就会少字；
 * 3. 后端把类型放在 JSON 里（`{"type":"delta"}`），所以只解析 `data:` 行。
 */
export async function streamSSE(
  path: string,
  body: unknown,
  { signal, onEvent }: StreamOptions,
): Promise<void> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    Accept: "text/event-stream",
    // 编排端点是 POST，服务端要 CSRF 双提交令牌；漏了这个头会 403，
    // 而 403 在流式场景下表现成「点了没反应」（错误体不是流，被 readError 吞成一句话）。
    ...csrfHeaders("POST"),
  };

  let res: Response;
  try {
    res = await fetch(apiUrl(path), {
      method: "POST",
      headers,
      body: JSON.stringify(body ?? {}),
      signal,
      // 凭证在 httpOnly Cookie 里，不显式带 credentials 浏览器不会发。
      credentials: "include",
    });
  } catch (err) {
    if (isAbort(err)) return;
    throw new ApiError(-1, "网络异常，请检查连接后重试");
  }

  // 非 200 时后端回的是普通 JSON 错误体（不是流）：按统一错误体解析，
  // 否则前端会把 `{"code":409,...}` 当成 SSE 数据，报一句毫无关联的解析错误。
  if (!res.ok || !res.body) {
    const message = await readError(res);
    throw new ApiError(res.status, message);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder("utf-8");
  let buffer = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      buffer = drainFrames(buffer, onEvent);
    }
    // 流结束时缓冲区可能还剩最后一帧（后端未以空行收尾时）。
    buffer += decoder.decode();
    drainFrames(buffer, onEvent, true);
  } catch (err) {
    if (isAbort(err)) return;
    throw err;
  } finally {
    reader.cancel().catch(() => undefined);
  }
}

/** 从缓冲区里取出完整帧，返回剩余残余。 */
function drainFrames(
  buffer: string,
  onEvent: (ev: SSEEvent) => void,
  flush = false,
): string {
  const normalized = buffer.replace(/\r\n/g, "\n");
  const parts = normalized.split("\n\n");
  // flush 时最后一段也是完整帧（流已结束），不保留残余。
  const rest = flush ? "" : (parts.pop() ?? "");
  for (const frame of parts) {
    const ev = parseFrame(frame);
    if (ev) onEvent(ev);
  }
  return rest;
}

/** 解析一帧：只取 `data:` 行，忽略 `:` 开头的注释帧与 `event:` 行。 */
function parseFrame(frame: string): SSEEvent | null {
  const dataLines: string[] = [];
  for (const line of frame.split("\n")) {
    if (!line.startsWith("data:")) continue;
    dataLines.push(line.slice(5).trimStart());
  }
  if (dataLines.length === 0) return null;
  const raw = dataLines.join("\n").trim();
  if (raw === "" || raw === "[DONE]") return null;
  try {
    const payload = JSON.parse(raw) as Record<string, unknown>;
    const type = typeof payload.type === "string" ? payload.type : "";
    if (!type) return null;
    return { type, payload };
  } catch {
    // 半帧/坏帧：丢掉而不是抛错。抛错会让一次网络抖动中断整轮编排，
    // 而丢掉一帧最多是少显示一小段（后端仍会落库完整内容）。
    return null;
  }
}

/** 读非 200 响应的错误文案（尽量读出后端统一错误体）。 */
async function readError(res: Response): Promise<string> {
  try {
    const payload = (await res.json()) as { message?: string };
    if (payload?.message) return payload.message;
  } catch {
    /* 非 JSON：用状态码兜底 */
  }
  return `请求失败（HTTP ${res.status}）`;
}

function isAbort(err: unknown): boolean {
  return (
    (err instanceof DOMException && err.name === "AbortError") ||
    (err instanceof Error && err.name === "AbortError")
  );
}

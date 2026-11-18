import { Tag } from "@/components/common/StatusTag";
import { formatBytes, type ProbeResult } from "@/lib/apidocs/probe";

/**
 * 试运行结果。
 *
 * 三件事必须在同一屏里看全：**状态码**（4xx 也是结果，不是"失败"）、
 * **耗时**（判断"是不是超时"要比看日志快）、**响应体原文**（错误体的形状
 * 就是接入方要照着处理的形状，任何美化都不能改变内容）。
 *
 * 2xx 与 4xx/5xx 的视觉差异只做在状态徽标上，不做在整体配色上：
 * 把 4xx 整块染红会让人以为"试运行失败了"，而它其实是一次成功的观察。
 */
export function ResponseView({ result }: { result: ProbeResult }) {
  return (
    <div className="adx-resp" data-testid="apidoc-try-result">
      <div className="adx-resphead">
        <Tag tone={result.ok ? "success" : "danger"}>
          {result.status} {result.statusText}
        </Tag>
        <span className="muted">{result.ms} ms</span>
        <span className="muted">{formatBytes(result.bytes)}</span>
        {result.contentType ? <span className="muted">{result.contentType}</span> : null}
      </div>
      <pre className="code-block adx-respbody">{result.body || "（空响应体）"}</pre>
    </div>
  );
}

import { Tag, type TagTone } from "@/components/common/StatusTag";

import type { ApiDocEndpoint } from "@/types";
import { JsonBlock } from "./JsonBlock";
import { ParamTable } from "./ParamTable";
import { TryItPanel } from "./TryItPanel";

/** 鉴权级别 → 徽标色调（与后端 Auth 枚举一一对应，不做推断）。 */
const AUTH_TONE: Record<string, TagTone> = {
  open: "gray",
  reader: "info",
  writer: "warning",
  session: "purple",
};

const AUTH_LABEL: Record<string, string> = {
  open: "免鉴权",
  reader: "只读",
  writer: "受限写",
  session: "仅会话票",
};

/**
 * 中栏：一条端点的完整契约。
 *
 * 数据全部来自后端 `apicontract`，本组件**只排版、不加工**：
 * 不重拼路径、不推断鉴权、不补默认参数。任何一个"顺手补上"都会让文档
 * 与运行期守卫分叉，而分叉的表现是接入方照着文档写代码拿到 403 ——
 * 这种缺陷没有任何自动化手段能发现（文档说 A，代码做 B，两边各自自洽）。
 */
export function EndpointDetail({ e }: { e: ApiDocEndpoint }) {
  const hasReqExample = Object.keys(e.request_example).length > 0;
  const hasRespExample = Object.keys(e.response_example).length > 0;

  return (
    <div className="adx-main">
      <header className="adx-head">
        <div className="row1">
          <span className={`adx-m ${e.method.toLowerCase()}`}>{e.method}</span>
          <span className="ttl">{e.title}</span>
          <Tag tone={AUTH_TONE[e.auth] ?? "gray"}>{AUTH_LABEL[e.auth] ?? e.auth}</Tag>
          {e.scope ? (
            <Tag tone="gray">
              {e.scope}
              {/* scope_enforced 为 false 时必须说清：否则接入方会去申请一个
                  运行期根本不校验的权限点，然后反复自查为什么申请了没用。 */}
              {e.scope_enforced ? "" : "（不校验）"}
            </Tag>
          ) : null}
          {e.media !== "json" ? <Tag tone="warning">{e.media}</Tag> : null}
        </div>
        <div className="adx-path">
          <span>{e.path}</span>
        </div>
        {e.idempotency ? <p className="muted">{e.idempotency}</p> : null}
        {e.notes ? <p className="muted">{e.notes}</p> : null}
      </header>

      {/* 试运行放在参数之前：点开一条接口，第一件想做的事是"发一次看看"，
          而不是先读 8 行参数说明。默认收起，理由见 TryItPanel 顶部注释。 */}
      <TryItPanel e={e} />

      <section className="adx-sec">
        <div className="h">
          <b>参数</b>
          <span>
            {e.params.length ? `${e.params.length} 个` : "无"}
          </span>
        </div>
        {e.params.length ? (
          <ParamTable params={e.params} />
        ) : (
          <p className="muted">这条接口不需要任何参数。</p>
        )}
      </section>

      {hasReqExample ? (
        <section className="adx-sec">
          <div className="h">
            <b>请求示例</b>
          </div>
          <JsonBlock text={JSON.stringify(e.request_example, null, 2)} testId="apidoc-req-example" />
        </section>
      ) : null}

      <section className="adx-sec">
        <div className="h">
          <b>返回</b>
          <span>{returnsHint(e)}</span>
        </div>
        {e.media === "json" && !e.no_data ? (
          <div className="table-wrap" data-testid="apidoc-returns">
            <table className="tbl adx-tbl">
              <thead>
                <tr>
                  <th>字段</th>
                  <th>类型</th>
                  <th>说明</th>
                </tr>
              </thead>
              <tbody>
                {e.returns.map((r) => (
                  <tr key={r.name}>
                    <td data-label="字段">
                      <span className="nm">{r.name}</span>
                    </td>
                    <td data-label="类型">
                      <span className="mono">{r.type}</span>
                    </td>
                    <td data-label="说明" className="ds">
                      {r.desc}
                      {r.note ? <span className="n">{r.note}</span> : null}
                      {r.enum.length ? (
                        <span className="adx-meta">
                          {r.enum.map((v) => (
                            <code key={v}>{v}</code>
                          ))}
                        </span>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </section>

      {hasRespExample ? (
        <section className="adx-sec">
          <div className="h">
            <b>响应示例</b>
          </div>
          <JsonBlock text={JSON.stringify(e.response_example, null, 2)} testId="apidoc-resp-example" />
        </section>
      ) : null}

      {e.errors.length ? (
        <section className="adx-sec">
          <div className="h">
            <b>失败码</b>
            <span>{e.errors.length} 个</span>
          </div>
          <div className="table-wrap" data-testid="apidoc-errors">
            <table className="tbl adx-tbl">
              <thead>
                <tr>
                  <th>错误码</th>
                  <th>什么时候会撞上</th>
                </tr>
              </thead>
              <tbody>
                {e.errors.map((c) => (
                  <tr key={c.code}>
                    <td data-label="错误码">
                      <span className="nm">{c.code}</span>
                    </td>
                    <td data-label="何时" className="ds">
                      {c.when}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      ) : null}
    </div>
  );
}

/**
 * 返回区的副标题。
 *
 * 四种媒体类型各说一句人话：只写 json/csv/binary 会让接入方以为
 * 响应体一定是 JSON 信封，然后在 CSV 端点上去 `res.json()`。
 */
function returnsHint(e: ApiDocEndpoint): string {
  if (e.no_data) return "成功即完成，没有返回数据";
  switch (e.media) {
    case "json":
      return `data 内的 ${e.returns.length} 个字段`;
    case "csv":
      return "直接返回 CSV 文件（带文件名）";
    case "binary":
      return "直接返回文件内容（二进制）";
    default:
      return e.media;
  }
}

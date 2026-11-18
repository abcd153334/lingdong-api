import type { DocV7Endpoint } from "@/types/docsv7";

import { CopyBtn } from "./CopyBtn";
import { Inline } from "./Inline";
import { ParamGroups } from "./ParamGroups";
import { VariantCard } from "./ResponseCards";
import { phaseTags } from "./render";

/**
 * 接口页正文（唯一渲染实现，公开站与工作台预览共用）。
 *
 * 顺序照原型 v6：面包屑 → 标题+标签+动作 → 地址条 → 一句描述 → 注意事项（收起）
 * → 请求参数 → 返回响应 → 错误码。右栏的示例代码不在这个组件里。
 *
 * `embedded` 是工作台窄栏预览：隐藏「复制/下载 Markdown」与「调试」——
 * 草稿接口的 Markdown 接口会 404，调试则会把管理员的浏览器当成终端用户。
 */
export function EndpointDoc({
  doc,
  embedded = false,
  onTry,
}: {
  doc: DocV7Endpoint;
  embedded?: boolean;
  onTry?: () => void;
}) {
  const tags = phaseTags(doc);
  const mdUrl = `/api/docs/nodes/${encodeURIComponent(doc.node.key)}/endpoints/${encodeURIComponent(doc.key)}/markdown`;

  return (
    <article className="d-epdoc" data-testid="docs-endpoint-v7" data-doc-key={doc.key}>
      <div className="d-crumb">
        {doc.node.section_name}
        {doc.node.title ? (
          <>
            {" / "}
            <b>{doc.node.title}</b>
          </>
        ) : null}
        {" / "}
        <b>{doc.name}</b>
      </div>

      <div className="d-ep-head" data-block="head">
        <h1 className="d-ep-h1">
          {doc.name}
          {tags.map((t) => (
            <span key={t.text} className={`d-tag${t.tone ? ` ${t.tone}` : ""}`}>
              {t.text}
            </span>
          ))}
        </h1>
        {embedded ? null : (
          <span className="d-ep-actions">
            <CopyBtn
              text={async () => {
                const res = await fetch(mdUrl, { headers: { Accept: "text/markdown" } });
                if (!res.ok) throw new Error(String(res.status));
                return res.text();
              }}
              label="复制 Markdown"
              title="复制这篇接口文档的 Markdown"
            />
            <a className="d-iconbtn" href={mdUrl} download={`${doc.node.key}__${doc.key}.md`}>
              下载 .md
            </a>
          </span>
        )}
      </div>

      {doc.aliases.length > 0 ? <div className="d-alias">别名：{doc.aliases.join("、")}</div> : null}

      <div className="d-ep-line">
        <span className={`d-mth ${doc.method}`}>{doc.method}</span>
        <code className="path">{doc.path}</code>
        <CopyBtn text={doc.path} title="复制路径" />
        {embedded || !onTry ? null : (
          <button type="button" className="d-btn-try" onClick={onTry}>
            调试
          </button>
        )}
      </div>

      {/* 摘要走 Inline：族页摘要在末尾指回「视频生成通用规范」，
          按纯文本渲染的话那句话是一个点不动的方括号。 */}
      {doc.summary ? (
        <p className="d-ep-desc">
          <Inline text={doc.summary} />
        </p>
      ) : null}
      {doc.run_hint ? <p className="d-hint">{doc.run_hint}</p> : null}
      {doc.max_body ? <p className="d-hint">请求体上限 {doc.max_body}</p> : null}

      {doc.notes.length > 0 ? (
        <details className="d-fold" data-block="notes">
          <summary>
            注意事项
            <span className="d-fold-n">{doc.notes.length} 条</span>
            <span className="d-chev" />
          </summary>
          <div className="d-fold-b">
            <ul>
              {doc.notes.map((n, i) => (
                <li key={i}>
                  <Inline text={n} />
                </li>
              ))}
            </ul>
          </div>
        </details>
      ) : null}

      <div data-block="params">
        <ParamGroups fields={doc.fields} models={doc.models} />
      </div>

      <div data-block="response">
        <div className="d-sec-t">
          <h2>返回响应</h2>
          <span className="n">{doc.response.variants.length} 种</span>
        </div>
        {doc.response.variants.length === 0 ? (
          <p className="d-hint">这个接口还没有响应示例。</p>
        ) : (
          doc.response.variants.map((v, i) => (
            <VariantCard key={`${v.status}-${i}`} variant={v} first={i === 0} />
          ))
        )}
      </div>

      {doc.errors.length > 0 ? (
        <div data-block="errors">
          <div className="d-sec-t">
            <h2>错误码</h2>
            <span className="n">{doc.errors.length} 个</span>
          </div>
          <div className="d-pgroup">
            {/* 卡片是 overflow:hidden：窄屏（390）下三列表格会被裁掉第三列，
                必须在表格外面自己兜一层横向滚动，而不是让整页横向滚动。 */}
            <div className="d-tblwrap">
              <table className="d-tbl">
                <thead>
                  <tr>
                    <th style={{ width: 80 }}>状态码</th>
                    <th style={{ width: 220 }}>错误码</th>
                    <th>含义</th>
                  </tr>
                </thead>
                <tbody>
                  {doc.errors.map((e) => (
                    <tr key={`${e.status}-${e.code}`}>
                      <td>
                        <code>{e.status}</code>
                      </td>
                      <td>
                        <code>{e.code}</code>
                      </td>
                      <td>{e.desc}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      ) : null}
    </article>
  );
}

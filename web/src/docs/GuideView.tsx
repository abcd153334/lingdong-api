import type { DocV7Block, DocV7Node } from "@/types/docsv7";

import { PlainBlock } from "./code";

/**
 * 说明页正文（快速开始 / 鉴权 / 错误码）。
 *
 * 内容全部来自后端的 `blocks`（`doc_nodes.blocks`），前端不拼文案、不解析 Markdown：
 * 说明页与接口页共用同一套排版，靠的就是「后端给结构化块、前端只负责摆放」。
 *
 * 块类型只有五种，未知类型跳过而不是渲染成空段落 —— 库里手工改坏一条 block
 * 时，页面少一行比出现一个空盒子好。
 */
export function GuideView({ node }: { node: DocV7Node }) {
  const blocks = node.blocks ?? [];
  return (
    <div className="d-guide">
      <div className="d-crumb">
        {node.section_name}
        {" / "}
        <b>{node.title}</b>
      </div>
      <h1 className="d-ep-h1">{node.title}</h1>
      {blocks.length === 0 ? (
        <p className="d-hint">这篇说明还没有正文。</p>
      ) : (
        blocks.map((b, i) => <Block key={i} block={b} />)
      )}
    </div>
  );
}

function Block({ block: b }: { block: DocV7Block }) {
  switch (b.type) {
    case "para":
      return <p>{b.text}</p>;
    case "note":
      return <div className="note">{b.text}</div>;
    case "steps":
      return (
        <>
          {b.title ? <h3>{b.title}</h3> : null}
          <ol>
            {(b.items ?? []).map((t, i) => (
              <li key={i}>{t}</li>
            ))}
          </ol>
        </>
      );
    case "bullets":
      return (
        <>
          {b.title ? <h3>{b.title}</h3> : null}
          <ul>
            {(b.items ?? []).map((t, i) => (
              <li key={i}>{t}</li>
            ))}
          </ul>
        </>
      );
    case "code":
      return (
        <>
          {b.title ? <h3>{b.title}</h3> : null}
          <PlainBlock text={b.code ?? ""} />
        </>
      );
    default:
      return null;
  }
}

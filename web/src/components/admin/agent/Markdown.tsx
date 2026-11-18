import type { ReactNode } from "react";

/**
 * 极简 Markdown 渲染（只做模型回答里真实出现的那些语法）。
 *
 * 为什么自己写而不是装 marked / react-markdown：
 *  1. 这两个都要配 sanitizer（回答内容来自**上游模型**，属于不可信输入），
 *     多两个依赖 + 一条 `dangerouslySetInnerHTML` 路径，只为渲染几个标题列表不划算；
 *     本文件全程输出 React 节点，**不存在 HTML 注入面**。
 *  2. Markdown 语法子集是稳定的：标题 / 列表 / 代码块 / 表格 / 强调 / 链接，
 *     这几样覆盖了助手答复的绝大多数场景。
 *
 * 三条容错（流式渲染必须）：
 *  - 未闭合的代码围栏：按"进行中的代码块"渲染，不能吞掉后面所有内容；
 *  - 未闭合的行内标记：原样输出，不能把 `**` 当语法吃掉；
 *  - 超长行：交给 CSS 换行，不做截断。
 */

interface MdProps {
  text: string;
}

const FENCE = /^\s*```\s*([\w+#.-]*)\s*$/;
const HEADING = /^(#{1,6})\s+(.*)$/;
const UL = /^(\s*)[-*+]\s+(.*)$/;
const OL = /^(\s*)(\d+)[.)]\s+(.*)$/;
const QUOTE = /^\s*>\s?(.*)$/;
const HR = /^\s*(?:-{3,}|\*{3,}|_{3,})\s*$/;
const TABLE_SEP = /^\s*\|?[\s:|-]+\|[\s:|-]*$/;

interface CodeBlock {
  type: "code";
  lang: string;
  content: string;
  open: boolean;
}
interface ListBlock {
  type: "list";
  ordered: boolean;
  items: { depth: number; text: string }[];
}
interface TableBlock {
  type: "table";
  head: string[];
  rows: string[][];
}
type Block =
  | { type: "p"; text: string }
  | { type: "h"; level: number; text: string }
  | { type: "quote"; text: string }
  | { type: "hr" }
  | CodeBlock
  | ListBlock
  | TableBlock;

function splitRow(line: string): string[] {
  return line
    .trim()
    .replace(/^\|/, "")
    .replace(/\|$/, "")
    .split("|")
    .map((c) => c.trim());
}

function parseBlocks(src: string): Block[] {
  const lines = src.replace(/\r\n/g, "\n").split("\n");
  const blocks: Block[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];

    const fence = line.match(FENCE);
    if (fence) {
      const lang = fence[1] ?? "";
      const body: string[] = [];
      i++;
      let closed = false;
      while (i < lines.length) {
        if (/^\s*```\s*$/.test(lines[i])) {
          closed = true;
          i++;
          break;
        }
        body.push(lines[i]);
        i++;
      }
      blocks.push({ type: "code", lang, content: body.join("\n"), open: !closed });
      continue;
    }

    if (HR.test(line)) {
      blocks.push({ type: "hr" });
      i++;
      continue;
    }

    const h = line.match(HEADING);
    if (h) {
      blocks.push({ type: "h", level: h[1].length, text: h[2] });
      i++;
      continue;
    }

    // 表格：当前行含 | 且下一行是分隔行
    if (line.includes("|") && i + 1 < lines.length && TABLE_SEP.test(lines[i + 1])) {
      const head = splitRow(line);
      const rows: string[][] = [];
      i += 2;
      while (i < lines.length && lines[i].includes("|") && lines[i].trim() !== "") {
        rows.push(splitRow(lines[i]));
        i++;
      }
      blocks.push({ type: "table", head, rows });
      continue;
    }

    if (QUOTE.test(line)) {
      const buf: string[] = [];
      while (i < lines.length && QUOTE.test(lines[i])) {
        buf.push((lines[i].match(QUOTE)?.[1] ?? "").trim());
        i++;
      }
      blocks.push({ type: "quote", text: buf.join(" ") });
      continue;
    }

    const ul = line.match(UL);
    const ol = line.match(OL);
    if (ul || ol) {
      const ordered = Boolean(ol);
      const items: { depth: number; text: string }[] = [];
      while (i < lines.length) {
        const u = lines[i].match(UL);
        const o = lines[i].match(OL);
        if (!u && !o) break;
        // 有序与无序混排时以"列表开始时的类型"为准：切换类型说明是新列表，
        // 但把两个列表连成一个也不会错得离谱（比裂成两段更耐看）。
        const m = u ?? o!;
        const indent = Math.floor((m[1]?.length ?? 0) / 2);
        items.push({ depth: Math.min(indent, 3), text: (u ? u[2] : o![3]) ?? "" });
        i++;
      }
      blocks.push({ type: "list", ordered, items });
      continue;
    }

    if (line.trim() === "") {
      i++;
      continue;
    }

    // 段落：连续的非空行合成一段（软换行保留为 <br/>，模型常按行断句）
    const buf: string[] = [];
    while (
      i < lines.length &&
      lines[i].trim() !== "" &&
      !FENCE.test(lines[i]) &&
      !HEADING.test(lines[i]) &&
      !UL.test(lines[i]) &&
      !OL.test(lines[i]) &&
      !QUOTE.test(lines[i]) &&
      !HR.test(lines[i])
    ) {
      buf.push(lines[i]);
      i++;
    }
    blocks.push({ type: "p", text: buf.join("\n") });
  }
  return blocks;
}

const INLINE =
  /(`[^`]+`|\*\*[^*]+\*\*|__[^_]+__|\[[^\]]+\]\([^)\s]+\)|https?:\/\/[^\s<>()]+|\*[^*\s][^*]*\*)/g;

function safeHref(url: string): string | null {
  const u = url.trim();
  if (/^https?:\/\//i.test(u) || /^mailto:/i.test(u)) return u;
  return null;
}

/** 行内标记 → React 节点（不拼 HTML 字符串）。 */
function inline(text: string, keyBase: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let idx = 0;
  for (const m of text.matchAll(INLINE)) {
    const start = m.index ?? 0;
    if (start > last) out.push(text.slice(last, start));
    const tok = m[0];
    const key = `${keyBase}-${idx++}`;
    if (tok.startsWith("`") && tok.endsWith("`") && tok.length > 2) {
      out.push(
        <code key={key} className="agent-md__code">
          {tok.slice(1, -1)}
        </code>,
      );
    } else if ((tok.startsWith("**") && tok.endsWith("**")) || (tok.startsWith("__") && tok.endsWith("__"))) {
      out.push(<strong key={key}>{tok.slice(2, -2)}</strong>);
    } else if (tok.startsWith("[")) {
      const mm = tok.match(/^\[([^\]]+)\]\(([^)\s]+)\)$/);
      const href = mm ? safeHref(mm[2]) : null;
      if (mm && href) {
        out.push(
          <a key={key} href={href} target="_blank" rel="noopener noreferrer">
            {mm[1]}
          </a>,
        );
      } else {
        // 不支持的协议（javascript: / file: …）：只渲染文字，**不把 URL 原样吐出来**
        // ——一段 javascript: 字样在界面上既没用，又容易被当成"可以点"。
        out.push(<span key={key}>{mm ? mm[1] : tok}</span>);
      }
    } else if (tok.startsWith("http")) {
      out.push(
        <a key={key} href={tok} target="_blank" rel="noopener noreferrer">
          {tok}
        </a>,
      );
    } else if (tok.startsWith("*") && tok.endsWith("*") && tok.length > 2) {
      out.push(<em key={key}>{tok.slice(1, -1)}</em>);
    } else {
      out.push(tok);
    }
    last = start + tok.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

/** 段落内的软换行 → <br/> */
function withBreaks(text: string, keyBase: string): ReactNode[] {
  const parts = text.split("\n");
  const out: ReactNode[] = [];
  parts.forEach((p, i) => {
    if (i > 0) out.push(<br key={`${keyBase}-br${i}`} />);
    out.push(...inline(p, `${keyBase}-${i}`));
  });
  return out;
}

export function Markdown({ text }: MdProps) {
  const blocks = parseBlocks(text);
  return (
    <div className="agent-md">
      {blocks.map((b, i) => {
        switch (b.type) {
          case "code":
            return (
              <pre key={i} className="agent-md__pre" data-open={b.open ? "1" : undefined}>
                {b.lang ? <span className="agent-md__lang">{b.lang}</span> : null}
                <code>{b.content}</code>
              </pre>
            );
          case "h": {
            // 页面 h1 是隐藏的页标题，消息里的标题从 h3 起，避免打乱文档大纲。
            const Tag = (b.level <= 2 ? "h3" : "h4") as "h3" | "h4";
            return (
              <Tag key={i} className="agent-md__h">
                {inline(b.text, `h${i}`)}
              </Tag>
            );
          }
          case "list": {
            const Tag = b.ordered ? "ol" : "ul";
            return (
              <Tag key={i} className="agent-md__list">
                {b.items.map((it, j) => (
                  <li key={j} data-depth={it.depth || undefined}>
                    {inline(it.text, `l${i}-${j}`)}
                  </li>
                ))}
              </Tag>
            );
          }
          case "table":
            return (
              <div key={i} className="agent-md__tablewrap">
                <table className="agent-md__table">
                  <thead>
                    <tr>
                      {b.head.map((h, j) => (
                        <th key={j}>{inline(h, `th${i}-${j}`)}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {b.rows.map((r, j) => (
                      <tr key={j}>
                        {r.map((c, k) => (
                          <td key={k}>{inline(c, `td${i}-${j}-${k}`)}</td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            );
          case "quote":
            return (
              <blockquote key={i} className="agent-md__quote">
                {inline(b.text, `q${i}`)}
              </blockquote>
            );
          case "hr":
            return <hr key={i} className="agent-md__hr" />;
          default:
            return (
              <p key={i} className="agent-md__p">
                {withBreaks(b.text, `p${i}`)}
              </p>
            );
        }
      })}
    </div>
  );
}

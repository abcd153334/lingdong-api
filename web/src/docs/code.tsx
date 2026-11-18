import type { ReactNode } from "react";

/**
 * 代码块与 JSON 高亮。
 *
 * 原型用 `innerHTML` 拼 span，这里改成**分词后建 React 节点**：内容来自后端
 * （可能有用户写的字段说明），拼字符串就有注入面。
 *
 * 配色与原型一致：键 `k`、字符串 `s`、数字/布尔 `n`、注释 `c`。
 */

const TOKEN = /("(?:\\.|[^"\\])*")(\s*:)?|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)|\b(true|false|null)\b/g;

/** 把一段 JSON 文本切成带语义的节点（不包 <pre>，调用方决定容器）。 */
export function jsonParts(text: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let key = 0;
  for (const m of text.matchAll(TOKEN)) {
    const at = m.index ?? 0;
    if (at > last) out.push(text.slice(last, at));
    if (m[1] !== undefined) {
      // 字符串：后面紧跟冒号的是键
      const isKey = m[2] !== undefined;
      out.push(
        <span key={key++} className={isKey ? "k" : "s"}>
          {m[1]}
        </span>,
      );
      if (m[2] !== undefined) out.push(m[2]);
    } else if (m[3] !== undefined) {
      out.push(
        <span key={key++} className="n">
          {m[3]}
        </span>,
      );
    } else {
      out.push(
        <span key={key++} className="n">
          {m[0]}
        </span>,
      );
    }
    last = at + m[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

/** JSON 值 → 代码块（对象/数组自动缩进）。 */
export function JsonBlock({ value }: { value: unknown }) {
  const text = stringify(value);
  return <pre className="d-code">{jsonParts(text)}</pre>;
}

/** 纯文本 → 代码块（curl / python / node / go，原型不做语法高亮）。 */
export function PlainBlock({ text }: { text: string }) {
  return <pre className="d-code">{text || "// 无示例"}</pre>;
}

/**
 * 稳定的 JSON 文本：键顺序保持后端下发顺序（后端按契约声明顺序输出），
 * 不做排序 —— 排序会让示例与文档里的字段顺序对不上。
 */
export function stringify(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2) ?? String(v);
  } catch {
    return String(v);
  }
}

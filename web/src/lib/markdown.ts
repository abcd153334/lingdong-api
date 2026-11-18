/**
 * 公告 / 富文本的极简 Markdown 渲染（先转义再插标签，杜绝 XSS）。
 * 支持：标题 # ## ###、粗体 **、行内代码 `、链接 [文案](http…) 、无序列表、引用、代码块、空行分段。
 * 与原型 mdRender 的输出结构对齐（块级类名 nt-h1/nt-h2/nt-h3，样式由 .nt-md 收口）。
 */

const ESC: Record<string, string> = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
};

function escapeHtml(text: string): string {
  return text.replace(/[&<>"']/g, (c) => ESC[c]);
}

/** 行内语法渲染（折叠态摘要用：单行、不产生块级标签） */
export function renderInline(text: string): string {
  return escapeHtml(text)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(
      /\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g,
      '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>',
    );
}

/** 块级渲染（详情态正文用） */
export function renderMarkdown(text: string): string {
  const out: string[] = [];
  let list: string[] | null = null;
  let code: string[] | null = null;
  let para: string[] = [];

  const flushPara = (): void => {
    if (para.length) {
      out.push(`<p>${renderInline(para.join("\n"))}</p>`);
      para = [];
    }
  };
  const flushList = (): void => {
    if (list) {
      out.push(`<ul>${list.join("")}</ul>`);
      list = null;
    }
  };
  const flushAll = (): void => {
    flushPara();
    flushList();
  };

  for (const raw of text.replace(/\r\n?/g, "\n").split("\n")) {
    if (code) {
      if (raw.startsWith("```")) {
        out.push(`<pre><code>${escapeHtml(code.join("\n"))}</code></pre>`);
        code = null;
      } else {
        code.push(raw);
      }
      continue;
    }
    if (raw.startsWith("```")) {
      flushAll();
      code = [];
      continue;
    }
    if (raw.trim() === "") {
      flushAll();
      continue;
    }
    const head = /^(#{1,3})\s+(.*)$/.exec(raw);
    if (head) {
      flushAll();
      out.push(`<div class="nt-h${head[1].length}">${renderInline(head[2])}</div>`);
      continue;
    }
    const quote = /^>\s?(.*)$/.exec(raw);
    if (quote) {
      flushAll();
      out.push(`<blockquote>${renderInline(quote[1])}</blockquote>`);
      continue;
    }
    const item = /^[-*]\s+(.*)$/.exec(raw);
    if (item) {
      flushPara();
      list = list ?? [];
      list.push(`<li>${renderInline(item[1])}</li>`);
      continue;
    }
    flushList();
    para.push(raw);
  }

  flushPara();
  flushList();
  if (code) out.push(`<pre><code>${escapeHtml(code.join("\n"))}</code></pre>`);
  return out.join("");
}

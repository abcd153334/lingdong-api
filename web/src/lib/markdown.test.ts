import { describe, expect, it } from "vitest";
import { renderInline, renderMarkdown } from "./markdown";

/**
 * 公告/富文本是**管理员可编辑、所有用户可见**的内容，渲染结果会被直接塞进
 * dangerouslySetInnerHTML。这份用例锁死「先转义再插标签」这条唯一底线：
 * 任何用户可控文本都不得变成标签或属性。
 */
describe("markdown 渲染的 XSS 边界", () => {
  it("标签被转义成文本，不会真的执行", () => {
    const html = renderInline('<img src=x onerror="alert(1)">');
    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;img");
  });

  it("禁止 javascript: 协议（链接语法只认 http/https）", () => {
    const html = renderInline("[点我](javascript:alert(1))");
    expect(html).not.toContain("<a ");
    expect(html).toContain("javascript:alert(1)");
  });

  it("data: 协议同样不算链接", () => {
    const html = renderInline("[x](data:text/html,<script>alert(1)</script>)");
    expect(html).not.toContain("<a ");
  });

  it("属性注入被引号转义挡住", () => {
    const html = renderInline('[x](https://a.com" onmouseover="alert(1))');
    expect(html).not.toContain('onmouseover="alert');
    expect(html).toContain("&quot;");
  });

  it("合法链接仍要能渲染成可点的新窗口链接", () => {
    const html = renderInline("[文档](https://example.com/a?b=1)");
    expect(html).toContain('<a href="https://example.com/a?b=1"');
    expect(html).toContain('rel="noopener noreferrer"');
  });

  it("代码块内的内容一律按纯文本处理", () => {
    const html = renderMarkdown("```\n<script>alert(1)</script>\n```");
    expect(html).toContain("&lt;script&gt;");
    expect(html).not.toContain("<script>");
  });

  it("块级元素里的注入同样被转义", () => {
    const html = renderMarkdown("# <b>x</b>\n\n> <i>y</i>\n\n- <u>z</u>");
    expect(html).not.toContain("<b>");
    expect(html).not.toContain("<i>");
    expect(html).not.toContain("<u>");
  });
});

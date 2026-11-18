import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { Markdown } from "@/components/admin/agent/Markdown";

function html(text: string): string {
  const { container } = render(<Markdown text={text} />);
  return container.innerHTML;
}

describe("Markdown", () => {
  it("渲染标题、列表与粗体", () => {
    const out = html("## 标题\n\n- 第一项\n- **重点**项");
    expect(out).toContain("<h3");
    expect(out).toContain("标题");
    expect(out).toContain("<ul");
    expect(out).toContain("<strong>重点</strong>");
  });

  it("行内代码与围栏代码块", () => {
    const out = html("用 `adapter_id` 指定\n\n```json\n{\"a\":1}\n```");
    expect(out).toContain('class="agent-md__code"');
    expect(out).toContain("<pre");
    expect(out).toContain("json");
  });

  it("未闭合的代码围栏按进行中的代码块渲染（不吞掉后面所有内容）", () => {
    const out = html("说明：\n\n```go\nfunc main() {\n");
    expect(out).toContain("<pre");
    expect(out).toContain("func main()");
  });

  it("原始 HTML 不被当作标签执行", () => {
    const out = html('<img src=x onerror="alert(1)"> 与 <script>alert(1)</script>');
    expect(out).not.toContain("<img");
    expect(out).not.toContain("<script");
    // 作为文本原样出现（转义后是 &lt;）
    expect(out).toContain("&lt;script&gt;");
  });

  it("javascript: 伪协议链接退化成纯文本", () => {
    const out = html("[点我](javascript:alert(1))");
    expect(out).not.toContain("javascript:alert");
    expect(out).toContain("点我");
  });

  it("http 链接带 rel/target", () => {
    const out = html("[文档](https://example.com/doc)");
    expect(out).toContain('href="https://example.com/doc"');
    expect(out).toContain('rel="noopener noreferrer"');
    expect(out).toContain('target="_blank"');
  });

  it("GFM 表格渲染成 table", () => {
    const out = html("| 档位 | 价格 |\n| --- | --- |\n| 720p | 0.3 |");
    expect(out).toContain("<table");
    expect(out).toContain("<th");
    expect(out).toContain("720p");
  });

  it("引用与分隔线", () => {
    const out = html("> 注意这里\n\n---");
    expect(out).toContain("<blockquote");
    expect(out).toContain("<hr");
  });

  it("段落内软换行保留为 <br>", () => {
    const out = html("第一行\n第二行");
    expect(out).toContain("<br");
    expect(out).toContain("第二行");
  });
});

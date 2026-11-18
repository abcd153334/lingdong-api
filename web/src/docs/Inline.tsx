/**
 * 行内标记：反引号 → `<code>`，`**粗体**` → `<strong>`，站内链接 → `<a>`。
 *
 * 为什么需要：字段说明与注意事项里大量出现 `async: true`、`**不支持**` 这类写法，
 * 而 v7 的渲染一直是纯文本 —— 页面上原样显示成带反引号、带星号的句子。
 * 读者能看懂，但它把那句「这句是规则」的语气弄丢了，长段落里尤其明显。
 *
 * 为什么只支持这三种：文档需要的是「这个键要原样写」「这句是重点」「点这里去那篇」，
 * 不是一套 Markdown。引入完整解析器会带进表格、图片、转义一整串边界，
 * 而正文里的表格有专门的渲染块（.d-tbl / 分组 rows），不该由内联解析兼职。
 *
 * 链接**只放行站内路径**（`/docs/` 与 `/api/docs/`）：外部 URL 一律按纯文本显示。
 * 文档正文里出现可点击外链，等于给"把读者带去别处"开了一个不受审的通道，
 * 而发布闸门的 urlPattern 只管 http(s) 字面量——它拦不住 Markdown 链接语法。
 *
 * 不配对的标记保持原样：宁可露出一个反引号，也不要把后半句吞掉。
 */
export function Inline({ text }: { text: string }) {
  const parts = text.split(/(`[^`\n]+`|\*\*[^*\n]+\*\*|\[[^\]\n]+\]\(\/[^)\s]+\))/g);
  return (
    <>
      {parts.map((p, i) => {
        if (p.length > 2 && p.startsWith("`") && p.endsWith("`")) {
          return <code key={i}>{p.slice(1, -1)}</code>;
        }
        if (p.length > 4 && p.startsWith("**") && p.endsWith("**")) {
          return <strong key={i}>{p.slice(2, -2)}</strong>;
        }
        const link = /^\[([^\]\n]+)\]\((\/[^)\s]+)\)$/.exec(p);
        if (link && (link[2].startsWith("/docs/") || link[2].startsWith("/api/docs/"))) {
          return (
            <a key={i} href={link[2]}>
              {link[1]}
            </a>
          );
        }
        return <span key={i}>{p}</span>;
      })}
    </>
  );
}

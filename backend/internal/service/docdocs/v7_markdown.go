package docdocs

import (
	"encoding/json"
	"fmt"
	"strings"

	"lingdong-api/internal/model"
)

// 本文件是**对外文档站的 Markdown 全量导出**（`GET /api/docs/markdown`）。
//
// 三条约束：
//  1. **只读投影**：所有内容来自 `ProjectCatalog` / `ProjectNode` / `ProjectEndpoint`，
//     与页面逐字同源。任何"导出时单独拼一句说明"的写法都会让同一天的两份文档
//     出现两种措辞，而两边都自称是同一份契约。
//  2. **只导出已发布**：草稿是内部状态，导出的文件会被贴进邮件、工单、知识库，
//     草稿一旦漏出去就等于发布，而且再也没法收回。
//  3. **输出必须确定**：同一份数据两次导出必须逐字节相同 —— 响应带 ETag，
//     内容顺序随 map 迭代漂移会让每次请求都变成"已更新"。
//     所以所有遍历都按已经排好序的目录走，JSON 也用 `MarshalIndent`（键有序）。

// RenderMarkdown 导出全站 Markdown。
//
// 两个 base 不能合并：
//   - apiBase 进**示例代码**（`curl {apiBase}/v1/...`），必须是 API 入口地址；
//   - siteBase 进**目录链接**（`{siteBase}/docs/{node}/{ep}`），必须是门户地址。
//
// `siteurl.Base` 按包契约只认 API 地址、不回落站点地址；拿它拼站内链接
// 会把读者送到 API 域名下的 404。所以调用方必须分别传。
func RenderMarkdown(apiBase, siteBase string) (string, error) {
	t, err := LoadTree()
	if err != nil {
		return "", err
	}
	apiBase = trimSlash(apiBase)
	siteBase = trimSlash(siteBase)
	cat, err := ProjectCatalog(t, apiBase)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# API 文档\n\n")
	fmt.Fprintf(&b, "> 契约版本 %s · 共 %d 个接口\n\n", cat.ContractVersion, countEndpoints(cat))
	if siteBase != "" {
		fmt.Fprintf(&b, "在线文档：%s/docs\n\n", siteBase)
	}

	writeMarkdownToc(&b, cat, siteBase)
	ms := loadModelIndex()
	for _, sec := range cat.Sections {
		writeSection(&b, sec, apiBase, ms)
	}
	return b.String(), nil
}

// RenderEndpointMarkdown 导出**单篇**接口文档（页面上的「复制 / 下载 Markdown」）。
//
// 与全站导出共用同一套 writer：两处各拼一份，复制出来的和页面上的必然不同 ——
// 而读者正是拿着复制出来的那份去写代码。
func RenderEndpointMarkdown(nodeKey, epKey, apiBase, siteBase string) (string, error) {
	eb, err := LoadEndpoint(nodeKey, epKey)
	if err != nil {
		return "", ErrDocNotFound
	}
	if !visibleEndpoint(eb.Node, eb.EP) {
		return "", ErrDocNotFound
	}
	var b strings.Builder
	doc := projectEndpoint(eb, trimSlash(apiBase), loadModelIndex())
	fmt.Fprintf(&b, "# %s\n\n", doc.Name)
	if siteBase = trimSlash(siteBase); siteBase != "" {
		fmt.Fprintf(&b, "> 在线查看：%s/docs/%s/%s\n\n", siteBase, doc.Node.Key, doc.Key)
	}
	writeEndpointBody(&b, doc)
	return b.String(), nil
}

// countEndpoints 统计目录里的接口总数（只用于首段概览）。
func countEndpoints(cat Catalog) int {
	n := 0
	for _, s := range cat.Sections {
		for _, node := range s.Nodes {
			n += len(node.Endpoints)
		}
	}
	return n
}

// writeMarkdownToc 生成目录。
//
// 用**在线链接**而不是页内锚点：中文标题的锚点规则各家渲染器不一致
// （GitHub 保留下划线、GitLab 去掉、VS Code 预览又不同），
// 而这份文件最常见的用法是「贴到聊天/工单里让人点开」。
func writeMarkdownToc(b *strings.Builder, cat Catalog, siteBase string) {
	b.WriteString("## 目录\n\n")
	for _, s := range cat.Sections {
		if len(s.Nodes) == 0 {
			continue
		}
		fmt.Fprintf(b, "- **%s**\n", s.Name)
		for _, n := range s.Nodes {
			// 链接形态必须与前端路由逐字一致：`/docs/{node}` 与 `/docs/{node}/{ep}`。
			// 旧写法 `/docs/nodes/{key}` 会被 SPA 解析成 nodeKey="nodes" → 404。
			if siteBase != "" {
				fmt.Fprintf(b, "  - [%s](%s/docs/%s)\n", n.Title, siteBase, n.Key)
			} else {
				fmt.Fprintf(b, "  - %s\n", n.Title)
			}
			for _, e := range n.Endpoints {
				if siteBase != "" {
					fmt.Fprintf(b, "    - [%s](%s/docs/%s/%s)\n", e.Name, siteBase, n.Key, e.Key)
				} else {
					fmt.Fprintf(b, "    - %s\n", e.Name)
				}
			}
		}
	}
	b.WriteString("\n")
}

func writeSection(b *strings.Builder, sec CatalogSection, apiBase string, ms modelIndex) {
	if len(sec.Nodes) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n", sec.Name)
	if sec.Desc != "" {
		fmt.Fprintf(b, "%s\n\n", sec.Desc)
	}
	for _, node := range sec.Nodes {
		nb, err := LoadNode(node.Key)
		if err != nil {
			// 目录刚读出来、节点就没了（并发删除）：跳过而不是让整份导出 500。
			// 少一节比拿不到文件好，而且下次请求就会自愈。
			continue
		}
		writeNode(b, nb, node, apiBase, ms)
	}
}

func writeNode(b *strings.Builder, nb *NodeBundle, cn CatalogNode, apiBase string, ms modelIndex) {
	n := projectNode(nb, apiBase, ms)
	if n.Title == "" {
		return
	}
	fmt.Fprintf(b, "### %s\n\n", n.Title)
	if n.Summary != "" {
		fmt.Fprintf(b, "%s\n\n", n.Summary)
	}
	if len(n.Models) > 0 {
		names := make([]string, 0, len(n.Models))
		for _, m := range n.Models {
			names = append(names, m.Name)
		}
		fmt.Fprintf(b, "适用模型：%s\n\n", strings.Join(names, "、"))
	}
	// 说明页：正文块本身就是这一页的全部内容，没有接口。
	if n.Kind == model.DocNodeGuide {
		writeGuideBlocks(b, n.Blocks)
		return
	}

	for _, ep := range cn.Endpoints {
		eb, err := LoadEndpoint(n.Key, ep.Key)
		if err != nil {
			continue
		}
		if !visibleEndpoint(eb.Node, eb.EP) {
			continue
		}
		writeEndpoint(b, projectEndpoint(eb, apiBase, ms))
	}
}

// writeGuideBlocks 渲染说明页的正文块（结构见 apidoc.GuideBlock）。
func writeGuideBlocks(b *strings.Builder, blocks []GuideBlockView) {
	for _, blk := range blocks {
		switch blk.Type {
		case "para":
			fmt.Fprintf(b, "%s\n\n", blk.Text)
		case "note":
			fmt.Fprintf(b, "> %s\n\n", blk.Text)
		case "steps", "bullets":
			for i, it := range blk.Items {
				if blk.Type == "steps" {
					fmt.Fprintf(b, "%d. %s\n", i+1, it)
				} else {
					fmt.Fprintf(b, "- %s\n", it)
				}
			}
			b.WriteString("\n")
		case "code":
			lang := blk.Lang
			if lang == "" {
				lang = "text"
			}
			fmt.Fprintf(b, "```%s\n%s\n```\n\n", lang, strings.TrimRight(blk.Code, "\n"))
		}
	}
}

func writeEndpoint(b *strings.Builder, ep Endpoint) {
	fmt.Fprintf(b, "#### %s\n\n", ep.Name)
	writeEndpointBody(b, ep)
}

// writeEndpointBody 是不带标题的正文（全站导出用 `####` 标题，单篇导出用 `#`）。
func writeEndpointBody(b *strings.Builder, ep Endpoint) {
	if ep.Summary != "" {
		fmt.Fprintf(b, "%s\n\n", ep.Summary)
	}
	// 方法与路径：读者最常复制的一行，单独成行并放进代码块（避免 Markdown 吃掉大括号）。
	fmt.Fprintf(b, "`%s %s`\n\n", ep.Method, ep.Path)
	if len(ep.Aliases) > 0 {
		fmt.Fprintf(b, "别名：%s\n\n", strings.Join(ep.Aliases, "、"))
	}
	if ep.RunHint != "" {
		fmt.Fprintf(b, "> %s\n\n", ep.RunHint)
	}
	if ep.MaxBody != "" {
		fmt.Fprintf(b, "请求体上限：%s\n\n", ep.MaxBody)
	}
	// 接口级注意事项（计费口径 / 超时 / 409 语义）：页面上在标题下方，这里逐条列。
	for _, n := range ep.Notes {
		fmt.Fprintf(b, "- %s\n", n)
	}
	if len(ep.Notes) > 0 {
		b.WriteString("\n")
	}

	writeFieldsTable(b, "请求参数", ep.Fields)
	writeResponse(b, ep)
	writeExamples(b, ep)
	writeErrors(b, ep)
}

// writeFieldsTable 输出一张参数表；空字段表整节略过（不写"无参数"占位）。
func writeFieldsTable(b *strings.Builder, title string, fields []FieldView) {
	if len(fields) == 0 {
		return
	}
	fmt.Fprintf(b, "**%s**\n\n", title)
	b.WriteString("| 名称 | 类型 | 位置 | 必填 | 来源 | 说明 |\n|---|---|---|---|---|---|\n")
	for _, f := range fields {
		desc := f.Desc
		if len(f.Enum) > 0 {
			desc = strings.TrimSpace(desc + " " + enumText(f.Enum))
		}
		if f.Default != "" {
			desc = strings.TrimSpace(desc + " 默认 " + f.Default + "。")
		}
		if f.Passthrough {
			desc = strings.TrimSpace(desc + " 平台按原样转发，不校验。")
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s | %s |\n",
			mdCell(f.Name), mdCell(f.Type), mdCell(f.In),
			mdCell(requiredText(f.Required, f.RequiredNote)),
			mdCell(tierText(f.Tier)), mdCell(desc))
	}
	b.WriteString("\n")
}

// tierText 是来源列的两字写法。
//
// 为什么 Markdown 要有这一列：页面上的 chip 靠 hover 与图例理解，而 Markdown
// 是 AI 与工单的消费面 —— 没有 hover、没有图例位置，缺列等于这条信息永久丢失。
// 用两字而不是四字：一行里已经有五种信息，长标签会把表压到折行。
func tierText(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "p":
		return "平台"
	case "s", "standard":
		return "标准"
	case "v":
		return "厂商"
	case "n":
		return "兼容"
	}
	return "—"
}

// enumText 把枚举写成「取值：a / b / c」。
func enumText(vals []EnumView) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, v.Value)
	}
	return "取值：" + strings.Join(parts, " / ") + "。"
}

// requiredText 把契约里的三态翻成人话。
func requiredText(req, note string) string {
	switch req {
	case "required":
		return "是"
	case "conditional":
		if note != "" {
			return "条件（" + note + "）"
		}
		return "条件"
	default:
		return "否"
	}
}

func writeResponse(b *strings.Builder, ep Endpoint) {
	if len(ep.Response.Variants) == 0 {
		return
	}
	b.WriteString("**响应**\n\n")
	if ep.Response.ContentType != "" {
		fmt.Fprintf(b, "响应类型：`%s`\n\n", ep.Response.ContentType)
	}
	for _, v := range ep.Response.Variants {
		label := strings.TrimSpace(fmt.Sprintf("%d %s", v.Status, v.Label))
		fmt.Fprintf(b, "**%s**\n\n", label)
		if v.ContentType != "" {
			fmt.Fprintf(b, "响应类型：`%s`\n\n", v.ContentType)
		}
		for _, n := range v.Notes {
			fmt.Fprintf(b, "- %s\n", n)
		}
		if len(v.Notes) > 0 {
			b.WriteString("\n")
		}
		writeFieldsTable(b, "返回字段", v.Returns)
		if v.Raw != "" {
			fmt.Fprintf(b, "```text\n%s\n```\n\n", strings.TrimRight(v.Raw, "\n"))
		}
		writeJSONBlock(b, v.Example)
	}
}

func writeExamples(b *strings.Builder, ep Endpoint) {
	if ep.Samples == nil {
		return
	}
	b.WriteString("**示例代码**\n\n")
	writeCodeBlock(b, "bash", ep.Samples.Curl)
	writeCodeBlock(b, "python", ep.Samples.Python)
	writeCodeBlock(b, "javascript", ep.Samples.Node)
	writeCodeBlock(b, "go", ep.Samples.Go)
	if ep.Samples2 != nil {
		title := ep.ExampleTitle2
		if title == "" {
			title = "另一种写法"
		}
		fmt.Fprintf(b, "**%s**\n\n", title)
		writeCodeBlock(b, "bash", ep.Samples2.Curl)
	}
}

func writeErrors(b *strings.Builder, ep Endpoint) {
	if len(ep.Errors) == 0 {
		return
	}
	b.WriteString("**错误码**\n\n")
	b.WriteString("| 状态码 | 错误码 | 说明 |\n|---|---|---|\n")
	for _, e := range ep.Errors {
		fmt.Fprintf(b, "| %d | `%s` | %s |\n", e.Status, mdCell(e.Code), mdCell(e.Desc))
	}
	b.WriteString("\n")
}

func writeCodeBlock(b *strings.Builder, lang, code string) {
	code = strings.TrimRight(code, "\n")
	if code == "" {
		return
	}
	fmt.Fprintf(b, "```%s\n%s\n```\n\n", lang, code)
}

func writeJSONBlock(b *strings.Builder, v any) {
	if v == nil {
		return
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	fmt.Fprintf(b, "```json\n%s\n```\n\n", raw)
}

// mdCell 转义表格单元格：`|` 会截断列，换行会让整张表错位。
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", "<br>")
	s = strings.ReplaceAll(s, "\n", "<br>")
	return s
}

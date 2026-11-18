package apicontract

// 本文件把契约渲染成**给人读的 Markdown 手册**。
//
// 与 OpenAPI 同源（同一份 catalog + details），区别是消费对象：
//   - OpenAPI 给工具读（Swagger UI、SDK 生成器、网关导入）；
//   - Markdown 给人读（可粘进群里、可贴进 wiki、可 diff）。
//
// 为什么不全量罗列字段：几十条端点的参数与返回加起来上千行，粘进聊天窗口
// 没人会读完。超过 12 条时截断并注明"完整字段见 OpenAPI" —— 这个上限是
// 刻意的：**能一眼扫完**比**完整但不看**有用。
//
// 为什么从 openapi.go 搬出来：那个文件已经有 500 行、还在继续长，
// 两个渲染器混在一起时，"改 OpenAPI 的 schema 形状"与"改手册的排版"
// 会互相干扰 review。

import (
	"strings"
)

// markdownFieldLimit 是每个端点字段表的最大行数（超出则提示看 OpenAPI）。
const markdownFieldLimit = 12

// Markdown 渲染接入方自阅的 Markdown 手册。
func Markdown() string {
	var b strings.Builder
	b.WriteString("# 灵动开放接口\n\n")
	b.WriteString("所有接口返回值统一为 `{code,message,error_code,data}`，`code=0` 表示成功；" +
		"失败时 `error_code` 用于分支判断。\n\n")
	b.WriteString("鉴权：`Authorization: Bearer <凭证>`。凭证有两种 —— 会话票（用户登录后下发，`lds_` 前缀）" +
		"与 API 密钥（`ld-` 前缀）。写操作只接受会话票，或带对应权限点的应用托管密钥。\n\n")
	b.WriteString("鉴权级别：`open` 无需凭证；`reader` 会话票或任意密钥；`writer` 会话票或带 scope 的托管密钥；" +
		"`session` 仅会话票。\n\n")
	b.WriteString("> `reader` 与 `session` 级别当前**不校验** scope：权限点只在 `writer` 上生效。\n\n")

	for _, g := range Groups() {
		b.WriteString("## ")
		b.WriteString(g)
		b.WriteString("\n\n")
		for _, e := range catalog {
			if e.Group != g {
				continue
			}
			writeEndpointMarkdown(&b, e)
		}
	}

	b.WriteString("## 不开放的能力\n\n")
	for _, n := range notExposed {
		b.WriteString("- **")
		b.WriteString(n.Title)
		b.WriteString("**")
		if n.Path != "" {
			b.WriteString("（`")
			b.WriteString(n.Method)
			b.WriteString(" ")
			b.WriteString(n.Path)
			b.WriteString("`）")
		}
		b.WriteString("：")
		b.WriteString(n.Reason)
		b.WriteString("\n")
	}
	return b.String()
}

// writeEndpointMarkdown 渲染单个端点。
func writeEndpointMarkdown(b *strings.Builder, e Endpoint) {
	d := e.Doc()
	b.WriteString("### ")
	b.WriteString(e.Title)
	b.WriteString("\n\n")
	b.WriteString("- `")
	b.WriteString(e.Method)
	b.WriteString(" ")
	b.WriteString(e.Path)
	b.WriteString("`\n")
	b.WriteString("- 鉴权：`")
	b.WriteString(e.Auth.String())
	b.WriteString("`")
	if e.Scope != "" {
		b.WriteString("，权限点：`")
		b.WriteString(e.Scope)
		b.WriteString("`")
		if !d.ScopeEnforced {
			b.WriteString("（当前不校验）")
		}
	}
	b.WriteString("\n")
	if e.Media != MediaJSON {
		b.WriteString("- 响应体：`")
		b.WriteString(string(e.Media))
		b.WriteString("`（不是 JSON）\n")
	}
	if e.Idempotency != "" {
		b.WriteString("- 幂等：")
		b.WriteString(e.Idempotency)
		b.WriteString("\n")
	}
	if e.Notes != "" {
		b.WriteString("- 注意：")
		b.WriteString(e.Notes)
		b.WriteString("\n")
	}
	if !e.Tries() {
		b.WriteString("- 不可在此调试：")
		b.WriteString(e.TryHint())
		b.WriteString("\n")
	}
	b.WriteString("\n")

	writeParamTable(b, d.Params)
	writeReturnTable(b, d)
}

// writeParamTable 渲染入参表。
func writeParamTable(b *strings.Builder, params []Param) {
	if len(params) == 0 {
		return
	}
	b.WriteString("| 字段 | 位置 | 必填 | 说明 |\n| --- | --- | --- | --- |\n")
	for i, p := range params {
		if i >= markdownFieldLimit {
			b.WriteString("| … | … | … | 其余 ")
			b.WriteString(itoa(len(params) - markdownFieldLimit))
			b.WriteString(" 个字段见 OpenAPI |\n")
			break
		}
		b.WriteString("| `")
		b.WriteString(p.Name)
		b.WriteString("` | ")
		b.WriteString(string(p.In))
		b.WriteString(" | ")
		b.WriteString(requiredText(p))
		b.WriteString(" | ")
		b.WriteString(tableCell(joinDesc(p.Desc, p.Note)))
		b.WriteString(" |\n")
	}
	b.WriteString("\n")
}

// writeReturnTable 渲染返回字段表。
func writeReturnTable(b *strings.Builder, d Detail) {
	if d.NoData {
		b.WriteString("成功时 `data` 为 `null`，无需解析。\n\n")
		return
	}
	if len(d.Returns) == 0 {
		return
	}
	b.WriteString("| 返回字段 | 类型 | 说明 |\n| --- | --- | --- |\n")
	for i, r := range d.Returns {
		if i >= markdownFieldLimit {
			b.WriteString("| … | … | 其余 ")
			b.WriteString(itoa(len(d.Returns) - markdownFieldLimit))
			b.WriteString(" 个字段见 OpenAPI |\n")
			break
		}
		b.WriteString("| `")
		b.WriteString(r.Name)
		b.WriteString("` | ")
		b.WriteString(string(r.Type))
		b.WriteString(" | ")
		b.WriteString(tableCell(joinDesc(r.Desc, r.Note)))
		b.WriteString(" |\n")
	}
	b.WriteString("\n")
}

// requiredText 把三态必填渲染成人话。
func requiredText(p Param) string {
	switch p.Required {
	case ReqRequired:
		return "必填"
	case ReqConditional:
		if p.RequiredNote != "" {
			return "条件必填（" + p.RequiredNote + "）"
		}
		return "条件必填"
	default:
		return "可选"
	}
}

// tableCell 转义表格分隔符（字段说明里带 `|` 会把表格打散）。
func tableCell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

// itoa 是小整数转字符串（避免为一个数字引入 strconv）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

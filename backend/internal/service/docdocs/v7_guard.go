package docdocs

import (
	"fmt"
	"strings"

	"lingdong-api/internal/apidoc"
	sitecfg "lingdong-api/internal/pkg/sitecfg"
)

// 文案守卫：发布前扫一遍"不该出现在对外文档里的东西"。
//
// 与 v7 的其他体检码分工不同 —— 那些查结构（缺字段、路径漂移），这里查**用词**：
//
//	forbidden_word  内部视角用词（对外读者看不懂，或暴露实现）
//	forwarder_name  上游/转发平台名（等于把供货商告诉客户）
//	external_host   第三方域名（示例里出现别人的地址，读者会照着调）
//
// 为什么仍挂在发布闸门上而不是保存时：草稿允许半成品，运营先贴一段内部记录再改写
// 是正常流程；一保存就红会逼人绕过校验。这与 v6 的 Save/Publish 分工一致。
//
// 为什么必须是阻断项：这类错误**没有任何运行时症状** —— 页面 200、字段齐全，
// 只是把不该说的话说出去了。

// AllowHosts 是守卫的域名白名单。
//
// 由「占位域名 + 平台自己的全部域名」组成（多地址时每一条都算平台自己）。
// 来源必须是配置而不是请求 Host：AI 的调用没有 HTTP 请求上下文，
// 用请求推导会得到空集合，于是"引用平台自己的地址"被判成第三方域名。
func AllowHosts() []string {
	hosts := append([]string{}, apidoc.PlaceholderHosts...)
	return append(hosts, sitecfg.Load().Hosts()...)
}

// guarded 是一段"读者会读到的文字"及其在文档里的位置。
type guarded struct {
	path string
	text string
}

// guardEndpoint 扫一篇接口文档的全部人话（名称/说明/示例/错误文案）。
func guardEndpoint(b *EndpointBundle) []Problem {
	out := make([]Problem, 0, 2)
	add := func(items []guarded) {
		for _, it := range items {
			out = append(out, guardOne(it)...)
		}
	}
	add(guardedFields(b))
	return out
}

func guardedFields(b *EndpointBundle) []guarded {
	g := []guarded{
		{path: "name", text: b.EP.Name},
		{path: "summary", text: b.EP.Summary},
		{path: "run_hint", text: b.EP.RunHint},
	}
	for _, f := range b.Fields {
		p := "fields." + f.Name
		if f.VariantID != nil {
			p = "response.returns." + f.Name
		}
		g = append(g,
			guarded{path: p + ".desc", text: f.Desc},
			guarded{path: p + ".default", text: f.DefaultValue},
			guarded{path: p + ".example", text: string(f.Example)},
		)
	}
	for _, v := range b.Variants {
		p := fmt.Sprintf("response.variants[%d]", v.StatusCode)
		g = append(g, guarded{path: p + ".label", text: v.Label},
			guarded{path: p + ".raw", text: v.Raw},
			guarded{path: p + ".example", text: string(v.Example)})
	}
	// 示例代码与标题也是**展示给读者的文字**：不扫的话，一段写死第三方域名的
	// curl 就能绕过 E_EXTERNAL_HOST，而页面上看起来只是一段示例。
	for i, s := range []*SamplesView{
		samplesViewOf(b.EP.Samples, ""), samplesViewOf(b.EP.Samples2, ""),
	} {
		if s == nil {
			continue
		}
		p := fmt.Sprintf("samples[%d]", i)
		g = append(g,
			guarded{path: p + ".curl", text: s.Curl},
			guarded{path: p + ".python", text: s.Python},
			guarded{path: p + ".node", text: s.Node},
			guarded{path: p + ".go", text: s.Go},
		)
	}
	g = append(g,
		guarded{path: "example_title", text: b.EP.ExampleTitle},
		guarded{path: "example_title2", text: b.EP.ExampleTitle2},
	)
	for i, p := range b.Presets {
		path := fmt.Sprintf("presets[%d]", i)
		g = append(g, guarded{path: path + ".label", text: p.Label},
			guarded{path: path + ".summary", text: p.Summary},
			guarded{path: path + ".note", text: p.Note})
		if s := samplesViewOf(p.Samples, ""); s != nil {
			g = append(g, guarded{path: path + ".samples.curl", text: s.Curl},
				guarded{path: path + ".samples.python", text: s.Python},
				guarded{path: path + ".samples.node", text: s.Node},
				guarded{path: path + ".samples.go", text: s.Go})
		}
	}
	for i, e := range b.Errors {
		g = append(g, guarded{path: fmt.Sprintf("errors[%d].message", i), text: e.Message})
	}
	return g
}

// guardNode 扫节点的人话（标题/摘要/正文块）。
func guardNode(b *NodeBundle) []Problem {
	g := []guarded{{path: "title", text: b.Node.Title}, {path: "summary", text: b.Node.Summary}}
	for i, blk := range guideBlocks(b.Node.Blocks, "") {
		p := fmt.Sprintf("blocks[%d]", i)
		g = append(g, guarded{path: p + ".title", text: blk.Title},
			guarded{path: p + ".text", text: blk.Text},
			guarded{path: p + ".code", text: blk.Code})
		for _, it := range blk.Items {
			g = append(g, guarded{path: p + ".items", text: it})
		}
	}
	out := make([]Problem, 0, 2)
	for _, it := range g {
		out = append(out, guardOne(it)...)
	}
	return out
}

// guardOne 扫一段文字，命中即一条 Problem。
func guardOne(it guarded) []Problem {
	if strings.TrimSpace(it.text) == "" {
		return nil
	}
	vs := apidoc.CheckText(it.text, AllowHosts()...)
	if len(vs) == 0 {
		return nil
	}
	out := make([]Problem, 0, len(vs))
	for _, v := range vs {
		// Code 必须写成字面量：TestProblemCatalogCoversAllCodes 靠源码扫描守
		// 「每个码都能被解释」，拼出来的码会绕过它（漏登记也没人发现）。
		if v.Kind == "external_host" {
			out = append(out, Problem{
				Code: "E_EXTERNAL_HOST", Field: it.path, Blocking: true,
				Message: fmt.Sprintf("示例里出现了第三方地址 %q：对外文档一律写 https://api.example.com", v.Text),
			})
			continue
		}
		out = append(out, Problem{
			Code: "E_TEXT_FORBIDDEN", Field: it.path, Blocking: true,
			Message: fmt.Sprintf("%s（命中 %q）：对外文档不能出现内部用词与上游平台名", v.Hint, v.Text),
		})
	}
	return out
}

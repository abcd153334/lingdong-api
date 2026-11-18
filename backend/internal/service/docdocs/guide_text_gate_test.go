package docdocs

import (
	"strings"
	"testing"

	"lingdong-api/internal/apidoc"
)

// 本文件守着**说明页正文**这条线。它和接口页的门禁（apidoc/image_doc_gate_test.go）
// 分开放，是因为说明页的内容不在 apidoc 里，而在本包的 seed_pages.go：
// 门禁落在别的包，改这里的人就收不到红灯。

// TestQuickstartDocumentsIdempotencyKey 守住「通用约定必须写进快速开始」。
//
// 退化路径是真实发生过的：`Idempotency-Key` 实现完、上线、真机验过 409 重放，
// 而文档站上没有任何一处提到它 —— 功能存在但对外不存在，等于没做。
// 同步出图一次 20–60 秒，客户端超时自动重发是常态，没有这个键就是双扣。
func TestQuickstartDocumentsIdempotencyKey(t *testing.T) {
	var text string
	for _, p := range baselinePages() {
		if p.Slug == "quickstart" {
			for _, b := range p.Blocks {
				text += b.Text + "\n" + strings.Join(b.Items, "\n") + "\n"
			}
		}
	}
	if text == "" {
		t.Fatal("快速开始页不存在（说明页基线被删了？）")
	}
	if !strings.Contains(text, "Idempotency-Key") {
		t.Fatal("快速开始页没有提到 Idempotency-Key：同步接口的重试安全是通用约定，不能只存在于代码里")
	}
	// 「失败不占键」是这条契约最容易被实现细节带偏的一半：只写"重复提交返回 409"，
	// 读者会在请求失败后也换键重试，而换键就等于放弃幂等保护。
	if !strings.Contains(text, "失败且未计费") {
		t.Fatal("快速开始页没有说明「失败且未计费的请求不会占用这个键」")
	}
}

// TestErrorPageListsIdempotentReplay 守住错误码页里的 409 说明。
//
// 409 在这套协议里有两个来源（任务状态冲突 / 幂等重放），只写前者的话，
// 读者拿到 409 会去查任务状态，永远查不到原因。
func TestErrorPageListsIdempotentReplay(t *testing.T) {
	var text string
	for _, p := range baselinePages() {
		if p.Slug == "errors" {
			for _, b := range p.Blocks {
				text += b.Text + "\n" + strings.Join(b.Items, "\n") + "\n"
			}
		}
	}
	if !strings.Contains(text, "idempotent_replay") {
		t.Fatal("错误码页没有列出 idempotent_replay：409 的两个来源必须都说清楚")
	}
}

// TestGuidePagesPassTextGuard 把发布期的用词守卫提前到 `go test`。
//
// 守卫（E_TEXT_FORBIDDEN）只在**发布**这条路径上跑，而这批页面是播种出来的：
// 一旦基线里出现「渠道 / 上游 / 适配器」这类内部视角用词，页面会静默变成草稿，
// 测试全绿、线上少一篇。这一轮就是这么被咬到的。
func TestGuidePagesPassTextGuard(t *testing.T) {
	for _, p := range baselinePages() {
		var sb strings.Builder
		for _, b := range p.Blocks {
			sb.WriteString(b.Title)
			sb.WriteString("\n")
			sb.WriteString(b.Text)
			sb.WriteString("\n")
			sb.WriteString(strings.Join(b.Items, "\n"))
			sb.WriteString("\n")
			// 代码块里是 curl / JSON，写的是真实路径，不参与用词判断。
		}
		if vs := apidoc.CheckText(sb.String()); len(vs) > 0 {
			for _, v := range vs {
				t.Errorf("说明页 %s 含内部视角用词 %q（%s）", p.Slug, v.Text, v.Hint)
			}
		}
	}
}

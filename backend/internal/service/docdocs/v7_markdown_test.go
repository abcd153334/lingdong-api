package docdocs

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// TestRenderMarkdownExportsPublishedOnly 守两条：内容完整，且草稿不外泄。
//
// 导出的文件会被贴进工单、邮件、知识库 —— 草稿一旦漏出去就等于发布，
// 而且再也没法收回。判据必须与页面完全相同（`LoadTree` 的 where 条件）。
func TestRenderMarkdownExportsPublishedOnly(t *testing.T) {
	gdb := withDocDocsDB(t)
	if _, err := SeedDocs(); err != nil {
		t.Fatalf("播种失败: %v", err)
	}
	const base = "https://api.example.com"

	md, err := RenderMarkdown(base, base)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	for _, want := range []string{
		"# API 文档",
		"## 目录",
		"## 语言",
		"### OpenAI 兼容",
		"`POST /v1/chat/completions`",
		"**请求参数**",
		"**响应**",
		"**错误码**",
		"| 名称 | 类型 | 位置 | 必填 | 来源 | 说明 |",
		base + "/docs/openai",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("导出内容缺少 %q\n前 2000 字：\n%s", want, head(md, 2000))
		}
	}

	// 确定性：ETag 直接由字节算出，两次导出必须逐字节相同。
	// （顺序若随 map 迭代漂移，浏览器每次都会拿到"新版本"。）
	again, err := RenderMarkdown(base, base)
	if err != nil {
		t.Fatalf("二次导出失败: %v", err)
	}
	if md != again {
		t.Fatal("两次导出内容不一致：ETag 会永远失效")
	}

	// 把协议节点转草稿：导出里不该再出现它（但其他板块仍在）。
	if err := gdb.Model(&model.DocNode{}).Where("key = ?", "openai").
		Update("status", model.DocDraft).Error; err != nil {
		t.Fatalf("改状态失败: %v", err)
	}
	draftMD, err := RenderMarkdown(base, base)
	if err != nil {
		t.Fatalf("草稿场景导出失败: %v", err)
	}
	if strings.Contains(draftMD, "### OpenAI 兼容") {
		t.Fatal("草稿节点被导出：贴出去就等于发布了，而且收不回来")
	}
	if !strings.Contains(draftMD, "## 目录") {
		t.Fatal("整份导出不该因为一个节点是草稿就变空")
	}
}

// TestRenderMarkdownEscapesTableCells 守表格不会被内容撑坏。
//
// 字段说明里出现 `|` 或换行是常态（"a | b 二选一"），不转义会让整行错列，
// 而 Markdown 渲染器不会报错 —— 读者看到的是"说明少了一半"。
func TestRenderMarkdownEscapesTableCells(t *testing.T) {
	gdb := withDocDocsDB(t)
	if _, err := SeedDocs(); err != nil {
		t.Fatalf("播种失败: %v", err)
	}
	var f model.DocEndpointField
	if err := gdb.Where("name = ?", "model").Take(&f).Error; err != nil {
		t.Fatalf("取字段失败: %v", err)
	}
	const dirty = "a|b\n第二行"
	if err := gdb.Model(&model.DocEndpointField{}).Where("id = ?", f.ID).
		Update("desc", dirty).Error; err != nil {
		t.Fatalf("改字段说明失败: %v", err)
	}
	md, err := RenderMarkdown("https://api.example.com", "https://portal.example.com")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if !strings.Contains(md, `a\|b<br>第二行`) {
		t.Fatalf("表格单元格没有转义 `|` 与换行（整行会错列）")
	}
	if strings.Contains(md, "a|b\n") {
		t.Fatal("未转义的竖线出现在表格行里")
	}
}

// TestRenderMarkdownEmptySite 空站也要给一份可用文件。
//
// 导出是「打开链接」的动作，空库时回 500 会让读者以为服务坏了；
// 返回一个只有标题的文档，比一个错误页诚实得多。
func TestRenderMarkdownEmptySite(t *testing.T) {
	withDocDocsDB(t)
	md, err := RenderMarkdown("", "")
	if err != nil {
		t.Fatalf("空站导出失败: %v", err)
	}
	if !strings.HasPrefix(md, "# API 文档") {
		t.Fatalf("空站导出应只有标题与目录，实际：%q", head(md, 200))
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestUploadSamplesUseMultipart 守「文件上传的示例必须是 -F 表单」。
//
// 这是一条会静默出错的链路：示例生成器靠 `Transport` 判断拼 JSON 还是 -F，
// 而 v7 的投影层曾经没传这个字段 —— 于是上传接口的 curl 示例变成 JSON 体，
// 读者照抄必然 400，而页面上看不出任何异常（字段名、说明、错误码全都对）。
func TestUploadSamplesUseMultipart(t *testing.T) {
	withDocDocsDB(t)
	if _, err := SeedDocs(); err != nil {
		t.Fatalf("播种失败: %v", err)
	}
	b, err := LoadEndpoint("files", "ep-files-upload")
	if err != nil {
		t.Fatalf("装载上传接口失败: %v", err)
	}
	ep := ProjectEndpoint(b, "https://api.example.com")
	if ep.Samples == nil {
		t.Fatal("上传接口没有示例代码")
	}
	if !strings.Contains(ep.Samples.Curl, "-F ") {
		t.Fatalf("上传接口的 curl 示例必须是 -F 表单，实际：\n%s", ep.Samples.Curl)
	}
	if strings.Contains(ep.Samples.Curl, "-d '{") {
		t.Fatalf("上传接口的示例把字段拼成了 JSON 体（照抄会 400）：\n%s", ep.Samples.Curl)
	}
}

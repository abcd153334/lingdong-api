package agent

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 这些用例覆盖文档副本的纯文本化与切片（不连库）。
//
// 为什么值得单测：HTML 去标签是"AI 读到的唯一事实来源"，
// 去错了不会报错，只会让模型基于错的东西写提案。

func TestHTMLToText(t *testing.T) {
	in := `<html><head><title>t</title><style>.a{color:red}</style></head>
	<body><h1>对话接口</h1><p>第一段</p><p>第二段<br>换行</p>
	<script>var key="sk-secret"</script>
	<h2>参数</h2><ul><li>model</li><li>messages</li></ul>
	<p>&nbsp;引号&quot;与&amp;符号</p></body></html>`
	out := htmlToText(in)

	for _, bad := range []string{"<", ">", "color:red", "sk-secret"} {
		if strings.Contains(out, bad) {
			t.Fatalf("去标签后不应残留 %q：%s", bad, out)
		}
	}
	if !strings.Contains(out, "# 对话接口") {
		t.Fatalf("h1 应转成 Markdown 标题：%s", out)
	}
	if !strings.Contains(out, "## 参数") {
		t.Fatalf("h2 应转成二级标题：%s", out)
	}
	// 段落必须分行：`<p>a</p><p>b</p>` 粘成 `ab` 会让模型读不出这是两条独立事实。
	if !strings.Contains(out, "第一段\n") || !strings.Contains(out, "第二段") {
		t.Fatalf("段落应各自成行：%s", out)
	}
	if !strings.Contains(out, `"与&符号`) {
		t.Fatalf("实体未反转义：%s", out)
	}
}

func TestNormalizeText(t *testing.T) {
	in := "第一行   \r\n\r\n\r\n\r\n第二行\r\n"
	got := normalizeText(in)
	if got != "第一行\n\n第二行" {
		t.Fatalf("归一化结果不对：%q", got)
	}
}

func TestDocOutlineOffsetsAreSlicesOfContent(t *testing.T) {
	doc := &model.AgentDoc{Content: "# 总览\n介绍\n## 鉴权\n用 Bearer\n## 调用\nPOST /v1\n"}
	secs := DocOutline(doc)
	if len(secs) != 3 {
		t.Fatalf("应识别 3 个小节，实际 %d：%+v", len(secs), secs)
	}
	wantTitles := []string{"总览", "鉴权", "调用"}
	for i, s := range secs {
		if s.Title != wantTitles[i] {
			t.Fatalf("第 %d 节标题应为 %q，实际 %q", i, wantTitles[i], s.Title)
		}
		// 区间必须能在原文里切出这一节的正文：偏移算错的症状是
		// AI 读到别的小节内容，而两段话都"看起来合理"，无法察觉。
		if s.Start < 0 || s.End > len(doc.Content) || s.Start > s.End {
			t.Fatalf("第 %d 节偏移越界：%d-%d（总长 %d）", i, s.Start, s.End, len(doc.Content))
		}
		if !strings.Contains(doc.Content[s.Start:s.End], s.Title) {
			t.Fatalf("第 %d 节的区间不含自己的标题：%q", i, doc.Content[s.Start:s.End])
		}
	}
}

func TestSafeCutKeepsUTF8(t *testing.T) {
	s := "中文内容"
	// 3 是"中"的最后一字节之后、"文"的第一字节 —— 直接切会切出半个汉字。
	cut := safeCut(s, 4)
	if cut != "中" {
		t.Fatalf("应按字符边界回退，实际 %q", cut)
	}
	if safeCut(s, 100) != s {
		t.Fatal("上限大于长度时应原样返回")
	}
}

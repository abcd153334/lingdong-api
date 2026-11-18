package agent

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件覆盖"目录不再只认 Markdown"。
//
// 过去的实现在 JSON / CSV / 纯文本上一律返回空目录，模型拿到的
// "可用文档目录"是个空壳 —— 于是它要么整份索要，要么凭标题猜。

func TestProfileATXHeadings(t *testing.T) {
	text := "# 接入说明\n\n## 鉴权\n内容\n## 端点\n内容\n"
	p := buildDocProfile(text)
	if p.Kind != DocKindHeadings {
		t.Fatalf("kind 应为 headings，实际 %s", p.Kind)
	}
	if len(p.Sections) != 3 {
		t.Fatalf("应识别 3 个标题，实际 %d", len(p.Sections))
	}
	if p.Sections[1].Title != "鉴权" || p.Sections[1].Start == 0 {
		t.Fatalf("第二节不对：%+v", p.Sections[1])
	}
	// End 必须首尾相接：按节读时不能漏字也不能重字。
	for i := 0; i+1 < len(p.Sections); i++ {
		if p.Sections[i].End != p.Sections[i+1].Start {
			t.Fatalf("第 %d 节 End 与下一节 Start 不相接", i)
		}
	}
	if p.Sections[len(p.Sections)-1].End != len(text) {
		t.Fatal("最后一节应到文末")
	}
}

func TestProfileSetext(t *testing.T) {
	text := "接入说明\n====\n\n正文\n\n鉴权方式\n----\n\nBearer\n"
	p := buildDocProfile(text)
	if p.Kind != DocKindHeadings {
		t.Fatalf("kind 应为 headings，实际 %s", p.Kind)
	}
	if len(p.Sections) != 2 || p.Sections[0].Title != "接入说明" || p.Sections[1].Title != "鉴权方式" {
		t.Fatalf("Setext 标题识别不对：%+v", p.Sections)
	}
	// Start 指向标题行而不是下划线行。
	if !strings.HasPrefix(text[p.Sections[0].Start:], "接入说明") {
		t.Fatal("Start 应指向标题行")
	}
}

func TestProfileNumberedOutline(t *testing.T) {
	text := "1. 注册账号\n内容\n2. 创建密钥\n内容\n3. 调用接口\n内容\n4. 查询用量\n内容\n"
	p := buildDocProfile(text)
	if p.Kind != DocKindOutline {
		t.Fatalf("kind 应为 outline，实际 %s", p.Kind)
	}
	if len(p.Sections) != 4 {
		t.Fatalf("应识别 4 条，实际 %d：%+v", len(p.Sections), p.Sections)
	}
	if !strings.Contains(p.Sections[0].Title, "注册账号") {
		t.Fatalf("标题应包含正文：%s", p.Sections[0].Title)
	}
}

func TestProfileNumberedNeedsEnoughItems(t *testing.T) {
	// 只有两条列表不算结构，否则任何带项目符号的短文本都会被当成目录。
	text := "- 第一条\n- 第二条\n正文若干\n"
	p := buildDocProfile(text)
	if p.Kind == DocKindOutline {
		t.Fatal("两条列表不该被判成大纲")
	}
}

func TestProfileJSON(t *testing.T) {
	text := `{
  "endpoints": [
    {"path": "/v1/chat", "method": "POST"},
    {"path": "/v1/embed", "method": "POST"}
  ],
  "auth": {"header": "Authorization"}
}`
	p := buildDocProfile(text)
	if p.Kind != DocKindJSON {
		t.Fatalf("kind 应为 json，实际 %s", p.Kind)
	}
	titles := map[string]bool{}
	for _, s := range p.Sections {
		titles[s.Title] = true
	}
	for _, want := range []string{"endpoints", "auth", "path", "method"} {
		if !titles[want] {
			t.Errorf("JSON 目录缺少键 %q：%+v", want, titles)
		}
	}
	// 每个 section 的起点必须落在它自己的键上（近似定位的边界）。
	for _, s := range p.Sections {
		if s.Start < 0 || s.Start >= len(text) {
			t.Errorf("%s 的 Start 越界：%d", s.Title, s.Start)
		}
	}
}

func TestProfileTable(t *testing.T) {
	text := "model,endpoint,price\nkling,chat,1\nhailuo,video,2\n"
	p := buildDocProfile(text)
	if p.Kind != DocKindTable {
		t.Fatalf("kind 应为 table，实际 %s", p.Kind)
	}
	if len(p.Sections) != 1 || !strings.Contains(p.Sections[0].Title, "3 列") {
		t.Fatalf("表格目录不对：%+v", p.Sections)
	}
}

func TestProfilePlainFallback(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("这是一段没有任何标题的说明文字，用来验证兜底分块。\n")
		b.WriteString("第二行同样是普通正文。\n\n")
	}
	p := buildDocProfile(b.String())
	if p.Kind != DocKindPlain {
		t.Fatalf("kind 应为 plain，实际 %s", p.Kind)
	}
	if len(p.Sections) < 2 {
		t.Fatalf("无结构文档也应能分块，实际 %d 节", len(p.Sections))
	}
	if p.ReadHint == "" {
		t.Fatal("兜底也要给读法提示")
	}
}

func TestProfileSectionCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxDocSections+20; i++ {
		b.WriteString("# 标题")
		b.WriteString(strings.Repeat("x", i%5+1))
		b.WriteString("\n")
	}
	p := buildDocProfile(b.String())
	if len(p.Sections) != maxDocSections {
		t.Fatalf("应截到 %d 节，实际 %d", maxDocSections, len(p.Sections))
	}
}

// TestDocProfileOfIsStable 覆盖缓存的基本契约：同一份文档两次必须一致。
func TestDocProfileOfIsStable(t *testing.T) {
	doc := &model.AgentDoc{ID: 9001, SizeBytes: 12, Content: "# A\n内容\n# B\n内容\n"}
	p1 := DocProfileOf(doc)
	p2 := DocProfileOf(doc)
	if len(p1.Sections) != len(p2.Sections) || p1.Kind != p2.Kind {
		t.Fatalf("两次画像不一致：%+v / %+v", p1, p2)
	}
	if len(p1.Sections) != 2 {
		t.Fatalf("应识别 2 节，实际 %d", len(p1.Sections))
	}
}

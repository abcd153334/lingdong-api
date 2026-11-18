package apidoc

import (
	"fmt"
	"testing"
)

// 侧栏顺序是产品的可见结构：两个平台板块在前，能力板块按产品顺序
// （语言 → 图片 → 视频 → 音频 → 音乐），不是英文名字母序 —— 协议篇讲「怎么调」，
// 其余讲「调什么」，必须紧跟开始使用/通用接口之后。
//
// 这条测试是那份顺序的看门人：调换会让用户的肌肉记忆失效。
func TestSectionOrder(t *testing.T) {
	want := []string{"start", "general", SectionProtocols, "images", "videos", "audio", "music"}
	got := Sections()
	if len(got) != len(want) {
		t.Fatalf("板块数 %d，期望 %d：%+v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("第 %d 个板块是 %q，期望 %q", i+1, got[i].ID, id)
		}
		if got[i].Name == "" || got[i].Color == "" || got[i].Short == "" {
			t.Fatalf("板块 %s 缺名称/颜色/短名：%+v", id, got[i])
		}
	}
}

// 能力板块的顺序是**产品顺序**（语言 → 图片 → 视频 → 音频 → 音乐），不是英文名
// 字母序：协议类是「怎么调」，其余是「调什么」，必须紧跟开始使用/通用接口之后；
// 按 ASCII 排会把「协议类」推到音频后面，与读者的心智顺序相反。
//
// 顺带守中文名：改名（图像→图片、语言模型→语言）会同时改 Markdown 导出与面包屑，
// 名字写错时这条直接红，不必等页面出来才发现。
func TestSectionProductOrder(t *testing.T) {
	want := []struct{ id, name string }{
		{SectionProtocols, "语言"},
		{"images", "图片"},
		{"videos", "视频"},
		{"audio", "音频"},
		{"music", "音乐"},
	}
	got := aiSectionOrder()
	if len(got) != len(want) {
		t.Fatalf("能力板块数 %d，期望 %d：%v", len(got), len(want), got)
	}
	for i, w := range want {
		s, ok := SectionOf(got[i])
		if !ok {
			t.Fatalf("第 %d 个能力板块 %q 未登记", i+1, got[i])
		}
		if s.ID != w.id || s.Name != w.name {
			t.Fatalf("第 %d 个能力板块是 %q/%q，期望 %q/%q", i+1, s.ID, s.Name, w.id, w.name)
		}
		// 英文名仍然是侧栏标题里的括号内容，不能为空（空 En 会渲染出「（）」）
		if s.En == "" {
			t.Fatalf("能力板块 %q 缺英文名", s.ID)
		}
	}
}

// 容器必须覆盖每个板块恰好一次：漏一个板块会让它从侧栏消失，
func TestEverySectionInOrderList(t *testing.T) {
	inOrder := map[string]bool{}
	for _, id := range sectionOrderList {
		if inOrder[id] {
			t.Fatalf("顺序表里 %q 重复", id)
		}
		inOrder[id] = true
	}
	meta := map[string]bool{}
	for _, c := range containersMeta {
		meta[c.ID] = true
	}
	for id, s := range docOnlySections {
		if !inOrder[id] {
			t.Fatalf("板块 %q 在定义表却不在顺序表：它永远不会出现在响应里，且不报错", id)
		}
		if !meta[s.Container] {
			t.Fatalf("板块 %q 的容器 %q 未在 containersMeta 登记", id, s.Container)
		}
	}
	if len(sectionOrderList) != len(docOnlySections) {
		t.Fatalf("顺序表 %d 项、定义表 %d 项，两者必须一一对应", len(sectionOrderList), len(docOnlySections))
	}
	if got := len(Sections()); got != len(docOnlySections) {
		t.Fatalf("Sections() 返回 %d 个板块，定义表有 %d 个", got, len(docOnlySections))
	}
}

// 功能组（文件与产物 / 异步任务 / 账户与用量）不是能力板块，不能占侧栏一级位置。
func TestSectionExcludesFunctionalGroups(t *testing.T) {
	for _, id := range []string{"file", "task", "acct", "model"} {
		if s, ok := SectionOf(id); ok {
			t.Fatalf("功能组 %s 不该出现在文档站侧栏：%+v", id, s)
		}
	}
}

func TestSectionOfModel(t *testing.T) {
	cases := map[string]string{
		"chat":       SectionProtocols,
		"embedding":  SectionProtocols,
		"rerank":     SectionProtocols,
		"moderation": SectionProtocols,
		"image":      "images",
		"video":      "videos",
		"music":      "music",
		"audio":      "audio",
		"":           SectionProtocols, // 空模态按 chat 归一
		"unknown":    "",               // 未知模态不落任何板块（不建族）
	}
	for mod, want := range cases {
		got := SectionOfModel(ModelFacts{Modality: mod})
		if got != want {
			t.Fatalf("模态 %q 归属板块 %q，期望 %q", mod, got, want)
		}
	}
}

// 文档形态只有两种：语言/向量类走固定协议篇，图像/视频/音频/音乐建族。
//
// 三态（None / Protocol / Family）缺一不可：未知模态若判成 Family，会生成一个
// `section=""` 的幽灵组，侧栏里出现一个 id 当名字的空白板块，且没有报错。
func TestDocModeOf(t *testing.T) {
	for _, mod := range []string{"chat", "embedding", "rerank", "moderation", ""} {
		if got := DocModeOf(ModelFacts{Modality: mod}); got != DocModeProtocol {
			t.Fatalf("模态 %q 应走协议文档，实际 %v", mod, got)
		}
	}
	for _, mod := range []string{"image", "video", "music", "audio"} {
		if got := DocModeOf(ModelFacts{Modality: mod}); got != DocModeFamily {
			t.Fatalf("模态 %q 应建模型族，实际 %v", mod, got)
		}
	}
	if got := DocModeOf(ModelFacts{Modality: "unknown"}); got != DocModeNone {
		t.Fatalf("未知模态不该建文档，实际 %v", got)
	}
}

// 每个端点都必须落到一个**有名字**的能力板块或通用接口。
//
// 守的是「新增端点时忘了给 group 补映射」：Group 写成没人认识的字符串时，
// SectionOfGroup 会把它静默兜到通用接口 —— 文档还在，但用户在对应能力板块里
// 永远找不到它，而且没有任何报错。
func TestEveryEndpointGroupMapsToSection(t *testing.T) {
	for _, k := range Kinds() {
		tr, ok := TransportOf(k)
		if !ok {
			t.Fatalf("端点 %q 缺少传输契约", k)
		}
		if tr.Group == "" {
			t.Fatalf("端点 %q 没有导航组", k)
		}
		sec := SectionOfKind(k)
		if sec == "" {
			t.Fatalf("端点 %q 的导航组 %q 无法映射到板块", k, tr.Group)
		}
		if _, ok := SectionOf(sec); !ok {
			t.Fatalf("端点 %q 落到未登记的板块 %q", k, sec)
		}
		if tr.Group == "acct" || tr.Group == "model" {
			if err := validateGroupForModels(k); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// validateGroupForModels 是最小一致性检查：模型三兄弟必须同在 model 组。
func validateGroupForModels(k Kind) error {
	tr, _ := TransportOf(k)
	// KindGeminiModels 也是「模型清单」：它只是换了一种协议的写法，
	// 放在 model 组正是为了让它和 OpenAI 面的模型列表并排出现。
	isModel := k == KindModelsList || k == KindModelsGet || k == KindModelsDefaults || k == KindGeminiModels
	if isModel && tr.Group != "model" {
		return fmt.Errorf("模型端点 %q 必须在 model 组，实际 %q", k, tr.Group)
	}
	if !isModel && tr.Group == "model" {
		return fmt.Errorf("非模型端点 %q 不能挂在 model 组", k)
	}
	return nil
}

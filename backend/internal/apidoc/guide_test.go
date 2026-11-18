package apidoc

import (
	"strings"
	"testing"
)

func validBlocks() []GuideBlock {
	return []GuideBlock{
		{Type: BlockPara, Text: "一段说明。"},
		{Type: BlockSteps, Title: "步骤", Items: []string{"第一步", "第二步"}},
		{Type: BlockBullets, Items: []string{"要点"}},
		{Type: BlockCode, Lang: "bash", Code: "curl {{base_url}}/v1/models"},
		{Type: BlockNote, Text: "别重试 4xx。"},
	}
}

func TestValidateBlocksAcceptsAllTypes(t *testing.T) {
	if err := ValidateBlocks(validBlocks()); err != nil {
		t.Fatalf("合法正文被拒：%v", err)
	}
	// 空数组合法：新建说明页先存空壳是正常流程。
	if err := ValidateBlocks(nil); err != nil {
		t.Fatalf("空正文应合法：%v", err)
	}
}

// 每种非法都必须被拦在保存期：存进去之后没人会再看见它。
func TestValidateBlocksRejects(t *testing.T) {
	cases := map[string]GuideBlock{
		"未知类型":  {Type: "table", Text: "x"},
		"缺类型":   {Text: "x"},
		"段落为空":  {Type: BlockPara, Text: "   "},
		"列表无条目": {Type: BlockSteps, Items: []string{" ", ""}},
		"代码为空":  {Type: BlockCode, Lang: "bash"},
		"代码缺语言": {Type: BlockCode, Code: "echo 1"},
		"提示为空":  {Type: BlockNote, Text: ""},
		"段落超长":  {Type: BlockPara, Text: strings.Repeat("字", BlockTextMaxRunes+1)},
		"小标题超长": {Type: BlockPara, Text: "x", Title: strings.Repeat("字", BlockTitleMaxRunes+1)},
		"单条超长":  {Type: BlockBullets, Items: []string{strings.Repeat("字", BlockItemMaxRunes+1)}},
		"代码超长":  {Type: BlockCode, Lang: "go", Code: strings.Repeat("x", BlockCodeMaxRunes+1)},
	}
	for name, b := range cases {
		if err := ValidateBlocks([]GuideBlock{b}); err == nil {
			t.Fatalf("%s 应被拒绝", name)
		}
	}
	if err := ValidateBlocks(make([]GuideBlock, BlocksMaxCount+1)); err == nil {
		t.Fatal("块数超限应被拒绝")
	}
}

// BlockTypes 是管理台下拉的唯一来源，漏一个就会「存得进去但选不出来」。
func TestBlockTypesCoverValidation(t *testing.T) {
	ok := make(map[string]bool)
	for _, tp := range BlockTypes() {
		ok[tp] = true
	}
	for _, tp := range []string{BlockPara, BlockSteps, BlockBullets, BlockCode, BlockNote} {
		if !ok[tp] {
			t.Fatalf("BlockTypes() 缺少 %q", tp)
		}
	}
	if len(ok) != 5 {
		t.Fatalf("块类型应恰好 5 种，实际 %d", len(ok))
	}
}

// 占位符只替换已知变量：未知的**原样保留**，这样管理台预览能看见它还剩着，
// 比静默替换成空串（页面凭空少一个词，没人发现）安全。
func TestResolveGuideBlocks(t *testing.T) {
	blocks := []GuideBlock{
		{Type: BlockCode, Lang: "bash", Code: "curl {{base_url}}/v1/models"},
		{Type: BlockBullets, Items: []string{"{{base_url}} 与 {{unknown}}"}},
		{Type: BlockPara, Text: "不用占位符。"},
	}
	out := ResolveGuideBlocks(blocks, GuideVars{BaseURL: "https://api.example.com"})
	if strings.Contains(out[0].Code, "{{base_url}}") {
		t.Fatalf("代码块占位符未解析：%q", out[0].Code)
	}
	if out[0].Code != "curl https://api.example.com/v1/models" {
		t.Fatalf("代码块解析结果不对：%q", out[0].Code)
	}
	if out[1].Items[0] != "https://api.example.com 与 {{unknown}}" {
		t.Fatalf("列表项解析结果不对：%q", out[1].Items[0])
	}
	if out[2].Text != blocks[2].Text {
		t.Fatalf("无占位符的块被改动：%q", out[2].Text)
	}
	// 不得修改入参（调用方可能缓存原始块）。
	if blocks[0].Code != "curl {{base_url}}/v1/models" {
		t.Fatalf("入参被就地修改：%q", blocks[0].Code)
	}
	// BaseURL 为空时不解析（管理台在不知道真实域名时预览的是原始占位符）。
	raw := ResolveGuideBlocks(blocks, GuideVars{})
	if raw[0].Code != blocks[0].Code {
		t.Fatalf("空 BaseURL 时不该替换：%q", raw[0].Code)
	}
}

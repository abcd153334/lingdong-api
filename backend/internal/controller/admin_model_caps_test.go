package controller

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 能力声明是文档分组的输入，保存期必须把三类错误挡在库外：
// 未定义的词、与模态不匹配的词、重复的词。
func TestCapabilitiesOfRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"未定义词":   `["t2i","foo"]`,
		"模态不匹配":  `["music"]`,     // image 模态不接受 music
		"重复":     `["t2i","t2i"]`, // 同一能力声明两次
		"不是数组":   `{"t2i":true}`,  // 类型错误
		"元素非字符串": `[1,2]`,
	}
	for name, raw := range cases {
		body := model.JSON(raw)
		req := adminModelReq{Modality: "image", Capabilities: &body}
		if _, provided, err := capabilitiesOf(req); err == nil || !provided {
			t.Fatalf("%s：应判为「提供了非法值」并报错，实际 err=%v provided=%v", name, err, provided)
		}
	}
}

// nil 表示「不修改」：只改价格的保存绝不能把能力清成 `[]`
// （`[]` 是「明确声明没有额外能力」，与 NULL 语义不同，启动期补齐只补 NULL）。
func TestCapabilitiesOfNilMeansKeep(t *testing.T) {
	req := adminModelReq{Modality: "image"}
	got, provided, err := capabilitiesOf(req)
	if err != nil || provided || got != nil {
		t.Fatalf("未提供能力时应原样不动：got=%v provided=%v err=%v", got, provided, err)
	}
}

// 提供了合法值 → 归一化（小写/去空白/排序）后落库，保证与词表内的写法逐字一致。
func TestCapabilitiesOfNormalizes(t *testing.T) {
	body := model.JSON(`[" I2V ", "t2v"]`)
	req := adminModelReq{Modality: "video", Capabilities: &body}
	got, provided, err := capabilitiesOf(req)
	if err != nil || !provided {
		t.Fatalf("合法值应通过：err=%v provided=%v", err, provided)
	}
	// 排过序的归一结果（t2v < i2v 按字典序为 i2v, t2v）
	joined := strings.Join(got, ",")
	if joined != "i2v,t2v" && joined != "t2v,i2v" {
		t.Fatalf("归一结果不符合词表写法：%v", got)
	}
	for _, c := range got {
		if strings.Contains(c, " ") || c != strings.ToLower(c) {
			t.Fatalf("归一结果未去空白/未转小写：%v", got)
		}
	}
}

// 显式空数组是合法声明（= 这个模型没有额外能力），不能当成「未提供」。
func TestCapabilitiesOfEmptyArrayIsExplicit(t *testing.T) {
	body := model.JSON(`[]`)
	req := adminModelReq{Modality: "image", Capabilities: &body}
	got, provided, err := capabilitiesOf(req)
	if err != nil || !provided {
		t.Fatalf("空数组应视为显式声明：err=%v provided=%v", err, provided)
	}
	// 空数组与 nil 的区别由 modelcfg.Input.CapabilitiesSet 承载：
	// 这里只要求"不是 nil"（nil 会被当成未提供）。
	if got == nil || len(got) != 0 {
		t.Fatalf("空数组应落成非 nil 的空切片（不是 null）：%#v", got)
	}
}

// Package modality 是「模型大类」的唯一词表。
//
// 为什么单独成包：这 8 个值此前在 7 处各写一遍（文档字段枚举、接口契约枚举、
// 目录校验、板块归属、端点默认值、前端类型），取值集合一致但**顺序不同**，
// 于是同一份枚举在文档站与开放接口参考里长成两个样子；新增一个模态（比如
// "3d"）时，漏改的那一处不会报错 —— 它只是永远不接受那个值。
//
// 顺序即文档顺序：枚举是按这个顺序印出来的，排在最前的是"最常用"的那几个。
package modality

import "strings"

// All 是全集（顺序即对外文档里的枚举顺序）。
//
// 前四个是语言类（只出现在协议篇、不建模型文档），后四个是生成类。
// 分界线见 apidoc.SectionOfModel —— 那里按同一份集合判板块归属。
var All = []string{
	"chat", "embedding", "rerank", "moderation",
	"image", "video", "music", "audio",
}

// Default 是空值/未知值的回落（兼容协议里不写 modality 的一律按对话处理）。
const Default = "chat"

// Normalize 归一：去空白，空值回落 chat。
//
// 不校验合法性：未知值必须原样透传（模型可能带自定义模态，硬拒绝会让
// 一个本来能跑的模型进不了目录）。合法性由 IsValid 单独提供。
func Normalize(m string) string {
	m = strings.TrimSpace(m)
	if m == "" {
		return Default
	}
	return m
}

// IsValid 判断是否是已知模态。
func IsValid(m string) bool {
	m = strings.TrimSpace(m)
	for _, x := range All {
		if x == m {
			return true
		}
	}
	return false
}

// Language 判断是否语言类模态（不建模型文档，只进协议篇）。
func Language(m string) bool {
	switch Normalize(m) {
	case "chat", "embedding", "rerank", "moderation":
		return true
	}
	return false
}

// Copy 返回全集的副本（给需要拼接/排序的调用方，避免改到共享切片）。
func Copy() []string {
	out := make([]string, len(All))
	copy(out, All)
	return out
}

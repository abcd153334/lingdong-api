package apidoc

// 本文件是「人写的文案」各项上限的唯一真相源。
//
// 为什么数字必须集中在这里：同一个上限被三处消费 ——
//   - project.go 渲染时裁剪（超出的条目直接丢弃）；
//   - 管理台保存时校验（超了就拒绝，并说明超了哪一条）；
//   - /api/admin/docs/contract 把规则交给 AI 编排（它据此生成文案）。
//
// 三处各写一个数字的结果是：AI 按 5 条写、保存期按 5 条收、渲染期按 3 条裁，
// 用户看到的是「我写了五行、页面上只有三行」，而没有任何报错。
//
// 裁剪策略刻意分成两类（不要统一）：
//   - 列表类（highlights / limitations / notes / examples）超限 → 渲染时截断。
//     这是历史数据的兜底：早期写入的行不该让整页 500。
//   - 单条文本超长（intro / 每条要点）→ 渲染时**不裁**，只在保存时拒绝。
//     半句话被剪掉比整篇发不出去更糟。

// Caps 是文案容量。
type Caps struct {
	// IntroMaxRunes 是「能力简介」的字数上限。
	IntroMaxRunes int `json:"intro_max_runes"`
	// HighlightsMax / LimitationsMax 是能力亮点、使用限制的条数上限。
	HighlightsMax  int `json:"highlights_max"`
	LimitationsMax int `json:"limitations_max"`
	// NotesMax 是注意事项的条数上限。
	NotesMax int `json:"notes_max"`
	// ExamplesMax 是补充示例的条数上限。
	ExamplesMax int `json:"examples_max"`
	// LineMaxRunes 是「一条要点」的字数上限（highlights / limitations / notes 共用）。
	LineMaxRunes int `json:"line_max_runes"`
	// FieldNoteMaxRunes 是单条字段说明的字数上限。
	FieldNoteMaxRunes int `json:"field_note_max_runes"`
	// ExampleTitleMaxRunes 是示例标题的字数上限。
	ExampleTitleMaxRunes int `json:"example_title_max_runes"`
	// ExampleNoteMaxRunes 是示例备注的字数上限。
	ExampleNoteMaxRunes int `json:"example_note_max_runes"`
	// GroupTitleMaxRunes 是文档组人工标题的字数上限（自动标题不会超）。
	GroupTitleMaxRunes int `json:"group_title_max_runes"`
	// GuideTitleMaxRunes 是说明页标题的字数上限（侧栏一行放得下）。
	GuideTitleMaxRunes int `json:"guide_title_max_runes"`
}

var caps = Caps{
	IntroMaxRunes:        400,
	HighlightsMax:        5,
	LimitationsMax:       5,
	NotesMax:             12,
	ExamplesMax:          3,
	LineMaxRunes:         80,
	FieldNoteMaxRunes:    200,
	ExampleTitleMaxRunes: 40,
	ExampleNoteMaxRunes:  120,
	GroupTitleMaxRunes:   30,
	GuideTitleMaxRunes:   24,
}

// DocCaps 返回文案容量（供管理台校验与 AI 编排规则）。
func DocCaps() Caps { return caps }

// ForbiddenWords 返回内部视角用词表。
//
// 对外暴露是必要的：管理台要在输入时实时提示、AI 编排要知道哪些词不能写。
// 返回副本，避免调用方改到全局表（改了就等于把门禁关掉）。
func ForbiddenWords() []string {
	out := make([]string, len(forbiddenWords))
	copy(out, forbiddenWords)
	return out
}

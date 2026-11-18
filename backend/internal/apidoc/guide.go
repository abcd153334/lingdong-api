package apidoc

import (
	"fmt"
	"strings"
)

// 说明页的块类型（封闭枚举）。
//
// 为什么不是 Markdown 字符串：
//   - 前端要逐块渲染（代码块带复制按钮、步骤带序号），存 Markdown 就得在客户端
//     再实现一遍解析器，而两个解析器的容错行为必然不同 → 管理台预览与线上不一致；
//   - 块类型是**可校验的**：写了 `code` 却没给 lang 能在保存期报 400，
//     存 Markdown 则要等到有人肉眼看。
const (
	BlockPara    = "para"
	BlockSteps   = "steps"
	BlockBullets = "bullets"
	BlockCode    = "code"
	BlockNote    = "note"
)

// GuideBlock 是说明页正文的一块。
//
// 字段按类型取用（别的类型字段留空即可）：
//
//	para    Text
//	steps   Title?, Items[]（有序）
//	bullets Title?, Items[]（无序）
//	code    Title?, Lang, Code
//	note    Text（提示/警告语气）
//
// 保留零值语义：`Title` 为空表示不渲染小标题。
type GuideBlock struct {
	Type  string   `json:"type"`
	Title string   `json:"title,omitempty"`
	Text  string   `json:"text,omitempty"`
	Items []string `json:"items,omitempty"`
	Lang  string   `json:"lang,omitempty"`
	Code  string   `json:"code,omitempty"`
}

// 容量上限（保存期校验 + 前端计数器同源）。
const (
	BlockTextMaxRunes  = 2000
	BlockItemsMax      = 30
	BlockItemMaxRunes  = 300
	BlockCodeMaxRunes  = 6000
	BlocksMaxCount     = 80
	BlockTitleMaxRunes = 60
)

// BlockTypes 返回允许的块类型（管理台下拉与校验共用）。
func BlockTypes() []string {
	return []string{BlockPara, BlockSteps, BlockBullets, BlockCode, BlockNote}
}

// ValidateBlocks 校验说明页正文（保存期 400 的唯一实现）。
//
// 空块数组**合法**：新建说明页时先存个空壳再写内容，是正常流程。
func ValidateBlocks(blocks []GuideBlock) error {
	if len(blocks) > BlocksMaxCount {
		return fmt.Errorf("正文块过多（最多 %d 块）", BlocksMaxCount)
	}
	for i, b := range blocks {
		if err := validateBlock(i, b); err != nil {
			return err
		}
	}
	return nil
}

func validateBlock(i int, b GuideBlock) error {
	at := fmt.Sprintf("第 %d 块", i+1)
	if n := len([]rune(b.Title)); n > BlockTitleMaxRunes {
		return fmt.Errorf("%s：小标题超过 %d 字", at, BlockTitleMaxRunes)
	}
	switch b.Type {
	case BlockPara, BlockNote:
		text := strings.TrimSpace(b.Text)
		if text == "" {
			return fmt.Errorf("%s：正文不能为空", at)
		}
		if n := len([]rune(text)); n > BlockTextMaxRunes {
			return fmt.Errorf("%s：正文超过 %d 字", at, BlockTextMaxRunes)
		}
	case BlockSteps, BlockBullets:
		items := nonEmpty(b.Items)
		if len(items) == 0 {
			return fmt.Errorf("%s：至少需要一条内容", at)
		}
		if len(items) > BlockItemsMax {
			return fmt.Errorf("%s：条目过多（最多 %d 条）", at, BlockItemsMax)
		}
		for _, it := range items {
			if n := len([]rune(it)); n > BlockItemMaxRunes {
				return fmt.Errorf("%s：单条超过 %d 字", at, BlockItemMaxRunes)
			}
		}
	case BlockCode:
		if strings.TrimSpace(b.Code) == "" {
			return fmt.Errorf("%s：代码不能为空", at)
		}
		if n := len([]rune(b.Code)); n > BlockCodeMaxRunes {
			return fmt.Errorf("%s：代码超过 %d 字", at, BlockCodeMaxRunes)
		}
		if strings.TrimSpace(b.Lang) == "" {
			return fmt.Errorf("%s：代码块需要标注语言", at)
		}
	case "":
		return fmt.Errorf("%s：缺少类型", at)
	default:
		return fmt.Errorf("%s：未知类型 %q", at, b.Type)
	}
	return nil
}

// GuideVars 是说明页正文里可用的占位符。
//
// 只做**已知变量的整体替换**（`{{name}}` → 值），不支持表达式/条件：
// 说明页是静态文档，任何逻辑最终都会变成「预览与线上不一致」的来源。
type GuideVars struct {
	BaseURL string
}

// ResolveGuideBlocks 把占位符替换成真实值，返回新切片（不修改入参）。
//
// 未知占位符**原样保留**：管理台预览时能看到 `{{foo}}` 还剩着，
// 比静默替换成空串（页面上凭空少一个词，没人发现）安全。
func ResolveGuideBlocks(blocks []GuideBlock, vars GuideVars) []GuideBlock {
	repl := map[string]string{}
	if vars.BaseURL != "" {
		repl["{{base_url}}"] = vars.BaseURL
	}
	if len(repl) == 0 {
		out := make([]GuideBlock, len(blocks))
		copy(out, blocks)
		return out
	}
	fix := func(s string) string {
		for k, v := range repl {
			s = strings.ReplaceAll(s, k, v)
		}
		return s
	}
	out := make([]GuideBlock, len(blocks))
	for i, b := range blocks {
		out[i] = b
		out[i].Title = fix(b.Title)
		out[i].Text = fix(b.Text)
		out[i].Code = fix(b.Code)
		if b.Items != nil {
			out[i].Items = make([]string, len(b.Items))
			for j, it := range b.Items {
				out[i].Items[j] = fix(it)
			}
		}
	}
	return out
}

// nonEmpty 去空白并去空串。
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

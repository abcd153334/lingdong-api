package modelcfg

import (
	"strings"
	"time"
	"unicode/utf8"

	"lingdong-api/internal/model"
)

// 本文件是模型营销标签的**唯一**校验与归一实现（管理台与 AI 提案共用）。
//
// 三条规则值得单独记：
//  1. 空 label 的行**视为不存在**：表单里点了「添加」还没填就保存，不该 400；
//  2. 颜色要么是 12 个色板 key，要么是 `#RRGGBB`；未知 key 直接拒 —— 「存进去但显示灰」
//     比 400 难排查得多；
//  3. 不拒绝过去的时间：已过期的存量项必须能随模型整组保存，否则运营无法续期。

// normalizeTags 校验并归一标签数组（保序、去空行、颜色归一、到期转 UTC）。
func normalizeTags(in []model.ModelTag, loc *time.Location) ([]model.ModelTag, error) {
	out := make([]model.ModelTag, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, t := range in {
		label := strings.TrimSpace(t.Label)
		if label == "" {
			continue
		}
		if n := utf8.RuneCountInString(label); n > model.TagLabelMaxRunes {
			return nil, fe("tags", "标签「%s」超过 %d 字（当前 %d 字）",
				label, model.TagLabelMaxRunes, n)
		}
		if _, dup := seen[label]; dup {
			return nil, fe("tags", "标签「%s」重复", label)
		}
		seen[label] = struct{}{}

		color := strings.TrimSpace(t.Color)
		if strings.HasPrefix(color, "#") {
			hex, err := model.NormalizeHexColor(color)
			if err != nil {
				return nil, fe("tags", "标签「%s」的颜色 %q 需为 6 位十六进制", label, color)
			}
			color = hex
		} else if _, ok := model.TagColorHex(color); ok {
			color = strings.ToLower(color)
		} else {
			return nil, fe("tags", "标签「%s」的颜色 %q 不在可选色板内", label, color)
		}

		until, err := model.ParseTagUntil(t.Until, loc)
		if err != nil {
			return nil, fe("tags", "标签「%s」的到期时间无效：%v", label, err)
		}
		out = append(out, model.ModelTag{Label: label, Color: color, Until: until})
	}
	if len(out) > model.TagMaxCount {
		return nil, fe("tags", "最多 %d 个标签（当前 %d 个）", model.TagMaxCount, len(out))
	}
	return out, nil
}

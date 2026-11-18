package model

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// 本文件是模型营销标签（models.tags）的唯一实现。
//
// 数据形状（JSONB 数组，数组顺序即展示顺序）：
//
//	[{"label":"限时免费","color":"red","until":"2026-12-31T15:59:59Z"}]
//
// 三个决定：
//   - **没有 order 字段**：数组顺序就是顺序。多存一份 order 等于同一事实存两遍，
//     任何一处忘了重排就会出现「数组顺序 ≠ order 值」，前端/后端/导出各说一套。
//   - **没有 color_hex 字段**：它是色板的派生值，入库会把「改色板后旧数据保留旧色」
//     固化成 bug。派生唯一实现 TagColorHex。
//   - **颜色只有两个合法形态**：12 个色板 key（小写），或 `#RRGGBB`（归一为大写）。
//     未知 key 一律拒绝 —— 「存进去但显示成灰色」比 400 更难排查。

const (
	// TagMaxCount 是单模型标签上限。超过会把卡片徽章行挤成两行、移动端出现横向滚动。
	TagMaxCount = 3
	// TagLabelMaxRunes 是标签文案长度上限（按 rune，中文 12 字足够）。
	TagLabelMaxRunes = 12
)

// TagColor 是内置色板的一项。Key 是入库值，Hex 是对外下发的渲染色。
//
// 带 json tag 是因为它本身就会下发给管理台（`tag_palette`）：
// 没有 tag 时会序列化成 `{"Key":"red",...}`，前端拿到的是大写键。
type TagColor struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Hex   string `json:"hex"`
}

// TagPalette 是**唯一**色表：校验、管理台色块、对外 color_hex 都读它。
//
// 12 色刻意避开 tokens.css 的语义色（--primary/--success/--warning/--danger/--info）
// 与模态色（--m1..--m8）：标签是营销卖点，不该看起来像状态徽标或模态徽标。
var TagPalette = []TagColor{
	{"red", "红", "#E11D48"},
	{"orange", "橙", "#EA580C"},
	{"amber", "琥珀", "#D97706"},
	{"lime", "青柠", "#65A30D"},
	{"emerald", "翠绿", "#059669"},
	{"teal", "蓝绿", "#0D9488"},
	{"cyan", "青", "#0891B2"},
	{"sky", "天蓝", "#0284C7"},
	{"blue", "蓝", "#2563EB"},
	{"indigo", "靛蓝", "#4F46E5"},
	{"violet", "紫", "#7C3AED"},
	{"pink", "粉", "#DB2777"},
}

// ModelTag 是 models.tags 的元素，同时是管理台写入口的形状。
type ModelTag struct {
	Label string `json:"label"`
	Color string `json:"color"`
	// Until 是到期时刻（UTC RFC3339）。空 = 长期有效。
	// 到期后**不再对外下发**，但数据保留 —— 运营要能续期，而不是重新建一遍。
	Until string `json:"until,omitempty"`
}

// ModelTagView 是对外形状：只含渲染必需的两项。
//
// 为什么不下发 color / until：
//   - color 是入库形态（key 或 hex），客户端只需要最终颜色；
//   - until 是内部运营控制项，下发给第三方只会造出「还剩 N 天」这种平台没承诺的语义。
type ModelTagView struct {
	Label    string `json:"label"`
	ColorHex string `json:"color_hex"`
}

// TagColorHex 把入库颜色解析成渲染色。
//
// key 必须是 TagPalette 里的项（小写）；以 `#` 开头的走 NormalizeHexColor 校验。
// 未知 key 返回 ok=false —— 写入口会拒绝，读出口用原值兜底（见 viewOf）。
func TagColorHex(color string) (string, bool) {
	t := strings.TrimSpace(color)
	if strings.HasPrefix(t, "#") {
		hex, err := NormalizeHexColor(t)
		if err != nil {
			return "", false
		}
		return hex, true
	}
	for _, c := range TagPalette {
		if c.Key == strings.ToLower(t) {
			return c.Hex, true
		}
	}
	return "", false
}

// NormalizeHexColor 归一色值为 `#RRGGBB`（大写）。
//
// 必须有唯一实现：颜色会同时出现在 `<i style>`、EntityIcon 底色与前端的相等判断里，
// 大小写不一致会让"同一个颜色"看起来是两个。允许不带 `#`（原生 color input 一定带，
// 但手填的常常不带）。
//
// 供应商品牌色与标签颜色共用本函数 —— 两套 hex 归一必然在某个边界上分叉。
func NormalizeHexColor(s string) (string, error) {
	t := strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(t) != 6 {
		return "", errors.New("颜色需为 6 位十六进制")
	}
	for _, r := range t {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return "", errors.New("颜色需为 6 位十六进制")
		}
	}
	return "#" + strings.ToUpper(t), nil
}

// ParseTagUntil 把写入口的到期值归一成 UTC RFC3339（秒精度）。
//
// 接受的写法：
//   - ""：长期有效，原样返回空串；
//   - `YYYY-MM-DD`：按业务时区解释为**当日 23:59:59**（"到期日当天仍有效"是运营的直觉）；
//   - RFC3339（可带任意偏移）：转成 UTC。
//
// 不拒绝过去的时间：已过期的存量项必须能随模型整组保存，否则运营无法续期或删除它。
func ParseTagUntil(raw string, loc *time.Location) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", nil
	}
	if len(t) == len("2006-01-02") {
		d, err := time.ParseInLocation("2006-01-02", t, loc)
		if err != nil {
			return "", errors.New("到期时间格式应为 YYYY-MM-DD 或 RFC3339")
		}
		// 用 time.Date 而不是 Add(24h-1s)：夏令时切换日加 24 小时不等于"次日零点前"。
		end := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc)
		return end.UTC().Format(time.RFC3339), nil
	}
	ts, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return "", errors.New("到期时间格式应为 YYYY-MM-DD 或 RFC3339")
	}
	return ts.UTC().Format(time.RFC3339), nil
}

// TagUntilTime 解析已存储的到期时刻（UTC RFC3339）。
//
// 空值与脏值都返回 ok=false：脏数据**按长期有效**处理（不隐藏标签）——
// 因为写入侧已经校验过，读侧再报错只会让一个手改过的库把整张标签永久藏起来。
func TagUntilTime(until string) (time.Time, bool) {
	t := strings.TrimSpace(until)
	if t == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// TagExpired 判断某个 until 是否已到期（now == until 视为到期，与"到期时刻当刻起不再展示"一致）。
func TagExpired(until string, now time.Time) bool {
	ts, ok := TagUntilTime(until)
	return ok && !now.Before(ts)
}

// TagUntilDate 把 until 转成业务时区的 `YYYY-MM-DD`（管理台表单回填用）。空/脏值返回 ""。
func TagUntilDate(until string, loc *time.Location) string {
	ts, ok := TagUntilTime(until)
	if !ok {
		return ""
	}
	return ts.In(loc).Format("2006-01-02")
}

// ActiveTags 返回**对外可见**的标签（过滤已到期 + 恒非 nil）。
//
// 这是全局唯一的过期判定入口：门户、网关、文档站都读它的结果，
// 各自再判一次就会出现「门户隐藏了但 /v1/models 还在」。
func ActiveTags(tags []ModelTag, now time.Time) []ModelTagView {
	out := make([]ModelTagView, 0, len(tags))
	for _, t := range tags {
		if TagExpired(t.Until, now) {
			continue
		}
		out = append(out, TagViewOf(t))
	}
	return out
}

// TagViewOf 组装单个对外标签。
//
// 未知色值回落成原值：写入侧已校验，这里再丢弃颜色只会让一个手改过的库
// 把标签变成一段无色文本。管理台回显也走它，保证「渲染色」只有一处派生。
func TagViewOf(t ModelTag) ModelTagView {
	hex, ok := TagColorHex(t.Color)
	if !ok {
		hex = t.Color
	}
	return ModelTagView{Label: t.Label, ColorHex: hex}
}

// DecodeTags 解码 JSONB 列。nil / 非数组 / 脏数据一律返回 nil（由调用方决定兜底成 []）。
func DecodeTags(raw JSON) []ModelTag {
	if len(raw) == 0 {
		return nil
	}
	var out []ModelTag
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

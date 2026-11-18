package model

import (
	"encoding/json"
	"testing"
	"time"
)

// 本文件守标签的纯函数：色板解析、到期归一、对外投影。
// 写入约束（数量/长度/重复/去空行）在 modelcfg.tags_test.go，那边才是唯一校验入口。

func TestTagPaletteUnique(t *testing.T) {
	if len(TagPalette) != 12 {
		t.Fatalf("内置色板应为 12 色，实际 %d", len(TagPalette))
	}
	keys := map[string]bool{}
	hexes := map[string]bool{}
	for _, c := range TagPalette {
		if c.Key == "" || c.Label == "" || c.Hex == "" {
			t.Fatalf("色板项不完整：%+v", c)
		}
		if keys[c.Key] {
			t.Fatalf("色板 key 重复：%s", c.Key)
		}
		keys[c.Key] = true
		if hexes[c.Hex] {
			t.Fatalf("色板 hex 重复：%s", c.Hex)
		}
		hexes[c.Hex] = true
	}
}

// TestTagPaletteJSONKeys 守管理台 `tag_palette` 的键名：必须是 snake/lower，
// 不是 Go 字段名。没有 json tag 时序列化成 {"Key":"red"}，前端拿不到色板。
func TestTagPaletteJSONKeys(t *testing.T) {
	raw, err := json.Marshal(TagPalette[0])
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if got, want := string(raw), `{"key":"red","label":"红","hex":"#E11D48"}`; got != want {
		t.Fatalf("色板 JSON 形状不对：%s（期望 %s）", got, want)
	}
}

func TestTagColorHex(t *testing.T) {
	if got, ok := TagColorHex("teal"); !ok || got != "#0D9488" {
		t.Fatalf("预设 key 应解析为 hex，实际 %q/%v", got, ok)
	}
	if got, ok := TagColorHex("#0ea5e9"); !ok || got != "#0EA5E9" {
		t.Fatalf("自定义 hex 应归一为大写，实际 %q/%v", got, ok)
	}
	for _, bad := range []string{"rainbow", "", "#12", "#GGGGGG"} {
		if got, ok := TagColorHex(bad); ok {
			t.Fatalf("非法的 %q 不应解析成功（得到 %q）", bad, got)
		}
	}
}

func TestParseTagUntil(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("本机缺时区数据")
	}
	cases := []struct{ in, want string }{
		{"", ""},
		{"2026-12-31", "2026-12-31T15:59:59Z"}, // 业务时区当日末刻
		{"2026-12-31T00:00:00Z", "2026-12-31T00:00:00Z"},
		{"2026-12-31T08:00:00+08:00", "2026-12-31T00:00:00Z"},
	}
	for _, c := range cases {
		got, err := ParseTagUntil(c.in, loc)
		if err != nil {
			t.Fatalf("%q 解析失败：%v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q 解析为 %q，期望 %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"31/12/2026", "2026-13-01", "明天"} {
		if _, err := ParseTagUntil(bad, loc); err == nil {
			t.Fatalf("非法到期时间 %q 应报错", bad)
		}
	}
}

func TestTagExpired(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if TagExpired("", now) {
		t.Fatal("空 until 是长期有效")
	}
	if !TagExpired("2026-11-07T12:00:00Z", now) {
		t.Fatal("now == until 应视为已到期（到期时刻当刻起不再展示）")
	}
	if TagExpired("2026-11-07T12:00:01Z", now) {
		t.Fatal("未到期不应判过期")
	}
	// 脏数据按长期有效处理：不隐藏标签比"一个手改过的库让标签永久消失"更安全。
	if TagExpired("不是时间", now) {
		t.Fatal("无法解析的 until 应按长期有效处理")
	}
}

func TestActiveTagsKeepsOrderAndFiltersExpired(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tags := []ModelTag{
		{Label: "已过期", Color: "red", Until: "2026-11-07T11:59:59Z"},
		{Label: "未到期", Color: "teal", Until: "2026-11-07T12:00:01Z"},
		{Label: "长期", Color: "pink"},
	}
	got := ActiveTags(tags, now)
	if len(got) != 2 || got[0].Label != "未到期" || got[1].Label != "长期" {
		t.Fatalf("过期过滤或顺序不对：%v", got)
	}
	if got[0].ColorHex != "#0D9488" {
		t.Fatalf("渲染色应由色板解析，实际 %q", got[0].ColorHex)
	}
	// 恒非 nil：nil 序列化成 null 会让前端 .length 崩。
	if empty := ActiveTags(nil, now); empty == nil || len(empty) != 0 {
		t.Fatalf("空输入应返回空切片而不是 nil：%#v", empty)
	}
}

func TestTagUntilDate(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("本机缺时区数据")
	}
	// 15:59:59Z = 次日 00:00 前一刻（北京时间仍是 12-31）
	if got := TagUntilDate("2026-12-31T15:59:59Z", loc); got != "2026-12-31" {
		t.Fatalf("业务日期应为 2026-12-31，实际 %q", got)
	}
	if got := TagUntilDate("", loc); got != "" {
		t.Fatalf("空 until 应返回空日期，实际 %q", got)
	}
}

func TestDecodeTags(t *testing.T) {
	if got := DecodeTags(nil); got != nil {
		t.Fatalf("nil 列应返回 nil，实际 %v", got)
	}
	if got := DecodeTags(JSON("[]")); len(got) != 0 {
		t.Fatalf("[] 应返回空切片，实际 %v", got)
	}
	if got := DecodeTags(JSON("不是 json")); got != nil {
		t.Fatalf("脏数据应返回 nil（不 panic），实际 %v", got)
	}
	got := DecodeTags(JSON(`[{"label":"限时免费","color":"red"}]`))
	if len(got) != 1 || got[0].Label != "限时免费" {
		t.Fatalf("正常解码失败：%v", got)
	}
}

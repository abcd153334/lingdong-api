package money

import (
	"encoding/json"
	"testing"
)

// Decimal 必须同时吃字符串与 number，且 number 分支不得经过 float64。
func TestDecimalUnmarshal(t *testing.T) {
	cases := []struct {
		raw  string
		want int64 // -1 表示应报错
	}{
		{`"10.5"`, 10_500_000},
		{`10.5`, 10_500_000},
		{`"0.1"`, 100_000},
		{`0.1`, 100_000},
		{`"10"`, 10_000_000},
		{`10`, 10_000_000},
		{`null`, -1},
		{`""`, -1},
		{`"1e3"`, -1},
		{`1e3`, -1},
		{`"-5"`, -1},
		{`-5`, -1},
		{`"1,000"`, -1},
		{`true`, -1},
	}
	for _, c := range cases {
		var d Decimal
		if err := json.Unmarshal([]byte(c.raw), &d); err != nil {
			if c.want == -1 {
				continue
			}
			t.Fatalf("%s: 反序列化失败: %v", c.raw, err)
		}
		got, err := d.Micro()
		if c.want == -1 {
			if err == nil {
				t.Fatalf("%s: 期望解析失败，实际得 %d", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: 解析失败: %v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("%s: 期望 %d 微，实际 %d 微", c.raw, c.want, got)
		}
	}
}

// 0.1 这类值若经过 float64 会漂；这里锁住 JSON 原文直通 Parse 的行为。
func TestDecimalNoFloatDrift(t *testing.T) {
	for _, raw := range []string{`0.29`, `"0.29"`, `1234567.891011`, `"1234567.891011"`} {
		var d Decimal
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		trimmed := raw
		if trimmed[0] == '"' {
			trimmed = trimmed[1 : len(trimmed)-1]
		}
		want, err := Parse(trimmed)
		if err != nil {
			t.Fatalf("%s: Parse 失败: %v", raw, err)
		}
		got, err := d.Micro()
		if err != nil || got != want {
			t.Fatalf("%s: 期望 %d，实际 %d (err=%v)", raw, want, got, err)
		}
	}
}

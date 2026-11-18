package money

import (
	"encoding/json"
	"fmt"
)

// Decimal 是 HTTP 入参里的**十进制金额**，同时接受 JSON 字符串与 JSON number。
//
// 为什么需要它：金额的规范形态是字符串（`money.Parse` 的唯一入口，无 float 漂移），
// 但历史前端/第三方脚本会按直觉发 number。两者只差一个引号，症状却是
// `ShouldBindJSON` 直接失败 → 前端只看到「参数错误」，排查成本极高。
//
// 关键点：number 分支把 **JSON 原文** 交给 Parse（走 json.Number），
// 绝不经过 float64 —— 否则 0.1 会先被二进制近似成 0.1000000000000000055，
// 再被 Parse 当 6 位小数截断，金额悄悄差 1 微。
//
// 注意它不是「宽松」：`"1e3"`、`"-5"`、`"1,000"` 依然被 Parse 拒掉，只是错误信息
// 从「绑定失败」变成「金额格式不正确」。
type Decimal string

// UnmarshalJSON 实现 json.Unmarshaler。
func (d *Decimal) UnmarshalJSON(b []byte) error {
	s := string(trimJSONSpace(b))
	if s == "" || s == "null" {
		*d = ""
		return nil
	}
	switch s[0] {
	case '"':
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*d = Decimal(v)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("money: 金额必须是字符串或数字")
	}
	*d = Decimal(n.String())
	return nil
}

// Micro 解析为微（规则与 Parse 完全一致）。
func (d Decimal) Micro() (int64, error) { return Parse(string(d)) }

// String 返回原始十进制文本。
func (d Decimal) String() string { return string(d) }

func trimJSONSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\n' || b[start] == '\r') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\n' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}

package payment

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxTitleRunes / maxTitleBytes 是虎皮椒对 title 的限制：≤42 个字符、不能有表情符号和 %。
	//
	// 同时限制字节数（126）是保守做法：对方服务端可能按字节校验，
	// 42 个汉字是 126 字节，刚好卡在上限。
	maxTitleRunes = 42
	maxTitleBytes = 126
)

// titleAllowedPunct 是允许出现在标题里的标点（中文为主，兼容少量英文符号）。
// 不含 %（官方明确禁止）、不含 emoji（emoji 不在任何白名单类别里，会被自然剔除）。
const titleAllowedPunct = "，。、；：！？（）()《》〈〉【】—-·_+,.:;"

// SanitizeTitle 清洗订单标题：只保留汉字/字母/数字/空格/常见标点，并按字符与字节双上限截断。
//
// 为什么必须清洗而不是直接透传：
//   - 官方明确禁止 emoji 与 %，`%` 还会让「未解码原文参与签名」的上游算出不同 hash；
//   - 套餐名由运营在管理台自由填写，可能带 emoji；
//   - 用 len() 判长度会按字节算，中文被砍到 14 个字，必须用 rune。
func SanitizeTitle(raw string) string {
	var b strings.Builder
	runes, bytes := 0, 0
	for _, r := range raw {
		if !titleRuneAllowed(r) {
			continue
		}
		w := utf8.RuneLen(r)
		if runes+1 > maxTitleRunes || bytes+w > maxTitleBytes {
			break
		}
		b.WriteRune(r)
		runes++
		bytes += w
	}
	// 截断后可能留下尾随标点（如「充值-」），去掉后更自然；只剩标点时回落默认标题。
	out := strings.TrimRight(strings.TrimSpace(b.String()), titleAllowedPunct+" ")
	if out == "" {
		return "充值"
	}
	return out
}

// titleRuneAllowed 判断单个字符是否可用于订单标题。
func titleRuneAllowed(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r):
		return true
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == ' ':
		return true
	case strings.ContainsRune(titleAllowedPunct, r):
		return true
	}
	return false
}

// TitleSafe 判断一段名称能否**原样**进入支付标题（供套餐名等自由文本配置项在保存时校验）。
//
// 与 SanitizeTitle 同源判定，但不做「拼前缀再比对」：那种写法依赖
// 「前缀长度 + 名称上限 < 标题上限」这个隐含算术，任何一侧调整都会静默改变校验结果，
// 而且无法区分「含非法字符」与「以标点结尾」这两种不同的错误原因。
//
// 拒绝的两类输入：
//   - 含白名单外字符（emoji、%、&、# 等）—— 上游标题会把它剔掉，导致运营看到的订单标题与配置不一致；
//   - 以标点或空格结尾 —— SanitizeTitle 会 TrimRight 掉，标题与名称不一致。
//
// 长度只受 maxTitleRunes 约束；业务侧的 1–32 字上限由调用方单独校验（错误文案更准确）。
func TitleSafe(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > maxTitleRunes {
		return false
	}
	for _, r := range s {
		if !titleRuneAllowed(r) {
			return false
		}
	}
	return strings.TrimRight(s, titleAllowedPunct+" ") == s
}

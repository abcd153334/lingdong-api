// Package textx 提供文本处理的小工具（截断/摘要），统一按「字符（rune）」语义操作。
//
// 背景：数据库 error_msg/上游错误摘要等列是 varchar(N)（PG 按字符计数），
// 若按字节切片截断会把 UTF-8 多字节字符切成非法序列，PG 在写入时报
// `invalid byte sequence for encoding "UTF8"`（SQLSTATE 22021），
// 导致结算/释放事务整体回滚。所有截断必须经本包。
package textx

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// TruncateRunes 按字符截断 s，最多保留 n 个字符；n<=0 返回空串。
// 不追加省略号（列宽即上限，避免多占字符）。
func TruncateRunes(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	// 先修非法 UTF-8：下面的「未超限直接返回」会把原文原样放行，
	// 而非法字节一旦写进 PG 的 text/varchar 列就会报 22021，
	// 连带整个结算/释放事务回滚。合法串走原路，不额外分配。
	s = validUTF8(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// TruncateRunesEllipsis 按字符截断并在末尾追加省略号（总长不超过 n）。
func TruncateRunesEllipsis(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	s = validUTF8(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string([]rune(s)[:n-1]) + "…"
}

// validUTF8 把非法字节替换为 U+FFFD。
//
// 只在确有非法字节时才做转换：合法串（绝大多数）走原路，不额外分配。
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// Snippet 把上游响应体转为可落库的错误摘要：先按字符截断，再压平换行与多余空白。
// 用于 error_msg 等 500 字符列。
func Snippet(body []byte, n int) string {
	if len(body) == 0 || n <= 0 {
		return ""
	}
	s := strings.ReplaceAll(string(body), "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, n)
}

// SensitiveQueryKeys 是**不允许进日志/报文留痕**的 query 键。
//
// 这是唯一清单：访问日志（middleware）与上游抓包（netguard）过去各写一份，
// 一份有 signature/sig、另一份有 password，于是「哪个泄漏」取决于走的是哪条路径。
var SensitiveQueryKeys = []string{
	"key", "api_key", "apikey", "token", "access_token", "secret",
	"password", "signature", "sig", "sign",
}

// IsSensitiveQueryKey 判断键是否需要脱敏（**大小写不敏感**）。
//
// 必须 EqualFold：鉴权侧取钥用的是 EqualFold，`?Key=` 能过鉴权；
// 脱敏侧若区分大小写，这个请求就会带着明文密钥进访问日志 ——
// 一个只影响日志的判定差异，恰好是唯一会长期留存密钥的地方。
func IsSensitiveQueryKey(k string) bool {
	for _, s := range SensitiveQueryKeys {
		if strings.EqualFold(k, s) {
			return true
		}
	}
	return false
}

// RedactValues 就地把 q 里的敏感键值替换成 REDACTED，返回是否发生了改动。
//
// 打码而不是删键：保留「这里曾有参数」的信息，排障时能区分
// 「客户端没传 key」与「传了但已脱敏」；删键会让两者看起来一样。
func RedactValues(q url.Values) bool {
	changed := false
	for k := range q {
		if IsSensitiveQueryKey(k) {
			q.Set(k, "REDACTED")
			changed = true
		}
	}
	return changed
}

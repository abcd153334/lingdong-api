package conf

import "testing"

// TestCallIOBodyMode 锁住默认值与收紧口径。
//
// 默认 full：2026-11-04 的故障里，排障者只看得到入站摘要，把「平台配置错」
// 误判成「下游返回异常」。留全文的代价只是存储，丢排障信息的代价是一次误诊。
func TestCallIOBodyMode(t *testing.T) {
	cases := map[string]string{
		"":        "full", // 默认留正文（脱敏仍生效）
		"ful":     "full", // 拼错不收紧：宁可多存，不可少存
		"FULL ":   "full",
		"full":    "full",
		"meta":    "meta",
		"SUMMARY": "meta",
		"off":     "meta",
		"0":       "meta",
		"false":   "meta",
	}
	for in, want := range cases {
		if got := callIOBodyMode(in); got != want {
			t.Fatalf("callIOBodyMode(%q) = %q，期望 %q", in, got, want)
		}
	}
}

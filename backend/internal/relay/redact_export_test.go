package relay

import (
	"strings"
	"testing"
)

// TestRedactUpstreamExported 校验导出后的脱敏入口在**无渠道上下文**时仍然有效。
//
// 这是 B7 的接口约定：异步任务链路（task.Fail / worker）拿不到渠道与密钥，
// 只能做通用层脱敏。若导出时把 nil 渠道当错误直接返回原文，
// 上层以为已经脱敏，实际上 `sk-xxx` 会原样落进 usages.error_msg（门户可见）。
func TestRedactUpstreamExported(t *testing.T) {
	raw := "upstream 401: Authorization: Bearer sk-abcdefgh12345 / ak-abcdefgh12345"
	got := RedactUpstream(raw, nil, "")

	for _, leak := range []string{"sk-abcdefgh12345", "ak-abcdefgh12345"} {
		if strings.Contains(got, leak) {
			t.Fatalf("密钥 %q 未被脱敏: %q", leak, got)
		}
	}
	if !strings.Contains(got, "<redacted-key>") {
		t.Fatalf("应写入脱敏占位符，实际 %q", got)
	}
	if !strings.Contains(got, "401") {
		t.Fatalf("应保留可读状态码，实际 %q", got)
	}
}

// TestRedactUpstreamKeepsPlainText 无可脱敏内容时不得改写原文（避免日志失真）。
func TestRedactUpstreamKeepsPlainText(t *testing.T) {
	const plain = "上游任务失败：内容审核未通过"
	if got := RedactUpstream(plain, nil, ""); got != plain {
		t.Fatalf("普通文案被改写: %q", got)
	}
	if got := RedactUpstream("", nil, ""); got != "" {
		t.Fatalf("空串应原样返回，实际 %q", got)
	}
}

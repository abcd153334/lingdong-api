package logs

import (
	"strings"
	"testing"
	"time"
)

// TestKindOf 覆盖事件类型派生（§4.9.9）。
func TestKindOf(t *testing.T) {
	cases := []struct {
		status     string
		httpStatus int
		errorCode  string
		want       string
	}{
		{"settled", 200, "", "consume"},
		{"pending", 0, "", "pending"},
		{"released", 499, "", "client_abort"},
		{"released", 500, "", "upstream_error"},
		{"released", 0, "", "upstream_error"},
		{"released", 400, "", "error"},
		{"released", 429, "", "error"},
		// 额度不足：协议面已从 402 对齐成 403，必须靠业务码才分得出来。
		// 三条覆盖「新行按码」「历史行按 402 回落」「403 权限错仍归鉴权」。
		{"rejected", 403, "insufficient_balance", "insufficient_balance"},
		{"rejected", 402, "", "insufficient_balance"},
		{"rejected", 403, "forbidden", "auth_error"},
		{"rejected", 401, "invalid_api_key", "auth_error"},
		{"rejected", 429, "rate_limited", "rate_limited"},
		{"rejected", 400, "invalid_param", "invalid_request"},
	}
	for _, tc := range cases {
		if got := kindOf(tc.status, tc.httpStatus, tc.errorCode); got != tc.want {
			t.Errorf("kindOf(%s,%d,%s)=%s，期望 %s", tc.status, tc.httpStatus, tc.errorCode, got, tc.want)
		}
	}
}

// TestKindCond 派生类型到 SQL 条件映射齐全（筛选与展示同口径）。
func TestKindCond(t *testing.T) {
	for _, kind := range []string{"consume", "pending", "client_abort", "upstream_error", "error"} {
		if _, ok := kindCond(kind); !ok {
			t.Errorf("kind=%s 缺少 SQL 映射", kind)
		}
	}
	if _, ok := kindCond("unknown"); ok {
		t.Error("未知类型不应产生条件")
	}
	for _, status := range []string{"ok", "429", "err", "5xx"} {
		if _, ok := statusCond(status); !ok {
			t.Errorf("status=%s 缺少 SQL 映射", status)
		}
	}
	if _, ok := statusCond("unknown"); ok {
		t.Error("未知状态不应产生条件")
	}
}

// TestWhereFiltersChannelKeyID 锁死「按上游密钥筛日志」的条件构造。
//
// 三条要求：① 空值不产生条件（不带筛选时 SQL 与原来逐字相同）；
// ② 键 ID 只走占位符，绝不拼进 SQL 文本；③ 单值时参数恰好一个。
// keyID 是 32 位 hex，看着「安全」，但它来自 URL 查询串，拼进 SQL 就是注入口。
func TestWhereFiltersChannelKeyID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"

	cond, args := where(Filter{ChannelKeyID: id})
	if !strings.Contains(cond, "channel_key_id = ?") {
		t.Fatalf("缺少 channel_key_id 条件: %s", cond)
	}
	if strings.Contains(cond, id) {
		t.Fatalf("键 ID 被直接拼进 SQL 文本（注入面）: %s", cond)
	}
	if len(args) != 1 || args[0] != id {
		t.Fatalf("参数化错误: %#v", args)
	}

	// 注入试探：值里带引号引号也只有参数化能挡住。
	const evil = "x' OR 1=1 --"
	cond2, args2 := where(Filter{ChannelKeyID: evil})
	if strings.Contains(cond2, "OR 1=1") {
		t.Fatalf("注入串进入 SQL 文本: %s", cond2)
	}
	if len(args2) != 1 || args2[0] != evil {
		t.Fatalf("注入串未按参数传递: %#v", args2)
	}

	// 不筛选时必须一条都不产生 —— 否则默认查询会被悄悄收窄。
	empty, emptyArgs := where(Filter{})
	if strings.Contains(empty, "channel_key_id") || len(emptyArgs) != 0 {
		t.Fatalf("空筛选仍产生了条件: %q %#v", empty, emptyArgs)
	}
}

// TestCSVColumnsFrozen 导出列契约（E5）逐列冻结：表头与数据行列数一致。
//
// 表头已按设计文档 §4.9.9 的 E5 冻结契约补齐到 22 列（v2.0 起生效）：
// 原先只有 13 列，缺少各 token 通道、HTTP 状态、请求号等排障必需项，
// 导出后仍要回页面逐条点开才能定位问题。
func TestCSVColumnsFrozen(t *testing.T) {
	// 表头已去 USD 后缀 + 新增「币种」列（W18 破坏性变更）：
	// 一列金额在多币种下可能是 ¥ 也可能是 $，表头写死 USD 等于让文件撒谎。
	const header = "时间,用户,密钥名,模型,模态,事件类型,状态,HTTP," +
		"输入tok,输出tok,缓存tok,图片tok,音频入tok,音频出tok," +
		"渠道,分组,耗时ms,费用,成本,币种,请求ID,上游请求ID,IP"
	items := []Item{{
		CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local),
		Username:  "admin", KeyName: "默认KEY", Model: "gpt-4o-mini", ChannelName: "OpenAI",
		Modality: "text", Status: "settled", HTTPStatus: 200, PromptTokens: 10, CompletionTokens: 20,
		TotalTokens: 30, Charge: 250000, LatencyMs: 320,
		// 币种取**行内快照**：历史记录按当时币种记账，不该跟着后来的切换变味。
		Currency: "USD",
	}}
	csv := CSV(items)
	lines := strings.Split(strings.TrimRight(csv, "\n"), "\n")
	if lines[0] != header {
		t.Fatalf("表头漂移:\n got=%s\nwant=%s", lines[0], header)
	}
	if got := len(strings.Split(lines[1], ",")); got != len(strings.Split(header, ",")) {
		t.Fatalf("数据列数 %d ≠ 表头列数 %d", got, len(strings.Split(header, ",")))
	}
	// 金额列 = 250000 微 = 0.250000；成本列 Cost=0 → 0.000000；币种 USD。
	if !strings.Contains(lines[1], ",0.250000,0.000000,USD,") {
		t.Fatalf("金额/币种列换算错误: %s", lines[1])
	}
}

// TestCSVEscaping 逗号/引号/换行的单元格做整体加引号转义。
func TestCSVEscaping(t *testing.T) {
	if got := Esc(`a,b`); got != `"a,b"` {
		t.Errorf("逗号未转义: %s", got)
	}
	if got := Esc(`a"b`); got != `"a""b"` {
		t.Errorf("引号未转义: %s", got)
	}
	if got := Esc("plain"); got != "plain" {
		t.Errorf("普通文本不应加引号: %s", got)
	}
}

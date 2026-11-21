package logs

import (
	"encoding/json"
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
// 表头已按设计文档 §4.9.9 的 E5 冻结契约补齐（v2.0 起 23 列）：
// 原先只有 13 列，缺少各 token 通道、HTTP 状态、请求号等排障必需项，
// 导出后仍要回页面逐条点开才能定位问题。
//
// 2026-11 追加「应用ID/应用」两列 → 25 列。**只追加在尾部**：
// 按索引取列的旧对账脚本不受影响，插在中间就是静默错列。
func TestCSVColumnsFrozen(t *testing.T) {
	// 表头已去 USD 后缀 + 新增「币种」列（W18 破坏性变更）：
	// 一列金额在多币种下可能是 ¥ 也可能是 $，表头写死 USD 等于让文件撒谎。
	const header = "时间,用户,密钥名,模型,模态,事件类型,状态,HTTP," +
		"输入tok,输出tok,缓存tok,图片tok,音频入tok,音频出tok," +
		"渠道,分组,耗时ms,费用,成本,币种,请求ID,上游请求ID,IP,应用ID,应用"
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
	// 尾部两列：app_id=0 时必须写「平台直连」，空白会被 Excel 读成"缺字段"。
	if !strings.HasSuffix(lines[1], ",0,平台直连") {
		t.Fatalf("应用列应为 0/平台直连，实际行尾：%s", lines[1])
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

// TestWhereFiltersByApp 应用筛选条件必须只在 AppScoped 时出现。
//
// 两条都要断：① scoped 时条件进了 WHERE 且参数顺序正确；
// ② 未 scoped 时**不能**出现 —— 否则门户/默认列表会莫名其妙只看平台直连。
func TestWhereFiltersByApp(t *testing.T) {
	cond, args := where(Filter{AppScoped: true, AppID: 7})
	const want = "key_id IN (SELECT id FROM api_keys WHERE app_id = ?)"
	if !strings.Contains(cond, want) {
		t.Fatalf("缺少应用条件: %q", cond)
	}
	if len(args) != 1 || args[0] != uint64(7) {
		t.Fatalf("应用条件参数错误: %v", args)
	}

	if cond, _ := where(Filter{AppID: 7}); strings.Contains(cond, "api_keys") {
		t.Fatalf("AppScoped=false 时不应出现应用条件: %q", cond)
	}
	if cond, _ := where(Filter{}); cond != "" {
		t.Fatalf("空筛选必须产出空条件（否则 COUNT 与 SELECT 的拼接会多一个 AND）: %q", cond)
	}
}

// TestPortalItemHasNoAppAttribution 门户投影不得下发应用归属字段。
//
// 结构上本来就挡住了（PortalItemOf 是逐字段白名单，不拷贝 Item），
// 这条用例是"以后有人图省事直接返回 Item"的守门：应用归属属于管理台口径，
// 终端看到的应该是自己的密钥，而不是我们在后台怎么给数据归类。
func TestPortalItemHasNoAppAttribution(t *testing.T) {
	it := PortalItemOf(Item{ID: 1, AppID: 5, AppName: "官网门户"}, "", 0, 0, 0)
	raw, err := json.Marshal(it)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), "app_id") || strings.Contains(string(raw), "app_name") {
		t.Fatalf("门户调用记录不应下发应用归属: %s", raw)
	}
}

package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/service/logs"
	"lingdong-api/internal/service/order"
	"lingdong-api/internal/service/pricing"
	"lingdong-api/internal/service/stats"
	"lingdong-api/internal/service/task"
	"lingdong-api/internal/service/upload"
)

// 用户端「泄漏扫描」门禁（本方案的核心防线）。
//
// 思路：把每个门户/网关的对外投影 marshal 成 JSON，再按 denylist 扫键名与文案。
// 这比任何人工 checklist 都可靠 —— 以后谁加了一个 ChannelHint 字段或一句「上游」文案，
// 用例立刻红，而不是等用户发现。
//
// 不在这里覆盖的表面：管理台（/api/admin/*）。管理台**应该**看到渠道、上游、适配器，
// 把这些词一并禁掉会逼着人写豁免，最终门禁形同虚设。

// leakKeys 是不得出现在用户端 JSON 键名里的子串（小写比较）。
var leakKeys = []string{
	"channel", "upstream", "adapter", "platform",
	"bindings", "model_count", "cap_reason", "usage_source",
	"storage_backend", "cost", "raw_usage",
}

// leakWords 是不得出现在用户端文案里的词。
var leakWords = []string{"渠道", "上游", "适配器", "换渠道"}

// adminOnlyDefCodes 是**仅管理台**会返回的错误码：这些文案里出现「渠道/适配器」是合理的。
//
// 这个清单必须显式维护：新加一个用户端 Def 却带「渠道」字样，会被门禁抓住；
// 反过来，如果谁把管理台专用的 Def 拿去给门户用，也需要在这里删掉对应条目才会过。
var adminOnlyDefCodes = map[string]bool{
	errs.CodeChannelNameExists:    true, // 保存渠道时重名
	errs.CodeChannelBound:         true, // 删除渠道前检查绑定
	errs.CodeChannelConflict:      true, // 渠道被他人并发修改（版本 CAS 冲突），只在管理台保存/密钥池 PATCH 出现
	errs.CodeAdapterUnavailable:   true, // 管理台选路/注册表
	errs.CodeAdapterNotRegistered: true, // 管理台保存渠道时 adapter_id 不存在
	// L2 规格域：只在管理台「适配器」页出现。in_use 的文案必须点名「渠道」，
	// 而渠道是运营内部概念，用户端不得出现——所以整族都要登记为仅管理台。
	errs.CodeAdapterSpecExists:   true,
	errs.CodeAdapterSpecInUse:    true,
	errs.CodeAdapterSpecConflict: true,
	errs.CodeAdapterSpecInvalid:  true,
	errs.CodeModelProtected:      true, // 删除模型前检查渠道绑定
	// 通知域：这里的「通道」是邮件/短信/Webhook 的投递通道，与 AI 渠道无关，
	// 且只出现在管理台的通知配置与自检里，用户端永不返回。
	errs.CodeNotifyChannelDisabled: true,
	// AI 编排的探测闸门：文案必须点名「新建渠道 / 改上游地址」，
	// 否则运营不知道为什么要先探测。只出现在管理台提案卡上，用户端永不返回。
	errs.CodeAgentProbeRequired: true,
	// 退款域里的两条「人工处置指引」只出现在管理台订单页：它们必须点名
	// 「支付渠道后台」「上游核实凭证」，否则运营不知道该去哪里核实。
	// 用户端退款入口（/portal/*）从来不会命中这两个错误码。
	errs.CodeRefundNoPaymentID:      true,
	errs.CodeRefundEvidenceRequired: true,
	// 认证域的两条「管理台专属」：last_admin_protected 只在管理台改角色时出现，
	// mfa_setup_required 由 AdminAuth 在 /api/admin/* 上返回（前端据此跳绑定页）。
	// 两者的文案本身不含渠道类禁词，登记在这里是为了**声明归属**——
	// 将来谁把它们挪到门户端点，必须先回来删掉这条，等于强制过一遍评审。
	errs.CodeLastAdminProtected: true,
	errs.CodeMFASetupRequired:   true,
}

// TestPortalProjectionsDoNotLeak 逐个投影做 denylist 扫描。
func TestPortalProjectionsDoNotLeak(t *testing.T) {
	probe := int64(2_000_000) // 微
	snap := pricing.PublicSnapshot{
		Models: []pricing.PublicModel{{
			ID: "gpt-image-2-vip", DisplayName: "GPT Image 2 VIP",
			Vendor: "openai", VendorName: "OpenAI", VendorColor: "#10A37F",
			Modality: "image", BillingMode: model.BillingModeSpec,
			Prices:     model.Prices{Input: &probe},
			SpecPrices: model.SpecPrices{Kind: "per_image", ResolutionFixed: "1K", TierPrice: map[string]int64{"1K": probe}},
			Endpoints:  model.JSON(`["images/generations"]`),
			CreatedAt:  time.Now().Unix(),
		}},
		Vendors:  []pricing.PublicVendor{{ID: "openai", Name: "OpenAI", Color: "#10A37F"}},
		Defaults: map[string]string{"image": "gpt-image-2-vip"},
	}

	usageItem := logs.PortalItemOf(logs.Item{
		ID: 7, Model: "gpt-image-2-vip", Modality: "image", Kind: "relay",
		Status: "released", HTTPStatus: 503, CallKind: "sync", LatencyMs: 1200, Charge: 0,
		ChannelID: 3, ChannelName: "多米API", AdapterID: "builtin:duomi",
		ErrorMsg:    "上游提交失败：doubao-seedance-2-0-260128 拒绝",
		UsageSource: "upstream", Cost: 999, IP: "10.0.0.1",
		UpstreamRequestID: "req_upstream_1", Group: "default",
		CreatedAt: time.Now(),
	}, "", 0, 0, 0)

	detail := logs.PortalDetail(&logs.Detail{
		Item: logs.Item{
			ID: 8, Model: "gpt-4o", Modality: "chat", Kind: "relay",
			Status: "settled", CallKind: "sync", CreatedAt: time.Now(),
			ChannelID: 3, ChannelName: "多米API", AdapterID: "builtin:duomi",
		},
		BillingMode: "per_call",
		CapReason:   "frozen3x",
		IO: &logs.IOView{
			ReqBytes: 1024, RespBytes: 4096, SSEFrames: 0,
			RespRaw: `{"choices":[{"message":{"content":"你好"}}]}`,
			Media: []model.MediaItem{{
				Kind: "image", MIME: "image/png", Bytes: 2048,
				URL: "https://cdn.example-vendor.com/a.png", ArtifactIndex: -1,
			}},
		},
	})

	taskItem := task.PortalView(&model.Task{
		ID: 9, Model: "video-multimodal", Modality: "video", Kind: "video",
		Status: task.StatusFailed, Frozen: 1000,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, "/api/portal/tasks", nil)

	fileItem := upload.View(&model.Upload{
		ID: "01J8ZQ7W6Y5X4V3T2S1R0Q9P8N", Filename: "cat.png", SafeName: "cat.png",
		SizeBytes: 1024, ContentType: "image/png", StorageBackend: "s3",
		ExpiresAt: time.Now().Add(time.Hour),
	}, "生产密钥", "https://api.example.com")

	// 订单域：内部 View 带渠道三件套（管理台要这些），投影后必须一个都不剩。
	orderView := order.PortalOrderViewOf(order.View{
		ID: 11, UserID: 3, Username: "alice", OutTradeNo: "LD01J8", TradeNo: "4200001234",
		AmountCent: 1000, Currency: "CNY", Credited: 69444, Status: "pending",
		PayChannel: "虎皮椒-微信支付", PayChannelID: "xunhupay-wx", PayProtocol: "xunhupay",
		PayMethod: "wechat", PayMethodLabel: "微信支付",
		PayError: "虎皮椒返回 sign error", CreatedAt: time.Now(),
		RefundNo: "RF01J8", RefundError: "上游退款失败",
	})
	orderCreate := order.PortalCreateResultOf(&order.CreateResult{
		ID: 11, OutTradeNo: "LD01J8", AmountCent: 1000, Currency: "CNY",
		Credited: 69444, PayChannel: "虎皮椒-微信支付", PayChannelID: "xunhupay-wx",
		PayMethod: "wechat", PayMethodLabel: "微信支付", PayProtocol: "xunhupay",
		PayURL: "https://pay.example.com/cashier", QRRef: "weixin://wxpay/bizpayurl?pr=x",
		ExpiresIn: 300, PayError: "虎皮椒：商户号未配置",
	})
	payOpt := order.PortalPayOption{Method: "wechat", Label: "微信支付"}

	// 门户首页聚合：字段全是计数与金额，任何内部概念（渠道/上游/适配器/成本）都不该出现。
	// 它挂在 /api/portal/overview 上，是最容易被顺手塞进「渠道健康」之类字段的地方。
	overviewView := &stats.UserOverviewView{
		Today: stats.UserUsageStat{
			Requests: 3, Errors: 1, Pending: 1, Charge: 1000,
			PromptTokens: 10, CompletionTokens: 5,
		},
		Month: stats.UserUsageStat{
			Requests: 30, Errors: 2, Pending: 1, Charge: 9000,
			PromptTokens: 100, CompletionTokens: 50,
		},
		MonthRecharge: 2_000_000,
		Trend: []stats.UserTrendPoint{
			{Date: "2026-03-14", Charge: 8000, Recharge: 0},
			{Date: "2026-03-15", Charge: 1000, Recharge: 2_000_000},
		},
		ActiveTasks: 1,
		GeneratedAt: time.Now().Format(time.RFC3339),
	}

	cases := []struct {
		name  string
		value any
	}{
		{"pricing.PublicSnapshot", snap},
		{"logs.PortalItem", usageItem},
		{"logs.PortalDetailView", detail},
		{"task.PortalItem", taskItem},
		{"upload.Item", fileItem},
		{"order.PortalOrderView", orderView},
		{"order.PortalCreateResult", orderCreate},
		{"order.PortalPayOption", payOpt},
		{"stats.UserOverviewView", overviewView},
	}
	for _, c := range cases {
		raw, err := json.Marshal(c.value)
		if err != nil {
			t.Fatalf("%s 序列化失败: %v", c.name, err)
		}
		var tree any
		if err := json.Unmarshal(raw, &tree); err != nil {
			t.Fatalf("%s 反解失败: %v", c.name, err)
		}
		for _, key := range collectKeys(tree) {
			low := strings.ToLower(key)
			for _, bad := range leakKeys {
				if strings.Contains(low, bad) {
					t.Fatalf("%s 泄漏字段名 %q（命中禁词 %q）：%s", c.name, key, bad, raw)
				}
			}
		}
		for _, w := range leakWords {
			if strings.Contains(string(raw), w) {
				t.Fatalf("%s 文案泄漏 %q：%s", c.name, w, raw)
			}
		}
	}
}

// TestPortalErrorMessagesDoNotLeak 扫描全部错误定义的用户可见文案。
func TestPortalErrorMessagesDoNotLeak(t *testing.T) {
	all := errs.All()
	if len(all) == 0 {
		t.Fatal("errs.All() 为空：注册表没被正确填充")
	}
	for _, d := range all {
		if adminOnlyDefCodes[d.Code] {
			continue
		}
		for _, w := range leakWords {
			if strings.Contains(d.Message, w) {
				t.Errorf("错误码 %s 的文案含 %q：%s", d.Code, w, d.Message)
			}
		}
		for _, bad := range []string{"upstream", "channel"} {
			if strings.Contains(strings.ToLower(d.Code), bad) {
				t.Errorf("错误码 %s 的 code 本身带 %q（对外可读，等同泄漏）", d.Code, bad)
			}
		}
	}
}

// collectKeys 递归收集 JSON 对象的全部键名（数组元素同样下钻）。
func collectKeys(v any) []string {
	var out []string
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			out = append(out, k)
			out = append(out, collectKeys(child)...)
		}
	case []any:
		for _, child := range node {
			out = append(out, collectKeys(child)...)
		}
	}
	return out
}

package order

import (
	"encoding/json"
	"errors"
	"testing"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/service/payment"
)

// setPayChannels 临时替换 pay_channels 内存配置（resolveChannel 读的是 conf 镜像，不需要 DB）。
func setPayChannels(t *testing.T, chs []map[string]any) {
	t.Helper()
	prev, had := conf.System().Get("pay_channels")
	raw, err := json.Marshal(chs)
	if err != nil {
		t.Fatalf("序列化渠道配置失败: %v", err)
	}
	conf.System().Set("pay_channels", raw)
	t.Cleanup(func() {
		if had {
			conf.System().Set("pay_channels", prev)
			return
		}
		conf.System().Set("pay_channels", json.RawMessage("[]"))
	})
}

func chanRow(id, name, protocol string, enabled bool, methods ...string) map[string]any {
	ms := make([]any, 0, len(methods))
	for _, m := range methods {
		ms = append(ms, m)
	}
	return map[string]any{
		"id": id, "name": name, "protocol": protocol, "enabled": enabled, "methods": ms,
		"params": map[string]any{
			"pay_address": "https://pay.example.com", "pid": "1001", "key": "k",
			"app_id": "a1", "app_secret": "s1",
		},
	}
}

// TestResolveChannelRoutesByMethod 只给方式不给渠道时必须按方式找承接渠道。
//
// 反例（旧实现）是盲取 Options()[0]：用户点支付宝，钱却从微信通道扣。
func TestResolveChannelRoutesByMethod(t *testing.T) {
	setPayChannels(t, []map[string]any{
		chanRow("xunhupay-wx", "虎皮椒（微信）", "xunhupay", true, "wechat"),
		chanRow("xunhupay-ali", "虎皮椒（支付宝）", "xunhupay", true, "alipay"),
	})

	ch, m, err := resolveChannel("", "alipay")
	if err != nil {
		t.Fatalf("按方式解析失败: %v", err)
	}
	if ch.ID != "xunhupay-ali" || m != payment.MethodAlipay {
		t.Fatalf("应路由到支付宝渠道，实际 %s/%s", ch.ID, m)
	}

	// 方式缺失时才是「收银台第一个」
	ch, m, err = resolveChannel("", "")
	if err != nil || ch.ID != "xunhupay-wx" || m != payment.MethodWechat {
		t.Fatalf("缺省应取第一项，实际 %s/%s err=%v", ch.ID, m, err)
	}
}

func TestResolveChannelRejectsUnsupportedMethod(t *testing.T) {
	setPayChannels(t, []map[string]any{
		// 易支付配了聚合：协议层无法表达，必须拒绝而不是「随便传个 type」
		chanRow("epay", "易支付", "epay", true, "wechat"),
	})
	_, _, err := resolveChannel("epay", "alipay")
	if !errors.Is(err, errs.PayMethodUnavailable) {
		t.Fatalf("渠道不支持该方式应返回 pay_method_unavailable，实际 %v", err)
	}
	// 无法识别的方式字符串同样拒绝（不猜）
	_, _, err = resolveChannel("epay", "支付宝扫码")
	if !errors.Is(err, errs.PayMethodUnavailable) {
		t.Fatalf("无法识别的方式应返回 pay_method_unavailable，实际 %v", err)
	}
}

// TestResolveChannelFallsBackWhenChannelDisabled 渠道被停用后按方式回退到别的渠道。
func TestResolveChannelFallsBackWhenChannelDisabled(t *testing.T) {
	setPayChannels(t, []map[string]any{
		chanRow("epay-off", "易支付", "epay", false, "wechat", "alipay"),
		chanRow("xunhupay-wx", "虎皮椒（微信）", "xunhupay", true, "wechat"),
	})
	ch, m, err := resolveChannel("epay-off", "wechat")
	if err != nil {
		t.Fatalf("应回退到其它微信渠道: %v", err)
	}
	if ch.ID != "xunhupay-wx" || m != payment.MethodWechat {
		t.Fatalf("回退结果不符: %s/%s", ch.ID, m)
	}

	// 该方式已无渠道可承接 → 明确报「方式不可用」，而不是「无可用渠道」
	_, _, err = resolveChannel("epay-off", "alipay")
	if !errors.Is(err, errs.PayMethodUnavailable) {
		t.Fatalf("无承接渠道时应返回 pay_method_unavailable，实际 %v", err)
	}
}

func TestResolveChannelNoOptions(t *testing.T) {
	setPayChannels(t, nil)
	if _, _, err := resolveChannel("", ""); !errors.Is(err, errs.PayMethodDisabled) {
		t.Fatalf("全停用应返回 pay_channel_disabled，实际 %v", err)
	}
}

// TestResolveRetryChannelLocksOriginal 重试支付必须锁死原渠道：换渠道后回调永远验签失败。
func TestResolveRetryChannelLocksOriginal(t *testing.T) {
	setPayChannels(t, []map[string]any{
		chanRow("xunhupay-wx", "虎皮椒（微信）", "xunhupay", true, "wechat"),
		chanRow("xunhupay-ali", "虎皮椒（支付宝）", "xunhupay", true, "alipay"),
	})

	// 原渠道仍可用：原样返回
	o := &model.Order{PayChannelID: "xunhupay-ali", PayMethod: "alipay"}
	ch, m, err := resolveRetryChannel(o)
	if err != nil || ch.ID != "xunhupay-ali" || m != payment.MethodAlipay {
		t.Fatalf("重试应保持原渠道，实际 %s/%s err=%v", ch.ID, m, err)
	}

	// 原渠道被删除：绝不回退到微信渠道
	o2 := &model.Order{PayChannelID: "gone", PayMethod: "alipay"}
	if _, _, err := resolveRetryChannel(o2); !errors.Is(err, errs.PayMethodDisabled) {
		t.Fatalf("原渠道不存在时应报错而不是换渠道，实际 %v", err)
	}

	// 历史订单的方式是中文原文：仍要能重试（不能因为迁移没改订单快照就打死续付）
	o3 := &model.Order{PayChannelID: "xunhupay-wx", PayMethod: "微信支付"}
	ch3, m3, err := resolveRetryChannel(o3)
	if err != nil || ch3.ID != "xunhupay-wx" || m3 != payment.MethodWechat {
		t.Fatalf("历史中文方式应可重试，实际 %s/%s err=%v", ch3.ID, m3, err)
	}
}

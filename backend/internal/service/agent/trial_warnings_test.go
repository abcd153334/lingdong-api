package agent

import (
	"context"
	"strings"
	"testing"

	"lingdong-api/internal/model"
	// 只为副作用：注册内置适配器。缺了它，渠道保存会报「适配器不存在或未启用」，
	// 与本用例要验证的东西完全无关。
	_ "lingdong-api/internal/relay/adapters"
)

// 试跑是「提案能不能落地」的唯一判据，它跑完会回滚。告警（模型无法被渠道承接）
// 必须能从回滚里带出来 —— 否则 AI 拿到的永远是「预演通过」，而首次调用才炸。
//
// 这个用例同时锁三件事：告警能透出、事务真的回滚、库里零残留。
func TestTrialKeepsWarningsAfterRollback(t *testing.T) {
	gdb := withAgentDB(t)
	if err := gdb.Create(&model.Vendor{ID: "duomi", Name: "多米"}).Error; err != nil {
		t.Fatalf("种子供应商失败: %v", err)
	}

	// zzz-9 不属于任何协议族 → 多米适配器的预检必然报「未知的多米模型」。
	plan := `{"summary":"承接告警","operations":[
		{"op":"create","entity":"channel","key":"c1","data":{
			"name":"多米","adapter_id":"duomiapi","base_url":"https://duomiapi.com",
			"keys":["k"],"groups":["default"]}},
		{"op":"create","entity":"model","key":"zzz-9","data":{
			"display_name":"未知模型","vendor":"duomi","modality":"image","billing_mode":"spec",
			"endpoints":["images/generations"],
			"spec_prices":{"kind":"per_image","tier_price":{"1K":1000000},"max_n":1,"resolution_fixed":"1K"}}},
		{"op":"replace","entity":"model_bindings","key":"zzz-9","data":{
			"bindings":[{"channel_key":"c1","upstream_model":"zzz-9"}]}}
	]}`
	p, err := DecodePlan([]byte(plan))
	if err != nil {
		t.Fatalf("plan 解析失败: %v", err)
	}
	warns, err := trialApply(context.Background(), p)
	if err != nil {
		t.Fatalf("试跑应通过（模型无法承接只是告警，不是错误）：%v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("应产出 1 条承接告警，实际 %v", warns)
	}
	if !strings.Contains(warns[0], "未知的多米模型") {
		t.Fatalf("告警应带适配器原始原因，实际 %q", warns[0])
	}

	// 试跑必须整体回滚：渠道与模型都不该留下。
	var channels, models int64
	if err := gdb.Model(&model.Channel{}).Count(&channels).Error; err != nil {
		t.Fatalf("查询渠道失败: %v", err)
	}
	if err := gdb.Model(&model.Model{}).Count(&models).Error; err != nil {
		t.Fatalf("查询模型失败: %v", err)
	}
	if channels != 0 || models != 0 {
		t.Fatalf("试跑回滚不彻底：channels=%d models=%d", channels, models)
	}
}

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// TestTrialErrorPointsToOriginalIndex 守住「错误定位用原始下标」。
//
// 试跑按依赖顺序执行（规格→渠道→模型→绑定），失败时若拿**重排后**的位置
// 回复 `operations[i]`，模型改的是另一条操作：提示看着只是"不够精确"，
// 实际是把模型引向错误修改。
func TestTrialErrorPointsToOriginalIndex(t *testing.T) {
	gdb := withAgentDB(t)
	if err := gdb.Create(&model.Vendor{ID: "relayco", Name: "中转公司"}).Error; err != nil {
		t.Fatalf("种子供应商失败: %v", err)
	}

	// operations[1] 是模型（vendor 不存在）。执行顺序是 spec(0) → channel(2) → model(1)，
	// 所以"重排后的下标"会是 2，而正确值是 1。
	plan := `{"summary":"定位测试","operations":[
		{"op":"create","entity":"adapter_spec","key":"trial-x","data":{"label":"X","vendor":"relayco","spec":` + validChatSpec + `}},
		{"op":"create","entity":"model","key":"trial-m","data":{
			"display_name":"M","vendor":"ghost-vendor","modality":"chat","billing_mode":"tokens","input_price":1}},
		{"op":"create","entity":"channel","key":"trial-c","data":{
			"name":"C","adapter_id":"spec:trial-x","base_url":"https://api.b.example",
			"keys":["k"],"groups":["default"]}},
		{"op":"replace","entity":"model_bindings","key":"trial-m","data":{
			"bindings":[{"channel_key":"trial-c","upstream_model":"m"}]}}
	]}`

	_, err := CreateProposal(context.Background(), ProposalInput{Kind: "change", Plan: []byte(plan)})
	if err == nil {
		t.Fatal("vendor 不存在应被试跑拦下")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("错误应带字段定位，实际 %v", err)
	}
	if ve.Index != 1 {
		t.Fatalf("应定位到原始下标 1，实际 %d（field=%s）", ve.Index, ve.Field)
	}
	if !strings.Contains(ve.Field, "operations[1]") {
		t.Fatalf("field 应指向 operations[1]，实际 %s", ve.Field)
	}

	// 试跑必须整体回滚：不许留下半成品（规格/渠道建了又回滚不掉是最坏情况）。
	var specs, channels int64
	if err := gdb.Model(&model.AdapterSpec{}).Count(&specs).Error; err != nil {
		t.Fatalf("查询规格失败: %v", err)
	}
	if err := gdb.Model(&model.Channel{}).Count(&channels).Error; err != nil {
		t.Fatalf("查询渠道失败: %v", err)
	}
	if specs != 0 || channels != 0 {
		t.Fatalf("试跑回滚不彻底：specs=%d channels=%d", specs, channels)
	}
}

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// TestSubmitUnsupportedLands 锁死 L3 结论这条链路。
//
// 历史缺陷：submit_unsupported 用 map 拼提案，塞了 vendor / reasons /
// required_features 三个顶层键，而 DecodePlan 对未知键是严格拒绝的 ——
// 于是"判定为 L3"这条路 **100% 失败**（模型只能反复重试、最后放弃），
// 用户看到的就是"AI 既配不出来、也不沉淀结论"。
//
// 现在两边共用 Plan 结构体：结论进 plan.unsupported，与变更提案同一条解码路径。
func TestSubmitUnsupportedLands(t *testing.T) {
	gdb := withAgentDB(t)

	sess := &model.AgentSession{Title: "L3 结论", ModelID: "gpt-4o"}
	if err := gdb.Create(sess).Error; err != nil {
		t.Fatalf("造会话失败: %v", err)
	}
	env := &toolEnv{Ctx: context.Background(), SessionID: sess.ID, MessageID: 1, RunID: 1}

	args, _ := json.Marshal(map[string]any{
		"vendor":            "acme",
		"reasons":           []string{"自研 HMAC 签名", "先换 token 再调用"},
		"required_features": []string{"两步鉴权"},
	})
	got, err := toolSubmitUnsupported(env, args)
	if err != nil {
		t.Fatalf("工具不应返回错误: %v", err)
	}
	res, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("返回值形状不符: %#v", got)
	}
	if res["ok"] != true {
		t.Fatalf("应当成功沉淀结论: %#v", res)
	}
	if len(env.ProposedIDs) != 1 {
		t.Fatalf("应当记录一条提案，实际 %d 条", len(env.ProposedIDs))
	}

	var prop model.AgentProposal
	if err := gdb.First(&prop, env.ProposedIDs[0]).Error; err != nil {
		t.Fatalf("读取提案失败: %v", err)
	}
	if prop.Kind != ProposalKindUnsupported {
		t.Fatalf("提案种类应为 unsupported，实际 %q", prop.Kind)
	}
	plan, err := DecodePlan(prop.Plan)
	if err != nil {
		t.Fatalf("落库的提案必须能被解码: %v", err)
	}
	if plan.Unsupported == nil || plan.Unsupported.Vendor != "acme" {
		t.Fatalf("结论块丢失: %s", prop.Plan)
	}
	if len(plan.Unsupported.Reasons) != 2 {
		t.Fatalf("原因条数不符: %+v", plan.Unsupported)
	}
	// 结论提案没有操作：这是它与变更提案的区别，不能被结构校验误判成"空提案"。
	if len(plan.Operations) != 0 {
		t.Fatalf("结论提案不应带操作: %+v", plan.Operations)
	}
}

// TestUnsupportedPlanRejectedByChangePath 锁死两侧结构互斥：
// 结论块只能出现在 unsupported 提案里，操作清单只能出现在变更提案里。
func TestUnsupportedPlanRejectedByChangePath(t *testing.T) {
	onlyUnsupported := &Plan{Summary: "x", Unsupported: &UnsupportedPlan{Vendor: "v", Reasons: []string{"r"}}}
	if err := onlyUnsupported.ValidateStructure(ProposalKindChange); err == nil ||
		!strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("变更提案带结论块应被拒绝，实际: %v", err)
	}
	onlyOps := &Plan{Summary: "x", Operations: []Operation{{Op: OpCreate, Entity: EntityVendor, Key: "v"}}}
	if err := onlyOps.ValidateStructure(ProposalKindUnsupported); err == nil ||
		!strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("结论提案缺结论块应被拒绝，实际: %v", err)
	}
	// 结论块不完整（缺 reasons）也必须拒绝：没有原因的结论运营无法排期。
	noReason := &Plan{Summary: "x", Unsupported: &UnsupportedPlan{Vendor: "v"}}
	if err := noReason.ValidateStructure(ProposalKindUnsupported); err == nil {
		t.Fatal("缺 reasons 的结论应被拒绝")
	}
}

package docdocs

import (
	"testing"

	"lingdong-api/internal/model"
)

// TestSeedDocsIsPublishable 守一条「看起来完整、其实不能用」的坑。
//
// 出厂种子现在走「插入 → 体检 → 发布」的同一条路（见 v7_seed.go），
// 所以这条测试同时守住两件事：
//  1. 种子自己必须能过体检（否则落草稿，线上就是空页）；
//  2. 落库后的状态真的是 published（不是「体检过了但忘了改状态」）。
func TestSeedDocsIsPublishable(t *testing.T) {
	gdb := withDocDocsDB(t)
	rep, err := SeedDocs()
	if err != nil {
		t.Fatalf("播种失败: %v", err)
	}
	if len(rep.Damaged) > 0 {
		t.Fatalf("出厂种子有落草稿的条目：%v", rep.Damaged)
	}
	for _, p := range CheckAll() {
		if p.Blocking {
			t.Errorf("出厂种子自己过不了体检：%s %s %s", p.Code, p.Field, p.Message)
		}
	}
	var draft int64
	if err := gdb.Model(&model.DocNode{}).Where("status = ?", model.DocDraft).Count(&draft).Error; err != nil {
		t.Fatalf("统计草稿节点失败: %v", err)
	}
	if draft != 0 {
		t.Fatalf("出厂节点应全部已发布，实际还有 %d 个草稿", draft)
	}

	// 对话补全那篇必须真有返回字段（`choices[].delta` 这类是接入方最需要的东西）。
	b, err := LoadEndpoint("openai", "ep-completions")
	if err != nil {
		t.Fatalf("装载接口失败: %v", err)
	}
	if len(b.Variants) == 0 {
		t.Fatal("对话补全没有任何响应变体（页面上看不到返回结构）")
	}
	returns := 0
	for _, f := range b.Fields {
		if f.VariantID != nil {
			returns++
		}
	}
	if returns == 0 {
		t.Fatal("对话补全的返回字段为空（读者只能猜返回什么）")
	}
}

// TestSeedDocsIsIdempotent 守「重启不会把人工改动冲掉」。
//
// 播种每次启动都跑：它必须只补缺、不覆盖已有行；接口按 transport_doc_id 判存在
// （改名后不能被当成缺页再插一篇）。
func TestSeedDocsIsIdempotent(t *testing.T) {
	gdb := withDocDocsDB(t)
	if _, err := SeedDocs(); err != nil {
		t.Fatalf("首次播种失败: %v", err)
	}
	b, err := LoadEndpoint("openai", "ep-completions")
	if err != nil {
		t.Fatalf("装载接口失败: %v", err)
	}
	// 模拟人工改名。
	if err := gdb.Model(&model.DocEndpoint{}).Where("id = ?", b.EP.ID).
		Update("name", "人工改过的名字").Error; err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	rep, err := SeedDocs()
	if err != nil {
		t.Fatalf("二次播种失败: %v", err)
	}
	if rep.Nodes != 0 || rep.Endpoints != 0 {
		t.Fatalf("二次播种不该新增行：nodes=%d endpoints=%d", rep.Nodes, rep.Endpoints)
	}
	b2, err := LoadEndpoint("openai", "ep-completions")
	if err != nil {
		t.Fatalf("重新装载失败: %v", err)
	}
	if b2.EP.Name != "人工改过的名字" {
		t.Fatalf("二次播种覆盖了人工修改：name=%q", b2.EP.Name)
	}
	var n int64
	if err := gdb.Model(&model.DocEndpoint{}).
		Where("node_id = ? AND transport_doc_id = ?", b.EP.NodeID, *b.EP.TransportDocID).
		Count(&n).Error; err != nil {
		t.Fatalf("统计接口失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("同一契约身份被插了 %d 次（应按 transport_doc_id 判存在）", n)
	}
}

// TestSeedDoesNotOverwriteManualContent 守一条硬约束：
//
//	代码永远不能覆盖后台发布出去的内容。
//
// 旧实现的 `rebuildStaleFamilies` 在 `contract_ver` 与当前版本不等时，
// 直接 deleteNodeRows + 按种子重建 —— 运营改过的字段说明、示例、错误码全部消失，
// 只留一份磁盘 JSON 当安慰。这条测试把三种「覆盖」都钉死：
//  1. 人工写的注意事项不被契约改掉；
//  2. 人工编辑过的接口不被删除重建（id 必须不变）；
//  3. 契约版本落后的族节点不被整篇重建（节点 id 必须不变）。
func TestSeedDoesNotOverwriteManualContent(t *testing.T) {
	gdb := withDocDocsDB(t)
	if _, err := SeedDocs(); err != nil {
		t.Fatalf("首次播种失败: %v", err)
	}

	var node model.DocNode
	if err := gdb.Where("key = ?", "images-gpt-image").Take(&node).Error; err != nil {
		t.Fatalf("族节点缺失: %v", err)
	}
	var ep model.DocEndpoint
	if err := gdb.Where("node_id = ?", node.ID).Order("id ASC").Take(&ep).Error; err != nil {
		t.Fatalf("接口缺失: %v", err)
	}

	manualNotes := model.JSON(`["人工写的注意事项：出图失败不退额度"]`)
	// 同时把契约版本改成落后值：旧实现正是靠这个条件触发整族重建。
	if err := gdb.Model(&model.DocEndpoint{}).Where("id = ?", ep.ID).Updates(map[string]any{
		"notes":        manualNotes,
		"origin":       model.DocOriginManual,
		"updated_by":   uint64(7),
		"contract_ver": "0",
	}).Error; err != nil {
		t.Fatalf("模拟人工编辑失败: %v", err)
	}
	if err := gdb.Model(&model.DocNode{}).Where("id = ?", node.ID).
		Update("contract_ver", "0").Error; err != nil {
		t.Fatalf("改节点契约版本失败: %v", err)
	}

	if _, err := SeedDocs(); err != nil {
		t.Fatalf("二次播种失败: %v", err)
	}

	var after model.DocEndpoint
	if err := gdb.Where("id = ?", ep.ID).Take(&after).Error; err != nil {
		t.Fatalf("二次播种后接口消失了（被删除重建）: %v", err)
	}
	if string(after.Notes) != string(manualNotes) {
		t.Fatalf("人工注意事项被覆盖：期望 %s，实际 %s", manualNotes, after.Notes)
	}
	if after.Origin != model.DocOriginManual || after.UpdatedBy != 7 {
		t.Fatalf("人工归属被改写：origin=%s updated_by=%d", after.Origin, after.UpdatedBy)
	}

	var afterNode model.DocNode
	if err := gdb.Where("key = ?", "images-gpt-image").Take(&afterNode).Error; err != nil {
		t.Fatalf("族节点被删除: %v", err)
	}
	if afterNode.ID != node.ID {
		t.Fatalf("族节点被重建：id 从 %d 变成 %d", node.ID, afterNode.ID)
	}

	// 同族其它接口也必须还在（旧实现会整族删掉再插回来）。
	var eps int64
	if err := gdb.Model(&model.DocEndpoint{}).Where("node_id = ?", afterNode.ID).Count(&eps).Error; err != nil {
		t.Fatalf("统计接口失败: %v", err)
	}
	if eps == 0 {
		t.Fatal("族节点下的接口被清空了")
	}
}

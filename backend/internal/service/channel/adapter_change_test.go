package channel

import (
	"encoding/json"
	"testing"

	"lingdong-api/internal/model"
)

// TestUpdateClearsOptionsWhenAdapterChanges 换适配器必须清空私有配置。
//
// 不清空的后果不是"这个键不生效"，而是**整段配置**在新适配器的
// DisallowUnknownFields 下解析失败 → 该渠道绑定的一切模型 5xx
// （2026-11-04 多米渠道 12 个模型全挂即此形态，见 migrate_options.go）。
//
// 同时锁住反向：**同适配器**改名/改权重时配置必须保留 —— 否则运营改个名字就丢配置，
// 而这个损失在界面上完全看不出来（表单会回填成空）。
func TestUpdateClearsOptionsWhenAdapterChanges(t *testing.T) {
	gdb := withChannelDB(t)

	r := optionsReq("换适配器前的渠道", "autodl")
	r.AdapterOptions = json.RawMessage(`{"workflows":{"multimodal":"m1","image":"i1","text":"t1","first_last":"f1"}}`)
	item, err := Create(r, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if len(item.AdapterOptions) == 0 {
		t.Fatal("前置条件不成立：创建后应有私有配置")
	}

	// ① 同适配器改名 → 配置必须原样保留
	same := optionsReq("换适配器前的渠道（改名）", "autodl")
	same.AdapterOptions = nil
	if _, err := Update(item.ID, same, ""); err != nil {
		t.Fatalf("同适配器改名应成功：%v", err)
	}
	var row model.Channel
	if err := gdb.Where("id = ?", item.ID).First(&row).Error; err != nil {
		t.Fatalf("读库失败：%v", err)
	}
	if len(row.AdapterOptions) == 0 {
		t.Fatal("同适配器改名不应清空私有配置（改个名字就丢配置，界面上还看不出来）")
	}

	// ② 换到另一个适配器 → 配置必须清空成 NULL
	sw := optionsReq("换适配器前的渠道", "openai")
	if _, err := Update(item.ID, sw, ""); err != nil {
		t.Fatalf("换适配器应成功：%v", err)
	}
	row = model.Channel{}
	if err := gdb.Where("id = ?", item.ID).First(&row).Error; err != nil {
		t.Fatalf("读库失败：%v", err)
	}
	if len(row.AdapterOptions) != 0 {
		t.Fatalf("换适配器必须清空 adapter_options，实际残留 %s", string(row.AdapterOptions))
	}
	if row.AdapterID != "openai" {
		t.Fatalf("适配器应已更新，实际 %s", row.AdapterID)
	}
}

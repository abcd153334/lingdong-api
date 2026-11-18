package channel

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"

	// 空导入让内置适配器完成注册（openai / autodl）：能力门禁依赖注册表。
	_ "lingdong-api/internal/relay/adapters"
)

// optionsReq 用注册名构造入参。
func optionsReq(name, adapterID string) Req {
	r := channelReq(name)
	r.AdapterID = adapterID
	return r
}

// TestCreatePersistsAdapterOptions 私有配置必须落库并原样回显（前端靠它回填表单）。
func TestCreatePersistsAdapterOptions(t *testing.T) {
	gdb := withChannelDB(t)

	r := optionsReq("AutoDL 渠道", "autodl")
	r.AdapterOptions = json.RawMessage(`{"workflows":{"multimodal":"m1","image":"i1","text":"t1","first_last":"f1"},"resolution_map":{"720p":"768p"}}`)
	item, err := Create(r, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if len(item.AdapterOptions) == 0 {
		t.Fatal("出参未带回 adapter_options（前端无法回填）")
	}
	var got struct {
		Workflows     map[string]string `json:"workflows"`
		ResolutionMap map[string]string `json:"resolution_map"`
	}
	if err := json.Unmarshal(item.AdapterOptions, &got); err != nil {
		t.Fatalf("出参不是合法 JSON：%v", err)
	}
	if got.Workflows["text"] != "t1" || got.ResolutionMap["720p"] != "768p" {
		t.Fatalf("配置内容不符：%+v", got)
	}

	var row model.Channel
	if err := gdb.Where("id = ?", item.ID).First(&row).Error; err != nil {
		t.Fatalf("读库失败：%v", err)
	}
	if len(row.AdapterOptions) == 0 {
		t.Fatal("adapter_options 未落库")
	}
}

// TestUpdateReplacesAndClearsAdapterOptions 覆盖 / 清空 / 不传不修改三种语义。
func TestUpdateReplacesAndClearsAdapterOptions(t *testing.T) {
	withChannelDB(t)

	r := optionsReq("可变选项渠道", "autodl")
	r.AdapterOptions = json.RawMessage(`{"workflows":{"multimodal":"m1","image":"i1","text":"t1","first_last":"f1"}}`)
	item, err := Create(r, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}

	// 不传（nil）→ 保持原值。
	keep := optionsReq("可变选项渠道", "autodl")
	item2, err := Update(item.ID, keep, "")
	if err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if len(item2.AdapterOptions) == 0 {
		t.Fatal("不传 adapter_options 时不应清空已有配置")
	}

	// 覆盖。
	over := optionsReq("可变选项渠道", "autodl")
	over.AdapterOptions = json.RawMessage(`{"workflows":{"multimodal":"m2","image":"i2","text":"t2","first_last":"f2"}}`)
	item3, err := Update(item.ID, over, "")
	if err != nil {
		t.Fatalf("覆盖失败：%v", err)
	}
	if !strings.Contains(string(item3.AdapterOptions), `"m2"`) {
		t.Fatalf("配置未被整体替换：%s", item3.AdapterOptions)
	}

	// 显式 null → 清空（回到适配器内置默认）。
	clear := optionsReq("可变选项渠道", "autodl")
	clear.AdapterOptions = json.RawMessage(`null`)
	item4, err := Update(item.ID, clear, "")
	if err != nil {
		t.Fatalf("清空失败：%v", err)
	}
	if len(item4.AdapterOptions) != 0 {
		t.Fatalf("null 应清空配置，实际 %s", item4.AdapterOptions)
	}
}

// TestAdapterOptionsRejectedForUnsupportedAdapter 不支持私有配置的适配器必须在保存期拒绝。
func TestAdapterOptionsRejectedForUnsupportedAdapter(t *testing.T) {
	withChannelDB(t)

	r := optionsReq("OpenAI 渠道", "openai")
	r.AdapterOptions = json.RawMessage(`{"workflows":{"text":"t"}}`)
	if _, err := Create(r, ""); err == nil {
		t.Fatal("openai 适配器不接受私有配置，应拒绝保存")
	} else {
		var cfgErr *relay.ConfigError
		if !errors.As(err, &cfgErr) {
			t.Fatalf("应为 ConfigError（管理台要能定位字段），实际 %T", err)
		}
	}
}

// TestAdapterOptionsValidatedOnSave 非法配置必须保存即拒（不能等到第一次真实请求）。
func TestAdapterOptionsValidatedOnSave(t *testing.T) {
	withChannelDB(t)

	r := optionsReq("AutoDL 半配", "autodl")
	r.AdapterOptions = json.RawMessage(`{"workflows":{"text":"t1"}}`)
	if _, err := Create(r, ""); err == nil {
		t.Fatal("四项工作流只填一项应拒绝保存")
	} else if !strings.Contains(err.Error(), "必须全部填写") {
		t.Fatalf("错误文案应指出缺项，实际 %q", err.Error())
	}
}

// TestAdapterOptionsEmptyMeansDefault 空对象等价于未配置（用内置默认 workflow）。
func TestAdapterOptionsEmptyMeansDefault(t *testing.T) {
	withChannelDB(t)

	r := optionsReq("AutoDL 默认", "autodl")
	r.AdapterOptions = json.RawMessage(`{}`)
	item, err := Create(r, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if len(item.AdapterOptions) != 0 {
		t.Fatalf("{} 应视为未配置，实际 %s", item.AdapterOptions)
	}
}

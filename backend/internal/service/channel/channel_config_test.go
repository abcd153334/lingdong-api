package channel

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"

	// 空导入让内置适配器完成注册：SupportsDeclarativeConfig("openai") 依赖注册表。
	// 生产侧由 router 包导入 adapters，测试侧必须自己保证。
	_ "lingdong-api/internal/relay/adapters"
)

// declReq 用注册名构造入参（channelReq 已用 "openai"，这里保留一层显式覆盖，
// 便于后续用例换到别的内置适配器而不影响其余用例）。
func declReq(name string) Req {
	r := channelReq(name)
	r.AdapterID = "openai"
	return r
}

func TestCreatePersistsAdapterConfig(t *testing.T) {
	gdb := withChannelDB(t)

	r := declReq("声明式渠道")
	r.AdapterConfig = json.RawMessage(`{"auth_header":"x-api-key","auth_prefix":"none",
		"path_overrides":{"/v1/chat/completions":"/v4/chat"}}`)
	item, err := Create(r, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if item.AdapterConfig.AuthHeader != "x-api-key" || item.AdapterConfig.AuthPrefix != "none" {
		t.Fatalf("出参未带回配置：%+v", item.AdapterConfig)
	}
	if got := item.AdapterConfig.PathOverrides[relay.PathChatCompletions]; got != "/v4/chat" {
		t.Fatalf("路径覆盖丢失：%q", got)
	}

	var row model.Channel
	if err := gdb.Where("id = ?", item.ID).First(&row).Error; err != nil {
		t.Fatalf("读库失败：%v", err)
	}
	if len(row.AdapterConfig) == 0 {
		t.Fatal("adapter_config 未落库")
	}
}

func TestUpdateReplacesAndClearsAdapterConfig(t *testing.T) {
	withChannelDB(t)

	r := declReq("可变配置渠道")
	r.AdapterConfig = json.RawMessage(`{"auth_header":"x-api-key"}`)
	item, err := Create(r, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}

	// 覆盖：换成另一套配置。
	upd := declReq("可变配置渠道")
	upd.AdapterConfig = json.RawMessage(`{"auth_prefix":"Token "}`)
	item2, err := Update(item.ID, upd, "")
	if err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if item2.AdapterConfig.AuthHeader != "" || item2.AdapterConfig.AuthPrefix != "Token " {
		t.Fatalf("配置未被整体替换：%+v", item2.AdapterConfig)
	}

	// 清空：显式传 null。
	cleared := declReq("可变配置渠道")
	cleared.AdapterConfig = json.RawMessage("null")
	item3, err := Update(item.ID, cleared, "")
	if err != nil {
		t.Fatalf("清空失败：%v", err)
	}
	if !item3.AdapterConfig.IsZero() {
		t.Fatalf("显式 null 应清空，实际 %+v", item3.AdapterConfig)
	}
}

// TestUpdateWithoutFieldKeepsConfig 锁住「旧调用方不带该字段时不能把配置清掉」。
func TestUpdateWithoutFieldKeepsConfig(t *testing.T) {
	withChannelDB(t)

	r := declReq("保持配置渠道")
	r.AdapterConfig = json.RawMessage(`{"auth_header":"x-api-key"}`)
	item, err := Create(r, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}

	upd := declReq("保持配置渠道")
	upd.Weight = 3 // 只改别的东西，AdapterConfig 保持 nil
	item2, err := Update(item.ID, upd, "")
	if err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if item2.AdapterConfig.AuthHeader != "x-api-key" {
		t.Fatalf("未提供配置时不应清空，实际 %+v", item2.AdapterConfig)
	}
}

func TestValidateAdapterConfigRules(t *testing.T) {
	// 插件渠道不接受渠道级配置：它的鉴权与路径由插件清单决定，静默忽略会让人以为生效了。
	plugin := channelReq("插件渠道")
	plugin.AdapterID = "plugin:demo"
	plugin.AdapterConfig = json.RawMessage(`{"auth_header":"x-api-key"}`)
	if err := plugin.validate(false); err == nil {
		t.Fatal("插件渠道配声明式配置应被拒绝")
	}

	// 绝对 URL 会把渠道密钥带到别的主机。
	bad := declReq("恶意路径")
	bad.AdapterConfig = json.RawMessage(`{"path_overrides":{"/v1/chat/completions":"https://evil.com/x"}}`)
	if err := bad.validate(true); err == nil {
		t.Fatal("绝对 URL 应被拒绝")
	}

	// 改写 model 会让计费模型与上游模型不一致。
	reserved := declReq("改写 model")
	reserved.AdapterConfig = json.RawMessage(`{"request_patch":{"model":"other"}}`)
	if err := reserved.validate(true); err == nil {
		t.Fatal("改写 model 应被拒绝")
	}

	// 合法配置必须通过（否则运营根本存不进去）。
	ok := declReq("合法配置")
	ok.AdapterConfig = json.RawMessage(`{"request_patch":{"thinking.type":"enabled"}}`)
	if err := ok.validate(true); err != nil {
		t.Fatalf("合法配置不应被拒：%v", err)
	}
}

// TestToItemAdapterConfigSerializesAsObject 保证前端能按必填处理（零值必须是 {} 而不是缺失/null）。
func TestToItemAdapterConfigSerializesAsObject(t *testing.T) {
	item := toItem(&model.Channel{ID: 1, Name: "x", AdapterID: "openai"}, nil, "")
	b, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if !strings.Contains(string(b), `"adapter_config":{}`) {
		t.Fatalf("零值应序列化为 {}，实际：%s", b)
	}
}

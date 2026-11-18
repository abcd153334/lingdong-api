package modelcfg

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// 本文件覆盖 NormalizeReplace —— 人工抽屉与 AI apply 共用的唯一绑定校验入口。
//
// 三条语义是「收敛前只存在于人工路径、AI 路径没有」的那部分，必须逐条锁住：
// 漏了存量豁免 = 渠道被自动封禁后配置页锁死；漏了前后对比 = 在售模型能被清成零可用。

func mkChannel(t *testing.T, gdb *gorm.DB, name, adapterID string, status int16) uint64 {
	t.Helper()
	ch := model.Channel{
		Name: name, AdapterID: adapterID, BaseURL: "https://api.example.com",
		Keys: model.MustJSON([]string{"enc"}), MultiKeyMode: "random",
		Groups: model.MustJSON([]string{"default"}),
		Weight: 1, Priority: 1, Status: status, AutoBan: true,
	}
	if err := gdb.Select("*").Create(&ch).Error; err != nil {
		t.Fatalf("造渠道失败: %v", err)
	}
	// status 列声明 default:1，零值不会被 INSERT 写入 —— 要造停用渠道必须再补一条 UPDATE。
	if status != 1 {
		if err := gdb.Model(&model.Channel{}).Where("id = ?", ch.ID).Update("status", status).Error; err != nil {
			t.Fatalf("置渠道状态失败: %v", err)
		}
	}
	return ch.ID
}

func mkModel(t *testing.T, gdb *gorm.DB, id string, enabled bool) *model.Model {
	t.Helper()
	m := model.Model{
		ID: id, DisplayName: id, Vendor: "openai", Modality: "chat",
		BillingMode: model.BillingModeTokens, Enabled: enabled,
		// 端点必填：真实模型总有端点，夹具也必须带上 —— 否则测的是
		// 一个"存不进管理台"的非法状态。
		Endpoints: model.MustJSON([]string{"chat/completions"}),
	}
	if err := gdb.Select("*").Create(&m).Error; err != nil {
		t.Fatalf("造模型失败: %v", err)
	}
	// enabled 列声明 default:true，GORM 会把「零值 + default」换成列默认值，
	// **Select("*") 拦不住**（铁律 11）。要造未在售模型必须插入后显式置位。
	if !enabled {
		if err := gdb.Model(&model.Model{}).Where("id = ?", id).Update("enabled", false).Error; err != nil {
			t.Fatalf("置模型停用失败: %v", err)
		}
		m.Enabled = false
	}
	return &m
}

// 上游模型名是必填：它空着时同步图像桥会给上游发 model:""。
func TestNormalizeReplaceRejectsEmptyUpstream(t *testing.T) {
	gdb := withCfgDB(t)
	m := mkModel(t, gdb, "m-empty", true)
	chID := mkChannel(t, gdb, "c1", "openai", 1)

	_, _, err := NormalizeReplace(gdb, m, []Binding{{ChannelID: chID, UpstreamModel: "   ", Enabled: true}})
	if err == nil {
		t.Fatal("空上游名应被拒")
	}
	if !strings.Contains(err.Error(), "上游模型名") {
		t.Fatalf("文案应点名 upstream_model，实际 %v", err)
	}
}

// 存量豁免：渠道停用后，模型页保存（整组回传）仍必须放行。
func TestNormalizeReplaceKeepsLegacyExemption(t *testing.T) {
	gdb := withCfgDB(t)
	m := mkModel(t, gdb, "m-legacy", true)
	chID := mkChannel(t, gdb, "c1", "openai", 1)

	out, _, err := NormalizeReplace(gdb, m, []Binding{{ChannelID: chID, UpstreamModel: "u", Enabled: true}})
	if err != nil {
		t.Fatalf("首次绑定应通过：%v", err)
	}
	if err := ReplaceBindingsTx(gdb, m.ID, out); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	// 渠道被停用（运营手动关闭，或自动封禁）。
	if err := gdb.Model(&model.Channel{}).Where("id = ?", chID).Update("status", 0).Error; err != nil {
		t.Fatalf("停用渠道失败: %v", err)
	}
	// 再保存一次（只改权重）必须放行 —— 否则运营连模型名都改不了。
	if _, _, err := NormalizeReplace(gdb, m, []Binding{
		{ChannelID: chID, UpstreamModel: "u", Weight: 5, Enabled: true},
	}); err != nil {
		t.Fatalf("存量绑定应豁免渠道状态校验：%v", err)
	}
}

// 在售模型不能被改成「零可用绑定」：那会让它从目录消失但列表仍显示已启用。
func TestNormalizeReplaceBlocksZeroUsable(t *testing.T) {
	gdb := withCfgDB(t)
	m := mkModel(t, gdb, "m-onsale", true)
	chID := mkChannel(t, gdb, "c1", "openai", 1)

	out, _, err := NormalizeReplace(gdb, m, []Binding{{ChannelID: chID, UpstreamModel: "u", Enabled: true}})
	if err != nil {
		t.Fatalf("首次绑定应通过：%v", err)
	}
	if err := ReplaceBindingsTx(gdb, m.ID, out); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	// 把唯一一条绑定停用 → 可路由数从 1 变 0。
	_, _, err = NormalizeReplace(gdb, m, []Binding{{ChannelID: chID, UpstreamModel: "u", Enabled: false}})
	if err == nil {
		t.Fatal("在售模型被改成零可用应被拒")
	}
	if !strings.Contains(err.Error(), "先停用该模型") {
		t.Fatalf("错误必须给出可执行的下一步，实际 %v", err)
	}
}

// 非在售模型允许清空绑定（先解绑、再换渠道是正常操作）。
func TestNormalizeReplaceAllowsEmptyWhenOffSale(t *testing.T) {
	gdb := withCfgDB(t)
	m := mkModel(t, gdb, "m-off", false)
	chID := mkChannel(t, gdb, "c1", "openai", 1)

	out, _, err := NormalizeReplace(gdb, m, []Binding{{ChannelID: chID, UpstreamModel: "u", Enabled: true}})
	if err != nil {
		t.Fatalf("绑定应通过：%v", err)
	}
	if err := ReplaceBindingsTx(gdb, m.ID, out); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	got, notes, err := NormalizeReplace(gdb, m, nil)
	if err != nil {
		t.Fatalf("非在售模型清空绑定应放行：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("应返回空集合，实际 %v", got)
	}
	// warnings 必须是空切片（前端与 AI 会直接取 .length，nil 会被序列化成 null）。
	if notes == nil {
		t.Fatal("warnings 不能是 nil")
	}
}

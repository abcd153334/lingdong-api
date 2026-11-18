package modelcfg

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
	// 能力判定读适配器注册表：不导入就只有「未注册」一种答案。
	_ "lingdong-api/internal/relay/adapters"
)

// coverOf 是「替换绑定」语义下的端点覆盖（未知端点跳过，见 checkCoverageOf 的 strictUnknown）。
func coverOf(byID map[uint64]channelRow, items []Binding, endpoints []string) error {
	return checkCoverageOf(nil, byID, items, endpoints, false)
}

// 端点覆盖是「模型声明的每个端点都要有启用绑定能承接」的唯一实现
// （人工抽屉与 AI 提案共用 checkCoverageOf），所以它的边界必须单测：
// 绕过 SaveModelBindings 是因为模态闸门先执行，内置适配器里几乎造不出
// 「模态通过但端点不支持」的组合，只能在纯函数上构造。
func TestCheckCoverageOf(t *testing.T) {
	byID := map[uint64]channelRow{
		1: {ID: 1, Name: "视频渠道", AdapterID: "autodl", Status: 1}, // 无任何同步端点
		2: {ID: 2, Name: "通用渠道", AdapterID: "openai", Status: 1}, // 全端点支持（未实现 EndpointSupporter）
	}
	endpoints := []string{"embeddings"}

	// 只有 autodl → 端点无承接。
	err := coverOf(byID, []Binding{{ChannelID: 1, Enabled: true}}, endpoints)
	if err == nil {
		t.Fatal("端点无渠道承接应报错")
	}
	if !strings.Contains(err.Error(), "embeddings") {
		t.Fatalf("错误应点名缺失的端点，实际：%v", err)
	}

	// 换成 openai → 通过。
	if err := coverOf(byID, []Binding{{ChannelID: 2, Enabled: true}}, endpoints); err != nil {
		t.Fatalf("端点有渠道承接应通过，实际：%v", err)
	}

	// 绑定被停用 → 不算承接（停用绑定不路由，算进来就是纸面配置）。
	if err := coverOf(byID, []Binding{{ChannelID: 2, Enabled: false}}, endpoints); err != nil {
		t.Fatalf("全停用时不做端点校验，实际：%v", err)
	}

	// 未声明端点 → 直接拒绝：没有端点的模型没有任何接口，
	// 文档给不出一节、/v1/models 说不出接入路径，却会被挂上渠道。
	if err := coverOf(byID, []Binding{{ChannelID: 1, Enabled: true}}, nil); err == nil {
		t.Fatal("未声明端点应被拒绝")
	} else if !strings.Contains(err.Error(), "必须声明") {
		t.Fatalf("文案应说明必须声明端点，实际：%v", err)
	}

	// 未知端点写法不参与校验（存量别名不该锁死保存）。
	if err := coverOf(byID, []Binding{{ChannelID: 1, Enabled: true}},
		[]string{"openai-compat"}); err != nil {
		t.Fatalf("未知端点不应校验，实际：%v", err)
	}

	// 混合：未知 + 已知 → 只看已知。
	mix := []string{"openai-compat", "embeddings"}
	if err := coverOf(byID, []Binding{{ChannelID: 2, Enabled: true}}, mix); err != nil {
		t.Fatalf("已知端点被承接时应通过，实际：%v", err)
	}
	if err := coverOf(byID, []Binding{{ChannelID: 1, Enabled: true}}, mix); err == nil {
		t.Fatal("已知端点无承接时应报错")
	}
}

// TestCoverageRejectsAsyncEndpointOnWrongAdapter 守「端点按端点校验」。
//
// 改造前的判据是「该适配器能不能做异步」一个布尔，于是音乐模型能绑到 openai ——
// 保存体检通过、首次调用把音乐 body 打到视频端点，报错是 502，与真实原因无关。
// 矩阵本身在 relay/adapters 有单测，这里守的是**保存期真的会拒绝**这条链路。
func TestCoverageRejectsAsyncEndpointOnWrongAdapter(t *testing.T) {
	byID := map[uint64]channelRow{
		1: {ID: 1, Name: "OpenAI", AdapterID: "openai", Status: 1},
		2: {ID: 2, Name: "方舟", AdapterID: "volcengine-ark", Status: 1},
		3: {ID: 3, Name: "Metaso", AdapterID: "metaso", Status: 1},
		4: {ID: 4, Name: "多米", AdapterID: "duomiapi", Status: 1},
	}
	cases := []struct {
		name      string
		channelID uint64
		endpoint  string
		wantErr   bool
	}{
		{"音乐模型绑 openai", 1, "audio/music/generations", true},
		{"音乐模型绑方舟", 2, "audio/music/generations", true},
		{"音乐模型绑多米", 4, "audio/music/generations", false},
		{"歌词绑 openai", 1, "audio/lyrics", true},
		{"歌词绑多米", 4, "audio/lyrics", false},
		// 图像**两面同路径**（同步出图 / async:true 转任务），所以绑到只做同步的适配器
		// 仍然留下一个可用端点 —— 保存**允许**。异步面在提交期由
		// relay.TaskEndpointSupported 拦成明确的渠道配置错误（不是 502，也不重试），
		// 而且那个模型照样能同步出图。与音乐不同：音乐只有任务形态，没有任何可用面。
		{"图像绑 openai（只剩同步面）", 1, "images/generations", false},
		{"图像绑方舟（只剩同步面）", 2, "images/generations", false},
		{"图像绑多米（两面都可用）", 4, "images/generations", false},
		{"再生成绑方舟", 2, "video/regenerations", true},
		{"再生成绑 metaso", 3, "video/regenerations", false},
		{"视频绑 metaso", 3, "video/generations", false},
	}
	for _, c := range cases {
		err := coverOf(byID, []Binding{{ChannelID: c.channelID, Enabled: true}}, []string{c.endpoint})
		if c.wantErr {
			if err == nil {
				t.Fatalf("%s：应拒绝（声明了该端点却没有任何启用绑定能承接）", c.name)
			}
			if !strings.Contains(err.Error(), c.endpoint) {
				t.Fatalf("%s：文案应点名端点 %s，实际 %v", c.name, c.endpoint, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s：应通过，实际 %v", c.name, err)
		}
	}
}

// 端点解析只认 models.endpoints 的 JSON 数组；脏值返回空（不阻塞保存）。
func TestDecodeEndpointsString(t *testing.T) {
	m := &model.Model{Endpoints: model.MustJSON([]string{"embeddings", "chat/completions"})}
	got := decodeStrings(m.Endpoints)
	if len(got) != 2 || got[0] != "embeddings" {
		t.Fatalf("端点解析不符：%v", got)
	}
	if got := decodeStrings(nil); len(got) != 0 {
		t.Fatalf("空端点应返回空切片，实际 %v", got)
	}
}

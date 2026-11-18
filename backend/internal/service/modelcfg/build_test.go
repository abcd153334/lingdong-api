package modelcfg

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// chatPrices 是「按量、chat 模态」的合法基线（微 / 百万 tokens）。
func chatPrices() model.Prices {
	in := int64(1_000_000)
	return model.Prices{Input: &in}
}

func buildChat(t *testing.T, mutate func(*Input)) (*model.Model, error) {
	t.Helper()
	in := Input{ID: "m", DisplayName: sp("m"), Vendor: "v", Modality: "chat",
		BillingMode: model.BillingModeTokens, BillingModeSet: true, Prices: chatPrices()}
	if mutate != nil {
		mutate(&in)
	}
	return Build(in)
}

// TestBuildRejectsNegativeLanes 逐条 lane 守负价。
//
// 负价不是"便宜"，是**反向计费**：输出单价填 -1 会让每次调用变成给用户送钱，
// 而余额只会往上涨，没有任何告警。
func TestBuildRejectsNegativeLanes(t *testing.T) {
	neg := int64(-1)
	cases := map[string]func(*Input){
		"input":           func(in *Input) { in.Prices.Input = &neg },
		"output":          func(in *Input) { in.Prices.Output = &neg },
		"cache":           func(in *Input) { in.Prices.Cache = &neg },
		"cache_create":    func(in *Input) { in.Prices.CacheCreate = &neg },
		"cache_create_1h": func(in *Input) { in.Prices.CacheCreate1h = &neg },
		"image":           func(in *Input) { in.Prices.Image = &neg },
		"audio":           func(in *Input) { in.Prices.Audio = &neg },
		"audio_output":    func(in *Input) { in.Prices.AudioOutput = &neg },
	}
	for name, mutate := range cases {
		_, err := buildChat(t, mutate)
		if err == nil {
			t.Fatalf("%s 为负价时应被拒绝", name)
		}
		if !strings.Contains(err.Error(), "负数") {
			t.Fatalf("%s 的报错应说明负价，实际 %q", name, err.Error())
		}
	}

	// 按次与按规格的负价也必须拒。
	call := int64(-1)
	if _, err := Build(Input{ID: "m", Vendor: "v", Modality: "chat",
		BillingMode: model.BillingModeCall, BillingModeSet: true,
		Prices: model.Prices{Call: &call}}); err == nil || !strings.Contains(err.Error(), "负数") {
		t.Fatalf("按次负价应被拒绝，实际 %v", err)
	}
	// 规格负价：直接用负的档位秒价（per_video 不支持 default_price，
	// 那是 per_image 的兜底字段，用它做用例会因为"字段不支持"提前退出，
	// 测不到负价那条规则）。
	if _, err := Build(Input{ID: "m", Vendor: "v", Modality: "video",
		BillingMode: model.BillingModeSpec, BillingModeSet: true,
		SpecPrices: model.SpecPrices{Kind: "per_video",
			SecondPrice: map[string]int64{"720p": -1}}}); err == nil || !strings.Contains(err.Error(), "负数") {
		t.Fatalf("规格负价应被拒绝，实际 %v", err)
	}
	// 图像档位负价同样必须拒（按张计费必须同时给兜底价，否则会先撞上"缺默认价"）。
	negTier := map[string]int64{"1K": -1}
	if _, err := Build(Input{ID: "m", Vendor: "v", Modality: "image",
		BillingMode: model.BillingModeSpec, BillingModeSet: true,
		SpecPrices: model.SpecPrices{Kind: "per_image", TierPrice: negTier}}); err == nil || !strings.Contains(err.Error(), "负数") {
		t.Fatalf("图像档位负价应被拒绝，实际 %v", err)
	}
}

// TestBuildRejectsHalfFree：「输入免费、输出收费」的账单无法向用户解释。
func TestBuildRejectsHalfFree(t *testing.T) {
	zero, out := int64(0), int64(1_500_000)
	_, err := Build(Input{ID: "m", Vendor: "v", Modality: "chat",
		BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices: model.Prices{Input: &zero, Output: &out}})
	if err == nil {
		t.Fatal("半免费结构应被拒绝")
	}
	// 全 0 合法：显式免费模型。
	if _, err := Build(Input{ID: "m", Vendor: "v", Modality: "chat",
		BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices: model.Prices{Input: &zero, Output: &zero}}); err != nil {
		t.Fatalf("全 0 应合法: %v", err)
	}
}

// TestBuildNormalizesSpecPrices 落库的必须是**归一化结果**。
//
// 只校验不归一化是历史上的真实错价来源：提交 `1080P` 能过校验，
// 但运行期按 `1080p` 查档位永不命中 → 静默回落到默认价。
func TestBuildNormalizesSpecPrices(t *testing.T) {
	m, err := Build(Input{ID: "vid", Vendor: "v", Modality: "video",
		BillingMode: model.BillingModeSpec, BillingModeSet: true,
		SpecPrices: model.SpecPrices{Kind: "per_video", MaxSeconds: i64p(10),
			SecondPrice: map[string]int64{"1080P": 12000, "2K": 20000}}})
	if err != nil {
		t.Fatalf("应通过: %v", err)
	}
	if _, ok := m.SpecPrices.SecondPrice["1080p"]; !ok {
		t.Fatalf("档位键未归一化为小写: %+v", m.SpecPrices.SecondPrice)
	}
	if _, ok := m.SpecPrices.SecondPrice["1080P"]; ok {
		t.Fatalf("落库仍含未归一化的键: %+v", m.SpecPrices.SecondPrice)
	}
	if _, ok := m.SpecPrices.SecondPrice["2k"]; !ok {
		t.Fatalf("2K 应归一化为 2k: %+v", m.SpecPrices.SecondPrice)
	}
	// 档位不再有白名单：`8k` / `1440p` 这类词表外的键也必须能存（上游支持与否由官方文档定）。
	if _, err := Build(Input{ID: "vid", Vendor: "v", Modality: "video",
		BillingMode: model.BillingModeSpec, BillingModeSet: true,
		SpecPrices: model.SpecPrices{Kind: "per_video", MaxSeconds: i64p(10),
			SecondPrice: map[string]int64{"8k": 20000}}}); err != nil {
		t.Fatalf("8k 档应被接受（档位由运营自由配置）: %v", err)
	}
	// 真正该拒的是**不可落库**的键（空 / 逗号 / 过长），不是"词表外的档位"。
	if _, err := Build(Input{ID: "vid", Vendor: "v", Modality: "video",
		BillingMode: model.BillingModeSpec, BillingModeSet: true,
		SpecPrices: model.SpecPrices{Kind: "per_video", MaxSeconds: i64p(10),
			SecondPrice: map[string]int64{"": 20000}}}); err == nil {
		t.Fatal("空档位键应被拒绝")
	}
	if m, err := Build(Input{ID: "vid", Vendor: "v", Modality: "video",
		BillingMode: model.BillingModeSpec, BillingModeSet: true,
		SpecPrices: model.SpecPrices{Kind: "per_video", MaxSeconds: i64p(10),
			SecondPrice: map[string]int64{"4K": 20000}}}); err != nil {
		t.Fatalf("4k 档应被接受（Seedance 2.0 支持 4k）: %v", err)
	} else if _, ok := m.SpecPrices.SecondPrice["4k"]; !ok {
		t.Fatalf("4K 应归一化为 4k: %+v", m.SpecPrices.SecondPrice)
	}
}

// TestBuildRejectsBillingModeModalityMismatch 是「永久 0 元」的防线。
func TestBuildRejectsBillingModeModalityMismatch(t *testing.T) {
	cases := []struct {
		modality string
		mode     string
		why      string
	}{
		{"audio", model.BillingModeTokens, "TTS/ASR 解析不出 token → 按量 = 永久免费"},
		{"image", model.BillingModeTokens, "图像只能按规格"},
		{"video", model.BillingModeCall, "视频只能按规格"},
		{"music", model.BillingModeTokens, "音乐只能按规格"},
		{"chat", model.BillingModeSpec, "对话没有「规格」概念"},
	}
	for _, c := range cases {
		in := Input{ID: "m", Vendor: "v", Modality: c.modality,
			BillingMode: c.mode, BillingModeSet: true,
			Prices: model.Prices{Input: i64p(1), Call: i64p(1)}}
		if _, err := Build(in); err == nil {
			t.Fatalf("%s × %s 应被拒绝（%s）", c.modality, c.mode, c.why)
		}
	}
	// 未知模态也必须拒（不能靠"调用方应该先校验"）。
	if _, err := Build(Input{ID: "m", Vendor: "v", Modality: "hologram",
		BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices: model.Prices{Input: i64p(1)}}); err == nil {
		t.Fatal("未知模态应被拒绝")
	}
	// 未知计费方式同样要拒：它会让计费侧走到 default 分支，行为未定义。
	if _, err := Build(Input{ID: "m", Vendor: "v", Modality: "chat",
		BillingMode: "pay-as-you-go", BillingModeSet: true,
		Prices: model.Prices{Input: i64p(1)}}); err == nil {
		t.Fatal("未知计费方式应被拒绝")
	}
	// 新建时完全没提交计费方式 → 拒（静默按 tokens 会让音频模型永久 0 元）。
	if _, err := Build(Input{ID: "m", Vendor: "v", Modality: "chat",
		Prices: model.Prices{Input: i64p(1)}}); err == nil {
		t.Fatal("新建必须显式指定计费方式")
	}
}

// TestFillMissingKeepsFactsWhenBillingModeChanges 守住本包最贵的一条历史缺陷。
//
// 计费方式变了，价格段理应不回填；但 endpoints / capabilities / enabled
// 是"事实"，任何路径都不许丢。丢了不会报错，只会让门户端点消失、
// 文档右栏 TAB 全没、停用模型被重新启用。
func TestFillMissingKeepsFactsWhenBillingModeChanges(t *testing.T) {
	inPrice := int64(1_000_000)
	cur := &model.Model{
		ID: "m", DisplayName: "m", Vendor: "v", Modality: "chat",
		BillingMode: model.BillingModeTokens, Enabled: false,
		Prices:       model.Prices{Input: &inPrice},
		Endpoints:    model.MustJSON([]string{"chat/completions"}),
		Capabilities: model.MustJSON([]string{"tool_call"}),
	}
	call := int64(2_000_000)
	in := Input{
		ID: "m", DisplayName: sp("m"), Vendor: "v", Modality: "chat",
		BillingMode: model.BillingModeCall, BillingModeSet: true, // 按量 → 按次：价格段会提前返回
		Prices: model.Prices{Call: &call},
	}
	FillMissing(&in, cur)
	if !in.EndpointsSet || len(in.Endpoints) != 1 || in.Endpoints[0] != "chat/completions" {
		t.Fatalf("端点被丢了：%+v", in.Endpoints)
	}
	if !in.CapabilitiesSet || len(in.Capabilities) != 1 || in.Capabilities[0] != "tool_call" {
		t.Fatalf("能力被丢了：%+v", in.Capabilities)
	}
	if in.Enabled == nil || *in.Enabled {
		t.Fatal("enabled 未沿用库内的 false：停用模型会被这次改价重新启用")
	}

	m, err := Build(in)
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if string(m.Endpoints) == "" || string(m.Endpoints) == "null" {
		t.Fatalf("落库端点为空：%s", m.Endpoints)
	}
	if string(m.Capabilities) == "" || string(m.Capabilities) == "null" {
		t.Fatalf("落库能力为空：%s", m.Capabilities)
	}
	if m.Enabled {
		t.Fatal("落库 enabled 变成了 true")
	}
}

// TestBuildClearsSpecWhenModeNotSpec 守住「切计费方式后旧价目必须消失」。
//
// `Updates` 写全列，旧实现只是"碰巧"正确：tokens/call 分支不碰 m.SpecPrices，
// 而 m 是 Build 新分配的零值行。任何人把 Build 改成复用 cur（很自然的优化）都会
// 让旧档位价复活 —— 表现是「按次」模型在门户上仍挂着 ¥/秒 的价目。
func TestBuildClearsSpecWhenModeNotSpec(t *testing.T) {
	call := int64(10_000)
	cases := []struct {
		name string
		mode string
		p    model.Prices
	}{
		{"按次", model.BillingModeCall, model.Prices{Call: &call}},
	}
	for _, c := range cases {
		m, err := Build(Input{ID: "m", DisplayName: sp("m"), Vendor: "v", Modality: "audio",
			BillingMode: c.mode, BillingModeSet: true, Prices: c.p,
			// 客户端/提案可能顺手带上旧价目：必须被无声丢弃，而不是跟着落库。
			SpecPrices:    model.SpecPrices{Kind: "per_second", PerSecondPrice: &call},
			SpecPricesSet: true})
		if err != nil {
			t.Fatalf("%s：Build 失败: %v", c.name, err)
		}
		if !m.SpecPrices.IsZero() {
			t.Fatalf("%s：非 spec 模式落库价目必须为空，实际 %+v", c.name, m.SpecPrices)
		}
	}
}

// TestFillMissingKeepsSpecPricesOnRename 守住「只改显示名」不失败。
func TestFillMissingKeepsSpecPricesOnRename(t *testing.T) {
	cur := &model.Model{
		ID: "vid", DisplayName: "旧名", Vendor: "v", Modality: "video",
		BillingMode: model.BillingModeSpec, Enabled: true,
		SpecPrices: model.SpecPrices{Kind: "per_video",
			SecondPrice: map[string]int64{"720p": 12000}, MaxSeconds: i64p(10)},
	}
	in := Input{ID: "vid", DisplayName: sp("新名"), Vendor: "v", Modality: "video",
		BillingMode: model.BillingModeSpec, BillingModeSet: true}
	FillMissing(&in, cur)
	if in.SpecPrices.IsZero() {
		t.Fatal("spec_prices 未沿用库内：只改显示名会被规格校验拒掉")
	}
	if _, err := Build(in); err != nil {
		t.Fatalf("只改显示名应通过: %v", err)
	}
}

func i64p(v int64) *int64 { return &v }

// TestClearPricesWinsOverFillMissing 守住「清空单独定价」这个动作真的能落库。
//
// lane 是 `*int64`，nil 同时表示"没提交"与"要清空"，而 FillMissing 会把
// 没提交的 lane 用库内值补齐 —— 于是"删掉某个通道的单独价、保存"曾经永远
// 保存不进去（删了又回来）。清空必须由 ClearPrices 名单承载，且必须在
// fillPrices **之后**生效。
func TestClearPricesWinsOverFillMissing(t *testing.T) {
	inPrice, outPrice, cachePrice := int64(1_000_000), int64(2_000_000), int64(500_000)
	cur := &model.Model{
		ID: "m", DisplayName: "m", Vendor: "v", Modality: "chat",
		BillingMode: model.BillingModeTokens, Enabled: true,
		Prices: model.Prices{Input: &inPrice, Output: &outPrice, Cache: &cachePrice},
	}
	in := Input{
		ID: "m", DisplayName: sp("m"), Vendor: "v", Modality: "chat",
		BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices:      model.Prices{Input: &inPrice},
		ClearPrices: []string{"output", "cache"},
	}
	FillMissing(&in, cur)
	m, err := Build(in)
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if m.Prices.Output != nil || m.Prices.Cache != nil {
		t.Fatalf("清空未生效：output=%v cache=%v（删掉的单独定价又回来了）", m.Prices.Output, m.Prices.Cache)
	}
	if m.Prices.Input == nil || *m.Prices.Input != inPrice {
		t.Fatal("清空名单不该影响未列入的 lane")
	}
}

// TestBuildRejectsUnknownClearLane 守住拼错的通道名。
//
// 静默忽略等于"点了清除但价格还在"——运营会以为已经改好了，实际仍在按旧价收费。
func TestBuildRejectsUnknownClearLane(t *testing.T) {
	_, err := buildChat(t, func(in *Input) { in.ClearPrices = []string{"input_price"} })
	if err == nil {
		t.Fatal("未知的 clear_prices 通道名必须报错")
	}
	if !strings.Contains(err.Error(), "clear_prices") && !strings.Contains(err.Error(), "未知价格通道") {
		t.Fatalf("错误信息没说清是哪个字段: %v", err)
	}
}

// sp 是"取字符串地址"的测试辅助：Input 的展示名/描述用指针表达
// "未提交（nil）"，字面量不能直接取址。
func sp(s string) *string { return &s }

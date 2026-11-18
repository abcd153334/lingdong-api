package apidoc

import (
	"encoding/json"
	"testing"

	"lingdong-api/internal/service/billing"
)

// 能力词表是**封闭**的：新增能力必须三处同时改（capability.go 的常量、
// capabilityDefs、预设的 Requires）。这条测试守的是「常量加了但表里没加」——
// 那种情况下 ValidateCapabilities 会判它非法，而 DefaultCapabilities 又可能吐出它。
func TestCapabilityVocabularyClosed(t *testing.T) {
	defs := CapabilityDefs()
	// 9 条基础 + 7 条厂商原生命名引入的（image_stream / image_layer / image_group /
	// image_web_search / image_native / video_audio / video_native）。数字变化是**有意的门禁**：
	// 改它就必须同步确认「预设里有对应 TAB」（preset_test.go 的覆盖断言会一起红）。
	//
	// image_group 是 2026-11-16 新增的：组图能力此前只能靠 image_native 推断，
	// 而方舟 5.0 pro / flash 恰好是「收原生报文 + 有图层 + **不支持组图**」，
	// 用 image_native 当组图判据会让它们的组图请求按 15 张冻结。
	if len(defs) != 16 {
		t.Fatalf("能力词表应为 16 项（改词表必须同步改文档与预设），实际 %d", len(defs))
	}
	seen := make(map[Cap]bool, len(defs))
	for _, d := range defs {
		if d.Key == "" || d.Label == "" {
			t.Fatalf("能力定义缺键或标签: %+v", d)
		}
		if seen[d.Key] {
			t.Fatalf("能力重复定义: %s", d.Key)
		}
		seen[d.Key] = true
		if len(d.Modalities) == 0 {
			t.Fatalf("能力 %s 没声明适用模态 → 任何模态下都会被判非法", d.Key)
		}
		for _, m := range d.Modalities {
			if NormalizeModality(m) != m {
				t.Fatalf("能力 %s 的模态 %q 不是规范写法", d.Key, m)
			}
		}
		// Endpoint 非空时必须能在契约里找到，否则「声明了能力就永远用不到」。
		if d.Endpoint != "" {
			found := false
			for _, k := range Kinds() {
				if tr, ok := TransportOf(k); ok && tr.EndpointKey == d.Endpoint {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("能力 %s 声明的端点 %q 在契约里不存在", d.Key, d.Endpoint)
			}
		}
	}
}

func TestDefaultCapabilities(t *testing.T) {
	cases := []struct {
		modality string
		want     []Cap
	}{
		{"image", []Cap{CapT2I}},
		{"video", []Cap{CapT2V}},
		{"music", []Cap{CapMusic}},
		// chat / embedding 这类没有「预设 TAB」的概念，兜底为空。
		{"chat", nil},
		{"embedding", nil},
		// 空模态按 chat 归一（与模态闸门一致）。
		{"", nil},
	}
	for _, c := range cases {
		got := DefaultCapabilities(c.modality)
		if len(got) != len(c.want) {
			t.Fatalf("DefaultCapabilities(%q) = %v, want %v", c.modality, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("DefaultCapabilities(%q) = %v, want %v", c.modality, got, c.want)
			}
		}
	}
}

// 兜底绝不能带参考素材能力：猜错「支持参考音频」会让用户传参然后 400。
func TestDefaultCapabilitiesNeverGuessesRefAssets(t *testing.T) {
	for _, mod := range []string{"image", "video", "music", "chat", "audio"} {
		for _, c := range DefaultCapabilities(mod) {
			switch c {
			case CapRefAudio, CapRefVideo, CapFirstLast, CapI2I, CapI2V:
				t.Fatalf("模态 %s 的兜底能力里出现了 %s（参考素材类能力只能显式声明）", mod, c)
			}
		}
	}
}

func TestNormalizeCapabilities(t *testing.T) {
	cases := []struct {
		name     string
		modality string
		raw      string
		want     []Cap
	}{
		{"NULL 走兜底", "video", "", []Cap{CapT2V}},
		{"非法 JSON 走兜底", "video", `{bad`, []Cap{CapT2V}},
		// `[]` = 明确声明没有额外能力，必须尊重，绝不能被兜底打开。
		{"空数组不回填", "video", `[]`, nil},
		{"未知词丢弃", "video", `["t2v","no_such_cap"]`, []Cap{CapT2V}},
		{"模态不匹配丢弃", "video", `["t2v","i2i"]`, []Cap{CapT2V}},
		{"大小写与空格归一", "image", `[" T2I ","I2I"]`, []Cap{CapT2I, CapI2I}},
		{"去重", "video", `["i2v","i2v"]`, []Cap{CapI2V}},
		// 顺序按词表排（t2v 在 i2v 之前），保证签名稳定。
		{"按词表排序", "video", `["ref_video","i2v","t2v"]`, []Cap{CapT2V, CapI2V, CapRefVideo}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeCapabilities(c.modality, rawOf(c.raw))
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestValidateCapabilities(t *testing.T) {
	bad := []struct {
		name     string
		modality string
		raw      string
	}{
		{"未定义能力", "video", `["nope"]`},
		{"模态不匹配", "video", `["t2i"]`},
		{"重复声明", "video", `["t2v","t2v"]`},
		{"不是数组", "video", `"t2v"`},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateCapabilities(c.modality, rawOf(c.raw)); err == nil {
				t.Fatalf("期望校验失败，实际通过（raw=%s）", c.raw)
			}
		})
	}
	if err := ValidateCapabilities("video", nil); err != nil {
		t.Fatalf("NULL 是合法状态（未声明），不该报错: %v", err)
	}
	if err := ValidateCapabilities("video", rawOf(`["t2v","i2v","ref_audio"]`)); err != nil {
		t.Fatalf("合法能力被判非法: %v", err)
	}
}

// 能力要求的端点必须已声明，否则能力永远用不到（静默失效）。
func TestValidateCapabilitiesAgainstEndpoints(t *testing.T) {
	if err := ValidateCapabilitiesAgainstEndpoints(
		[]Cap{CapT2V},
		[]string{"video/generations"},
	); err != nil {
		t.Fatalf("端点齐备却被判非法: %v", err)
	}
	if err := ValidateCapabilitiesAgainstEndpoints(
		[]Cap{CapMusic},
		[]string{"video/generations"},
	); err == nil {
		t.Fatal("music 能力配 video 端点应被判非法")
	}
	// 空端点**不判非法**：语义是「用该模态的默认端点」，而默认端点里已经包含
	// 能力所需的端点。判非法会让「不填端点的模型」保存不了任何能力。
	if err := ValidateCapabilitiesAgainstEndpoints([]Cap{CapT2V}, nil); err != nil {
		t.Fatalf("空端点应跳过端点校验，实际报错: %v", err)
	}
}

// SuggestCapabilities 是「推荐」而不是「读时推导」，这条测试固定推荐口径。
func TestSuggestCapabilities(t *testing.T) {
	cases := []struct {
		name      string
		modality  string
		endpoints []string
		spec      billing.SpecFacts
		want      []Cap
	}{
		{
			name: "图像只声明 generations → 只有文生图", modality: "image",
			endpoints: []string{"images/generations"},
			want:      []Cap{CapT2I},
		},
		{
			name: "图像声明 edits → 加图生图", modality: "image",
			endpoints: []string{"images/generations", "images/edits"},
			want:      []Cap{CapT2I, CapI2I},
		},
		{
			// 关键边界：**端点为空时不能按模态默认端点推导**，否则每个图像模型
			// 都会被推荐成支持图生图（默认端点含 images/edits）。
			name: "图像端点为空 → 退回最小兜底", modality: "image",
			endpoints: nil,
			want:      []Cap{CapT2I},
		},
		{
			name: "视频无参考素材价 → 只有文生视频", modality: "video",
			endpoints: []string{"video/generations"},
			want:      []Cap{CapT2V},
		},
		{
			name: "视频配了参考图价 → 图生视频", modality: "video",
			endpoints: []string{"video/generations"},
			spec:      billing.SpecFacts{HasRefImagePrice: true},
			want:      []Cap{CapT2V, CapI2V},
		},
		{
			name: "视频配了免费参考图张数 → 图生视频", modality: "video",
			endpoints: []string{"video/generations"},
			spec:      billing.SpecFacts{FreeRefImagesSet: true, FreeRefImages: 5},
			want:      []Cap{CapT2V, CapI2V},
		},
		{
			name: "视频配了参考音频价 → 多模态", modality: "video",
			endpoints: []string{"video/generations"},
			spec:      billing.SpecFacts{HasRefImagePrice: true, HasRefAudioPrice: true},
			want:      []Cap{CapT2V, CapI2V, CapRefAudio},
		},
		{
			name: "参考视频按秒 → ref_video", modality: "video",
			endpoints: []string{"video/generations"},
			spec:      billing.SpecFacts{RefVideoSecondTiers: []string{"720p"}},
			want:      []Cap{CapT2V, CapRefVideo},
		},
		{
			name: "音乐端点", modality: "music",
			endpoints: []string{"audio/music/generations"},
			want:      []Cap{CapMusic},
		},
		{
			name: "歌词端点", modality: "music",
			endpoints: []string{"audio/lyrics"},
			want:      []Cap{CapLyrics},
		},
		{
			// chat 没有预设 TAB，推荐为空（空集合仍要走 DefaultCapabilities 的 nil）。
			name: "chat 无能力", modality: "chat",
			endpoints: []string{"chat/completions"},
			want:      nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SuggestCapabilities(c.modality, c.endpoints, c.spec)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

// first_last 推导不出来，只能人工声明；推荐值里永远不出现它。
func TestSuggestNeverGuessesFirstLast(t *testing.T) {
	spec := billing.SpecFacts{HasRefImagePrice: true, HasRefAudioPrice: true}
	for _, e := range [][]string{nil, {"video/generations"}, {"video/regenerations"}} {
		for _, c := range SuggestCapabilities("video", e, spec) {
			if c == CapFirstLast {
				t.Fatalf("推荐值里出现了 first_last（端点 %v）", e)
			}
		}
	}
}

func TestCapLabels(t *testing.T) {
	got := CapLabels([]Cap{CapT2V, CapI2V, CapRefAudio})
	want := []string{"文生视频", "图生视频", "参考音频"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// 未知能力静默跳过（不 panic、不输出空串）。
	if out := CapLabels([]Cap{"nope"}); len(out) != 0 {
		t.Fatalf("未知能力应被跳过，实际 %v", out)
	}
}

func rawOf(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

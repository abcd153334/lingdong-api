package adapters

import (
	"strings"
	"testing"
)

// 族前缀推断是「厂商升级型号不必发版」的唯一机制，所以它的正确性必须由用例锁死：
// 命中哪一族决定用哪个信封/端点，错一族就是整条请求打到错误的上游路径。
func TestDuomiFamilyInference(t *testing.T) {
	cases := []struct {
		id          string
		family      string
		protocol    string
		mode        string
		durationStr bool
		unsupported bool
	}{
		// 厂商同族升级：这三类是本次故障与后续升级的常态
		{"gpt-image-2.6-sunburst", "gpt-image", duomiProtoGPTImage, "", false, false},
		{"gpt-image-9", "gpt-image", duomiProtoGPTImage, "", false, false},
		{"kling-v4-pro", "kling.pro", duomiProtoKling, "pro", false, false},
		{"kling-v4-std", "kling.std", duomiProtoKling, "std", false, false},
		{"kling-v4-omni", "kling.omni", duomiProtoKlingOmni, "std", true, false},
		{"kling-v2.5-multi", "kling.multi", duomiProtoKlingMulti, "", true, false},
		{"kling-v5", "kling", duomiProtoKling, "", false, false},
		{"nano-banana-3", "nano", duomiProtoNano, "", false, false},
		{"seedance-3.0", "seedance", duomiProtoSeedance, "", false, false},
		{"doubao-seedance-3-0-260101", "seedance", duomiProtoSeedance, "", false, false},
		// muse 是多米自有视频协议：同族升级（muse-video-2）应零配置可用
		{"muse-video", "muse", duomiProtoMuse, "", false, false},
		{"muse-video-2", "muse", duomiProtoMuse, "", false, false},
		// Gemini 图像在文档里有两种叫法，第二种不含 image 也不算 nano 前缀
		{"gemini-3.2-flash-image-preview", "gemini-image", duomiProtoNano, "", false, false},
		{"gemini-nano-banana-2.1", "gemini-image", duomiProtoNano, "", false, false},
		{"chirp-v5", "suno.music", duomiProtoSunoMusic, "", false, false},
		{"suno-v5", "suno.music", duomiProtoSunoMusic, "", false, false},
		{"suno-lyrics", "suno.lyrics", duomiProtoSunoLyrics, "", false, false},
		{"suno-lyrics-v2", "suno.lyrics", duomiProtoSunoLyrics, "", false, false},

		// C 类：协议族存在但本适配器未实现，必须与「模型没登记」区分开
		{"veo-3", "veo", "", "", false, true},
		{"sora-2", "sora", "", "", false, true},
		{"minimax-hailuo-2", "hailuo", "", "", false, true},
		{"hailuo-02", "hailuo", "", "", false, true},
	}
	for _, c := range cases {
		got, ok := duomiMatchFamily(c.id)
		if !ok {
			t.Errorf("%s：未命中任何族", c.id)
			continue
		}
		if got.Name != c.family {
			t.Errorf("%s：命中族 %q，期望 %q（顺序即优先级，特化必须排在兜底前）", c.id, got.Name, c.family)
			continue
		}
		if got.Protocol != c.protocol || got.Mode != c.mode ||
			got.DurationAsString != c.durationStr || (got.Unsupported != "") != c.unsupported {
			t.Errorf("%s：档案不符 got=%+v want protocol=%q mode=%q durStr=%v unsupported=%v",
				c.id, got, c.protocol, c.mode, c.durationStr, c.unsupported)
		}
	}
}

// 完全看不懂的命名必须落空（让调用方报 B 类），绝不能猜一个协议出来。
func TestDuomiFamilyNoMatch(t *testing.T) {
	for _, id := range []string{"", "  ", "zzz-9", "图像旗舰版", "gpt-4o", "whisper-1"} {
		if fam, ok := duomiMatchFamily(id); ok {
			t.Errorf("%q 不该命中族，实际 %q", id, fam.Name)
		}
	}
}

// 精确表与族表是两份东西，但必须对同一 ID 给出同一个协议。
//
// 这条是防「双真相源」的唯一机制：以后谁往 duomiModels 里加一条与族前缀矛盾的档案
// （例如把 kling-v9-multi 写成 kling 协议），这里立刻红，逼他想清楚谁是真相。
func TestDuomiFamilyConsistentWithBuiltin(t *testing.T) {
	for id, p := range duomiModels {
		fam, ok := duomiMatchFamily(id)
		if !ok {
			t.Errorf("内置模型 %s 未命中任何族：新增内置型号时请同时确认族规则，否则文档与预检会与运行期不一致", id)
			continue
		}
		if fam.Protocol != p.Protocol {
			t.Errorf("内置模型 %s：精确表协议 %q，族 %q 推断 %q（两者必须一致）",
				id, p.Protocol, fam.Name, fam.Protocol)
		}
	}
}

// 未知模型的文案必须能直接操作：带平台模型 ID 的 JSON 片段 + 可用协议枚举。
func TestDuomiUnknownModelMsgIsActionable(t *testing.T) {
	msg := duomiUnknownModelMsg("gpt-image-2.6-sunburst")
	if !strings.HasPrefix(msg, `未知的多米模型 "gpt-image-2.6-sunburst"`) {
		t.Fatalf("首句必须保留「未知的多米模型」锚点：%s", msg)
	}
	if !strings.Contains(msg, `"gpt-image-2.6-sunburst":{"protocol"`) {
		t.Fatalf("缺少可直接粘贴的 override 片段：%s", msg)
	}
	if !strings.Contains(msg, duomiProtocolList) {
		t.Fatalf("缺少可用协议枚举：%s", msg)
	}
	// 安全：文案不得出现上游模型名占位之外的信息（这里只断言不含渠道措辞）
	if strings.Contains(msg, "api_key") || strings.Contains(msg, "Bearer") {
		t.Fatalf("文案不得含凭据类字样：%s", msg)
	}
}

func TestDuomiUnsupportedFamilyMsg(t *testing.T) {
	msg := duomiUnsupportedFamilyMsg("veo-3", "veo")
	if !strings.Contains(msg, "尚未实现该协议") || !strings.Contains(msg, "model_overrides") {
		t.Fatalf("C 类文案必须说明 override 救不了：%s", msg)
	}
	if !strings.Contains(msg, "docs/渠道接入手册-新增模型与协议.md") {
		t.Fatalf("C 类文案必须指向接入手册：%s", msg)
	}
}

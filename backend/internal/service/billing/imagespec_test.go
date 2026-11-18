package billing

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

func tierPriceOf(pairs map[string]int64, defTier string) model.SpecPrices {
	return model.SpecPrices{Kind: SpecKindPerImage, TierPrice: pairs, ResolutionFixed: defTier}
}

func proPrice() model.SpecPrices {
	return tierPriceOf(map[string]int64{"1K": 300000, "1.5K": 300000, "2K": 450000}, "2K")
}

func litePrice() model.SpecPrices {
	return tierPriceOf(map[string]int64{"2K": 300000, "3K": 450000, "4K": 600000}, "2K")
}

func gptImagePrice() model.SpecPrices {
	return tierPriceOf(map[string]int64{"1K": 600000}, "1K")
}

// TestParseImageSizeNeverRejects 是这次改造的核心不变量：
// **任何写法都必须解析出一个结果，永不报错**（支持什么由模型与官方文档决定）。
func TestParseImageSizeNeverRejects(t *testing.T) {
	cases := []struct {
		size, ratio  string
		wantTier     string
		wantW, wantH int
		wantRatio    string
		wantAuto     bool
		wantUnknown  string
		wantExplicit bool
	}{
		{"2K", "", "2K", 0, 0, "", false, "", true},
		{"1.5k", "", "1.5K", 0, 0, "", false, "", true},
		{"8K", "", "8K", 0, 0, "", false, "", true},
		{"2048x1152", "", "", 2048, 1152, "16:9", false, "", true},
		{"auto", "", "", 0, 0, "", true, "", false}, // auto 不算"显式"：上游挑档，不参与比对
		{"portrait_4_3", "", "", 0, 0, "", false, "portrait_4_3", true},
		{"", "", "", 0, 0, "", false, "", false},
		{"", "16:9", "", 0, 0, "16:9", false, "", true},
		// 画幅与像素矛盾：保留像素、画幅取像素推导、打标记但不报错。
		{"1024x1024", "16:9", "", 1024, 1024, "1:1", false, "", true},
		// size 里写比例：按画幅理解（以前直接 400）。
		{"16:9", "", "", 0, 0, "16:9", false, "16:9", true},
		// 超长：截断后留痕，不报错。
		{strings.Repeat("x", 80), "", "", 0, 0, "", false, strings.Repeat("x", 64), true},
	}
	for _, c := range cases {
		got := ParseImageSize(ImageDecl{Size: c.size, AspectRatio: c.ratio})
		if got.DeclaredTier != c.wantTier || got.Width != c.wantW || got.Height != c.wantH ||
			got.Ratio != c.wantRatio || got.Auto != c.wantAuto || got.Unknown != c.wantUnknown ||
			got.Explicit != c.wantExplicit {
			t.Fatalf("ParseImageSize(%q,%q) = %+v，期望 tier=%q %dx%d ratio=%q auto=%v unknown=%q explicit=%v",
				c.size, c.ratio, got, c.wantTier, c.wantW, c.wantH, c.wantRatio, c.wantAuto, c.wantUnknown, c.wantExplicit)
		}
	}
}

// TestResolveImageTierNeverErrors：档位解释失败只出标记，不影响请求。
func TestResolveImageTierNeverErrors(t *testing.T) {
	cases := []struct {
		name      string
		sp        model.SpecPrices
		size      ImageSize
		wantTier  string
		wantFlags []string
	}{
		{"档位在价目内", proPrice(), ImageSize{DeclaredTier: "2K"}, "2K", nil},
		{"档位不在价目（8K）按最高价收", proPrice(), ImageSize{DeclaredTier: "8K"}, "2K", []string{model.AnomalyImageTierUnpriced}},
		{"像素吸附到 2K", proPrice(), ImageSize{Width: 2048, Height: 2048}, "2K", nil},
		{"像素超窗按最高价收", proPrice(), ImageSize{Width: 8000, Height: 8000}, "2K", []string{model.AnomalyImagePixelsOutOfWindow}},
		{"档位与像素矛盾取高价", proPrice(), ImageSize{DeclaredTier: "1K", Width: 2048, Height: 2048}, "2K", []string{model.AnomalyImageTierConflict}},
		// size 里写画幅：这是**认得出的意图**，按默认档计费，不得打「未识别」。
		// 以前 ParseImageSize 把比例同时塞进 Unknown，ResolveImageTier 于是当成乱写，
		// 账单异常面板里出现成片假告警（2026-11-04 实测 3 笔）。
		{"size 写画幅按默认档且不算未识别", proPrice(), ParseImageSize(ImageDecl{Size: "16:9"}), "2K", nil},
		{"全认不出按默认档", proPrice(), ImageSize{Unknown: "weird"}, "2K", []string{model.AnomalyImageSpecUnrecognized}},
		{"auto 按默认档", litePrice(), ImageSize{Auto: true, Source: ImageSizeSourceAuto}, "2K", nil},
	}
	for _, c := range cases {
		s := c.size
		flags := ResolveImageTier(c.sp, &s)
		if s.Tier != c.wantTier {
			t.Fatalf("%s：Tier=%q，期望 %q", c.name, s.Tier, c.wantTier)
		}
		for _, f := range c.wantFlags {
			if !strings.Contains(flags, f) {
				t.Fatalf("%s：标记缺 %q，实际 %q", c.name, f, flags)
			}
		}
	}
}

// TestImageTierMapKeepsArbitraryKeys：价目里的任意档位键都必须被认（不再按白名单过滤）。
func TestImageTierMapKeepsArbitraryKeys(t *testing.T) {
	sp := tierPriceOf(map[string]int64{"2.5K": 100000, "8k": 900000}, "2.5K")
	got := ImageTierMap(sp)
	if _, ok := got["2.5K"]; !ok {
		t.Fatalf("2.5K 应保留，实际 %v", got)
	}
	if _, ok := got["8K"]; !ok {
		t.Fatalf("8k 应归一为 8K 并保留，实际 %v", got)
	}
}

// TestTierSidePixelsRule：档位→边长是一条规则（N×1024），不再是一张常量表。
func TestTierSidePixelsRule(t *testing.T) {
	for tier, want := range map[string]int{"1K": 1024, "1.5K": 1536, "2K": 2048, "3K": 3072, "4K": 4096, "8K": 8192, "2.5K": 2560} {
		got, ok := TierSidePixels(tier)
		if !ok || got != want {
			t.Fatalf("TierSidePixels(%q)=(%d,%v)，期望 %d", tier, got, ok, want)
		}
	}
	if _, ok := TierSidePixels("ultra"); ok {
		t.Fatal("无数字前缀的键不应推导出边长")
	}
	if p, ok := PixelsOfTier("8K"); !ok || p != 8192*8192 {
		t.Fatalf("PixelsOfTier(8K)=(%d,%v)", p, ok)
	}
}

// TestDefaultTierOf 守「不传 size 时按哪一档计费」的推导。
//
// **只认配置**：显式配置的用配置值；只有一档时用那一档；其余返回空（由计费侧报
// ErrImageDefaultTierUnset）。曾经这里会「没配就取最低价档」，那是第二套逻辑。
func TestDefaultTierOf(t *testing.T) {
	if tier := defaultTierOf(proPrice()); tier != "2K" {
		t.Fatalf("pro 默认档=%q，期望 2K（显式配置）", tier)
	}
	// 未配置默认档：不得自行挑一档。
	noDefault := proPrice()
	noDefault.ResolutionFixed = ""
	if tier := defaultTierOf(noDefault); tier != "" {
		t.Fatalf("未配置默认档应返回空，实际 %q", tier)
	}
	// 配了价目里没有的键：同样返回空，不退回任何档。
	badDefault := litePrice()
	badDefault.ResolutionFixed = "8K"
	if tier := defaultTierOf(badDefault); tier != "" {
		t.Fatalf("失效默认档应返回空，实际 %q", tier)
	}
	// 只有一档：没有第二个选择，直接用它。
	single := tierPriceOf(map[string]int64{"1K": 300000}, "")
	if tier := defaultTierOf(single); tier != "1K" {
		t.Fatalf("单档模型应返回该档，实际 %q", tier)
	}
}

func TestNormalizeRatio(t *testing.T) {
	if got := NormalizeRatio(2048, 1152); got != "16:9" {
		t.Fatalf("NormalizeRatio(2048,1152)=%q", got)
	}
	if got := NormalizeRatio(1024, 1024); got != "1:1" {
		t.Fatalf("NormalizeRatio(1024,1024)=%q", got)
	}
}

// TestParseImageSizeFlagsRatioConflict 守「同一语义写了两遍、且互相矛盾」必须留痕。
//
// 场景：`size=16:9` + `aspect_ratio=4:3`。两者都是画幅语义，实现上保留
// aspect_ratio（专用字段），但**必须**打 image_ratio_conflict：
// 否则出站按 4:3 出图、账单按 size 的 16:9 记，偏差在账面上完全看不出来。
func TestParseImageSizeFlagsRatioConflict(t *testing.T) {
	cases := []struct {
		name         string
		decl         ImageDecl
		wantRatio    string
		wantConflict bool
	}{
		{"两个画幅矛盾", ImageDecl{Size: "16:9", AspectRatio: "4:3"}, "4:3", true},
		{"两个画幅一致", ImageDecl{Size: "16:9", AspectRatio: "16:9"}, "16:9", false},
		{"只写画幅", ImageDecl{Size: "16:9"}, "16:9", false},
		{"只写 aspect_ratio", ImageDecl{AspectRatio: "4:3"}, "4:3", false},
		{"像素与画幅矛盾（既有规则）", ImageDecl{Size: "1024x1024", AspectRatio: "16:9"}, "1:1", true},
		{"档位与画幅不冲突", ImageDecl{Size: "2K", AspectRatio: "4:3"}, "4:3", false},
	}
	for _, c := range cases {
		got := ParseImageSize(c.decl)
		if got.Ratio != c.wantRatio {
			t.Fatalf("%s：Ratio=%q，期望 %q", c.name, got.Ratio, c.wantRatio)
		}
		has := strings.Contains(got.Anomaly, model.AnomalyImageRatioConflict)
		if has != c.wantConflict {
			t.Fatalf("%s：conflict=%v，期望 %v（anomaly=%q）", c.name, has, c.wantConflict, got.Anomaly)
		}
	}

	// size 写画幅必须被认定为「画幅」语义，而不是「认不出的写法」：
	// 适配器靠 Source 决定「不要塞进 image_size」。
	if s := ParseImageSize(ImageDecl{Size: "16:9"}); s.Source != ImageSizeSourceRatio {
		t.Fatalf("size=16:9 的 Source 应为 ratio，实际 %q", s.Source)
	}
	// 矛盾时 Ratio 被覆盖也必须保持 ratio 语义，否则出站侧又走回「塞 image_size」。
	if s := ParseImageSize(ImageDecl{Size: "16:9", AspectRatio: "4:3"}); s.Source != ImageSizeSourceRatio {
		t.Fatalf("矛盾时 Source 仍应为 ratio，实际 %q", s.Source)
	}
}

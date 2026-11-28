package db

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 折算规则是整个 M28 迁移里唯一会改变金额的地方，必须锁死。
//
// 这几个数字不是"随便定的系数"：ratio=1.25 对应 GPT-4o 官方 $2.5/1M，
// 汇率 7.2 下正好 ¥18/1M。
// 一旦有人把 ×2 写成 ×1 或漏掉汇率，这里立刻失败。
func TestLegacyRatioToMicro(t *testing.T) {
	if got := microOfRatio(ptr(1.25), 7.2); got == nil || *got != 18_000_000 {
		t.Fatalf("ratio 1.25 + fx 7.2 应为 ¥18/1M = 18000000 微，got %v", got)
	}
	// 旧额度锚：1 美元 = 500000 额度 → 6000 额度 = ¥0.0864 = 86400 微
	if got := quotaToMicro(6000, 7.2); got != 86_400 {
		t.Fatalf("6000 额度应为 86400 微，got %d", got)
	}
	if microOfRatio(nil, 7.2) != nil {
		t.Fatal("ratio 缺失必须保持 nil（未配置），不能补 0")
	}
}

// 迁移后的计费方式必须按**模态**纠正，不能照搬旧的 0 值。
//
// 事故复盘：图片模型曾整体落在 `tokens` 上，图片没有 token 通道，
// 于是"按张计费"变成"按 0 个 token 计费"，档位价目完全不参与。
func TestLegacyBillingModeByModality(t *testing.T) {
	cases := []struct {
		quotaType int
		specKind  string
		modality  string
		want      string
	}{
		{0, "per_image", "image", "spec"},
		{0, "", "image", "spec"},
		{1, "", "image", "call"},
		{0, "per_video", "video", "spec"},
		{0, "per_task", "music", "spec"},
		{0, "", "chat", "tokens"},
		{1, "", "audio", "call"},
		{2, "", "chat", "spec"},
	}
	for _, c := range cases {
		if got := legacyBillingMode(c.quotaType, c.specKind, c.modality); got != c.want {
			t.Errorf("quota_type=%d kind=%q modality=%s: want %s, got %s",
				c.quotaType, c.specKind, c.modality, c.want, got)
		}
	}
}

// 旧 key → 新 key，值折算成微；倍率 2 要变成 2000000。
func TestMigrateLegacySpecVideo(t *testing.T) {
	raw := `{"kind":"per_video","max_seconds":30,"free_ref_images":5,` +
		`"second_price_quota":{"480p":8333,"720p":12500,"1080P":19444},` +
		`"long_video_multiplier":2,"long_after_seconds":15,` +
		`"ref_image_price_quota":6944,"resolution_fixed":"720p"}`
	rawPtr := raw
	sp, keys, warns, err := migrateLegacySpec(&rawPtr, 7.2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("不该有告警: %v", warns)
	}
	if sp.Kind != "per_video" {
		t.Fatalf("kind 应沿用: %q", sp.Kind)
	}
	// 8333 × 14.4 = 119995.2 → 119995
	if got := sp.SecondPrice["480p"]; got != 119_995 {
		t.Fatalf("480p 应为 119995 微，got %d", got)
	}
	// 档位键必须归一化小写：旧的 1080P 不归一就等于"这一档不存在"
	if _, ok := sp.SecondPrice["1080p"]; !ok {
		t.Fatalf("1080P 未归一化为 1080p: %+v", sp.SecondPrice)
	}
	if sp.LongVideoMultiplier == nil || *sp.LongVideoMultiplier != 2_000_000 {
		t.Fatalf("倍率 2 应转成微比例 2000000，got %v", sp.LongVideoMultiplier)
	}
	if sp.RefImageFree == nil || *sp.RefImageFree != 5 {
		t.Fatalf("free_ref_images 5 应搬成 ref_image_free 5，got %v", sp.RefImageFree)
	}
	if sp.ResolutionFixed != "720p" {
		t.Fatalf("固定档位应沿用，got %q", sp.ResolutionFixed)
	}
	if len(keys) == 0 || !contains(keys, "second_price") || !contains(keys, "ref_image_free") {
		t.Fatalf("keys 应包含迁移过的键: %v", keys)
	}
}

// 把「元」写成小数的坏数据必须跳过并点名，绝不能折成 1 微（≈ 免费且看起来已配价）。
func TestMigrateLegacySpecSkipsFractional(t *testing.T) {
	raw := `{"kind":"per_image","default_price_quota":0.08,"resolution_prices_quota":{"1K":0.08,"2K":0.09}}`
	rawPtr := raw
	sp, _, warns, err := migrateLegacySpec(&rawPtr, 7.2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// 档位价与兜底价都是小数 → 全部跳过；默认档也推不出来（没有可用的档位价）。
	if sp.ResolutionFixed != "" || len(sp.TierPrice) != 0 {
		t.Fatalf("小数原值必须跳过，got resolution_fixed=%q tier=%v", sp.ResolutionFixed, sp.TierPrice)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, ";"), "小数") {
		t.Fatalf("必须点名小数坏数据，got %v", warns)
	}
}

// 已经是新结构的行不再产生旧键 → 调用方据此跳过，保证迁移幂等。
func TestMigrateLegacySpecIsIdempotent(t *testing.T) {
	raw := `{"kind":"per_image","max_n":1,"tier_price":{"1K":200002},"default_price":400003}`
	rawPtr := raw
	_, keys, _, err := migrateLegacySpec(&rawPtr, 7.2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("新结构不应被识别为旧键: %v", keys)
	}
}

// 折算后明显不合理的价格要点名（联调测试值 50 秒价 → ¥0.0007/秒）。
func TestSanityWarningsLowPrice(t *testing.T) {
	sp := specOf(map[string]int64{"720p": 720})
	warns := sanityWarnings("spec", nil, sp)
	if len(warns) == 0 {
		t.Fatal("720 微/秒 明显过低，应告警")
	}
	ok := specOf(map[string]int64{"720p": 180_000})
	if warns := sanityWarnings("spec", nil, ok); len(warns) != 0 {
		t.Fatalf("正常价不该告警: %v", warns)
	}
}

func ptr[T any](v T) *T { return &v }

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// specOf 构造只含秒价表的价目（per_video）。
func specOf(second map[string]int64) model.SpecPrices {
	return model.SpecPrices{Kind: "per_video", SecondPrice: second}
}

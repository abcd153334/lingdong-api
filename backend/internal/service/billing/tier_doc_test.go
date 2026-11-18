package billing

import "testing"

// TestImageTierPixels 守「档位面积只有一条推导规则」。
//
// 面积由 `TierSidePixels`（N × 1024）算出来，不再有常量表：运营可以配
// `8K` / `2.5K` 这类词表外的档位，表一旦写死，新档位就没有面积，
// 像素写法与面积吸附对它全部失效（表现为「配了 8K、请求 8K 却按默认档收费」）。
func TestImageTierPixels(t *testing.T) {
	want := map[string]int64{
		"1K":   1024 * 1024,
		"1.5K": 1536 * 1536,
		"2K":   2048 * 2048,
		"3K":   3072 * 3072,
		"4K":   4096 * 4096,
		// 词表外，但必须同样能推出面积。
		"8K": 8192 * 8192,
	}
	for tier, area := range want {
		got, ok := PixelsOfTier(tier)
		if !ok || got != area {
			t.Fatalf("档位 %s 的方形面积应为 %d，实际 %d（ok=%v）", tier, area, got, ok)
		}
	}
	// 大小写与空白应被容忍。
	if v, ok := PixelsOfTier(" 2k "); !ok || v != 2048*2048 {
		t.Fatalf("大小写/空白未归一到 2K：v=%d ok=%v", v, ok)
	}
	// 无数字前缀的私有档位不编造面积（只作价格键）。
	if _, ok := PixelsOfTier("ultra"); ok {
		t.Fatal("无数字前缀的键不应推导面积")
	}
}

// TestRatioVocabularyCoversStandardRatios 守归一吸附的词汇表不退化。
//
// 词汇表太小时，用户按某个比例写的像素会被吸到「最接近的另一个画幅」，
// 于是请求被悄悄换算成别的画幅 —— 文档与实际各说各话，谁都不报错。
func TestRatioVocabularyCoversStandardRatios(t *testing.T) {
	known := make(map[string]bool, len(imageStandardRatios))
	for _, r := range imageStandardRatios {
		if known[r] {
			t.Fatalf("吸附词汇表里有重复项 %q", r)
		}
		known[r] = true
	}
	if len(known) < 19 {
		t.Fatalf("吸附词汇表只剩 %d 项：7:4 / 1:3 这类画幅会落成机器才看得懂的值", len(known))
	}
}

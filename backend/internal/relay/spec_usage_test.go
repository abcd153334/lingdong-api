package relay

import (
	"encoding/json"
	"strconv"
	"testing"
	"unicode/utf8"

	"lingdong-api/internal/service/billing"
)

// TestSpecUsageAudioCharsCountsRunes 锁死「音频字数按字符数、不按字节数」。
//
// 这是按秒计费里最容易"看起来对"的一处单位错：中文 UTF-8 占 3 字节，
// 用 len() 会把 12 个字算成 36 个字 → 预扣冻结 3 倍；而结算期一旦拿不到
// 真实时长（opus/aac 裸流、无 Content-Length 的流式体），就会按 3 倍**收费**。
func TestSpecUsageAudioCharsCountsRunes(t *testing.T) {
	text := "你好世界，这是一句中文。"
	runes := int64(utf8.RuneCountInString(text))
	if runes == int64(len(text)) {
		t.Fatalf("样本必须是多字节字符，否则测不出单位错：runes=%d bytes=%d", runes, len(text))
	}
	h := relayHead{Input: json.RawMessage(strconv.Quote(text))}
	u := specUsageOf(kindAudioSpeech, h, billing.ImageSize{}, 15, imageCaps{})
	if u.AudioChars != runes {
		t.Fatalf("字数应为 %d 个字符，实际 %d（按字节算是 %d）", runes, u.AudioChars, len(text))
	}
}

// TestSpecUsageImageTier 顺带锁死图片档位归一（按秒之外的另一个量纲入口）。
func TestSpecUsageImageTier(t *testing.T) {
	// 档位由 resolveImageSize 归一后传入（它需要模型价目，specUsageOf 不重复算）。
	u := specUsageOf(kindImagesGenerations, relayHead{Quality: "hd", N: 2},
		billing.ImageSize{Tier: "1K", Source: billing.ImageSizeSourcePixels, Explicit: true}, 15, imageCaps{})
	if u.ImageSpec != "1K" || u.ImageN != 2 || u.ImageQuality != "hd" {
		t.Fatalf("图片用量解析不符：%+v", u)
	}
	// 同一串像素在不同模型上落到不同档 —— 由 resolveImageSize 决定，这里只验证快照搬运。
	u = specUsageOf(kindImagesGenerations, relayHead{},
		billing.ImageSize{Tier: "2K", Ratio: "16:9", Source: billing.ImageSizeSourcePixels, Explicit: true}, 15, imageCaps{})
	if u.ImageSpec != "2K" || u.ImageRatio != "16:9" || !u.ImageSpecExplicit {
		t.Fatalf("图片留痕搬运不符：%+v", u)
	}
}

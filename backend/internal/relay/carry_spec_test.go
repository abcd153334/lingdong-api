package relay

import (
	"testing"

	"lingdong-api/internal/service/billing"
)

// TestCarrySpecFieldsCarriesEveryBillingDimension 守住「请求侧计费维度整组搬运」。
//
// 背景：结算期组装用量走的是 buildBillingUsage → carrySpecFields，它只搬被列出的字段。
// 曾经漏掉 RefImages：冻结期按「参考图 + 生成张数」算钱，结算期 ref_images 归零，
// 于是只要价目里配了 ref_image_price，每条带参考图的请求都静默少收那部分。
// 这类缺陷不会报错、账单看起来正常，只能靠"整组搬"这条不变量守住。
//
// 判据：spec 里每个**请求侧声明**的维度，搬完必须逐字相等；
// 唯一允许被覆盖的是 ImageN（响应侧实际交付张数）与 AudioDurationSecs（响应可回报）。
func TestCarrySpecFieldsCarriesEveryBillingDimension(t *testing.T) {
	spec := billing.Usage{
		ImageSpec:         "2K",
		ImageRatio:        "16:9",
		ImageQuality:      "high",
		ImageN:            7,
		RefImages:         3,
		RefVideos:         2,
		RefAudios:         1,
		RefVideoSeconds:   11,
		VideoResolution:   "1080p",
		VideoSeconds:      12,
		ImageSpecExplicit: true,
		ImageSpecSource:   "tier",
	}
	var dst billing.Usage
	dst.ImageN = 99 // 预置一个"响应侧算出来的值"，验证搬运不会覆盖它
	carrySpecFields(&dst, spec)

	type check struct {
		name      string
		got, want any
	}
	for _, c := range []check{
		{"image_spec", dst.ImageSpec, spec.ImageSpec},
		{"image_ratio", dst.ImageRatio, spec.ImageRatio},
		{"image_quality", dst.ImageQuality, spec.ImageQuality},
		{"image_spec_explicit", dst.ImageSpecExplicit, spec.ImageSpecExplicit},
		{"image_spec_source", dst.ImageSpecSource, spec.ImageSpecSource},
		{"ref_images", dst.RefImages, spec.RefImages},
		{"ref_videos", dst.RefVideos, spec.RefVideos},
		{"ref_audios", dst.RefAudios, spec.RefAudios},
		{"ref_video_seconds", dst.RefVideoSeconds, spec.RefVideoSeconds},
		{"video_resolution", dst.VideoResolution, spec.VideoResolution},
		{"video_seconds", dst.VideoSeconds, spec.VideoSeconds},
	} {
		if c.got != c.want {
			t.Errorf("%s 没有被搬运：got %v want %v（新增请求侧维度必须同步加进 carrySpecFields）", c.name, c.got, c.want)
		}
	}
	if dst.ImageN != 99 {
		t.Errorf("ImageN 不该被 carrySpecFields 覆盖（由响应侧交付张数决定），got %d", dst.ImageN)
	}
}

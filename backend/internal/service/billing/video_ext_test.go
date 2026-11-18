package billing

import (
	"errors"
	"testing"

	"lingdong-api/internal/model"
)

// videoSnapRaw 用**严格解码器**构造快照：顺便证明这些键名与类型在真实保存路径上合法。
// 不用 map 字面量直接拼 struct，是为了让"键名写错"在测试里就炸，而不是等运营保存时炸。
func videoSnapRaw(raw string) Snapshot {
	sp, err := model.DecodeSpecPrices([]byte(raw))
	if err != nil {
		panic("videoSnapRaw: " + err.Error())
	}
	return Snapshot{ModelID: "vid", BillingMode: model.BillingModeSpec, SpecPrice: sp}
}

// videoSnapWith 在视频基线价目上叠加片段。
//
// 注意：不要在这里叠加第二份 second_price —— JSON 重复键后者覆盖前者，
// 用例会悄悄用错档位表。要换档位表就直接用 videoSnapRaw 写整份。
func videoSnapWith(extra string) Snapshot {
	raw := `{"kind":"per_video","second_price":{"480p":9600,"720p":12000,"1080p":18000}`
	if extra != "" {
		raw += "," + extra
	}
	raw += "}"
	return videoSnapRaw(raw)
}

// TestVideoChargeRefImageFree 锁定 ref_image_free 的语义。
//
// **语义反转（与旧 free_ref_images 相反）**：
//
//	旧 free_ref_images：0 = 全部免费（反直觉）
//	新 ref_image_free ：0 / 未配置 = **无免费**；N = 前 N 张免费
//
// 要"全部免费"就把单价填 0 —— 这样"免不免"只剩一条判据，不再有第二套语义。
func TestVideoChargeRefImageFree(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		imgs  int64
		want  int64
	}{
		// 720p × 1s = 12000，参考图单价 15000
		{"未配置=全部计费", `"ref_image_price":15000`, 7, 12000 + 7*15000},
		{"显式0=无免费(全部计费)", `"ref_image_price":15000,"ref_image_free":0`, 7, 12000 + 7*15000},
		{"前5张免费_7张", `"ref_image_price":15000,"ref_image_free":5`, 7, 12000 + 2*15000},
		{"前5张免费_刚好5张", `"ref_image_price":15000,"ref_image_free":5`, 5, 12000},
		{"前5张免费_不足5张", `"ref_image_price":15000,"ref_image_free":5`, 3, 12000},
		{"0张图", `"ref_image_price":15000,"ref_image_free":5`, 0, 12000},
		{"负数图数归零", `"ref_image_price":15000`, -3, 12000},
		// 单价 0 = 全部免费（取代旧的 free_ref_images=0 写法）
		{"单价0=全部免费", `"ref_image_price":0`, 9, 12000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := videoSnapWith(c.extra)
			got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 1, RefImages: c.imgs})
			if err != nil || got != c.want {
				t.Fatalf("want %d/nil, got %d/%v", c.want, got, err)
			}
		})
	}
}

// TestVideoChargeRefVideoBySecond 锁定「参考视频按秒、按输出档位取价」的口径。
func TestVideoChargeRefVideoBySecond(t *testing.T) {
	// 输出 720p × 3s = 36000；参考视频 720p 单价 12000 × 10s = 120000 → 合计 156000
	s := videoSnapWith(`"ref_video_second_price":{"480p":9600,"720p":12000,"1080p":18000,"2k":20000}`)
	if got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 3, RefVideoSeconds: 10}); err != nil || got != 156000 {
		t.Fatalf("want 156000/nil, got %d/%v", got, err)
	}
	// 参考视频单价按**输出档位**取键：同样 10 秒，1080p 输出 → 18000 × 10
	if got, err := Charge(s, Usage{VideoResolution: "1080p", VideoSeconds: 1, RefVideoSeconds: 10}); err != nil || got != 18000+180000 {
		t.Fatalf("want 198000/nil, got %d/%v", got, err)
	}
	// 某档未配 = 该档参考视频免费（不报错、不回落其它档）
	sPartial := videoSnapWith(`"ref_video_second_price":{"1080p":18000}`)
	if got, err := Charge(sPartial, Usage{VideoResolution: "720p", VideoSeconds: 3, RefVideoSeconds: 10}); err != nil || got != 36000 {
		t.Fatalf("want 36000/nil, got %d/%v", got, err)
	}
	// 秒数负数归零、超 MaxVideoSeconds 饱和
	if got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 1, RefVideoSeconds: -5}); err != nil || got != 12000 {
		t.Fatalf("want 12000/nil, got %d/%v", got, err)
	}
	// 饱和：12000 × 3600 = 43200000，加输出 1 秒
	if got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 1, RefVideoSeconds: 999999}); err != nil || got != 12000+43200000 {
		t.Fatalf("want 43212000/nil, got %d/%v", got, err)
	}
}

// TestVideoChargeRefVideoByCount 锁定「按条」口径（新区段缺省时）。
func TestVideoChargeRefVideoByCount(t *testing.T) {
	// 只有按条字段：720p × 2s = 24000 + 40000 × 2 条 = 104000
	s := videoSnapWith(`"ref_video_price":40000`)
	if got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 2, RefVideos: 2}); err != nil || got != 104000 {
		t.Fatalf("want 104000/nil, got %d/%v", got, err)
	}
	// 按条口径下 RefVideoSeconds 非 0：秒数不该被计费（只认条数）
	if got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 2, RefVideos: 2, RefVideoSeconds: 100}); err != nil || got != 104000 {
		t.Fatalf("want 104000/nil, got %d/%v", got, err)
	}
	// 两套同时非空：按秒接管，条数不再计费（保存期本应拒绝这种组合，这里锁运行期优先级）
	both := videoSnapWith(`"ref_video_price":40000,"ref_video_second_price":{"720p":12000}`)
	if got, err := Charge(both, Usage{VideoResolution: "720p", VideoSeconds: 2, RefVideos: 5, RefVideoSeconds: 3}); err != nil || got != 24000+36000 {
		t.Fatalf("want 60000/nil, got %d/%v", got, err)
	}
}

// TestVideoChargeLongVideoMultiplier 锁定超时溢价的三条硬约束：
//  1. 倍率是**微比例**（1.5 → 1500000），不是浮点倍数；
//  2. 只乘**时长费**，不乘素材费（素材是一次性成本，与视频长短无关）；
//  3. 阈值与倍率必须同时有效（阈值 > 0 且倍率 > 1e6）。
func TestVideoChargeLongVideoMultiplier(t *testing.T) {
	extra := `"ref_image_price":15000,"long_after_seconds":15,"long_video_multiplier":2000000`
	s := videoSnapWith(extra)
	// 15 秒（未超过阈值）：不乘 → 12000 × 15 = 180000
	if got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 15}); err != nil || got != 180000 {
		t.Fatalf("15s want 180000/nil, got %d/%v", got, err)
	}
	// 16 秒：时长费 ×2 → 12000 × 16 × 2 = 384000
	if got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 16}); err != nil || got != 384000 {
		t.Fatalf("16s want 384000/nil, got %d/%v", got, err)
	}
	// 素材费**不乘**：384000 + 图×1(15000) = 399000（旧实现是 414000，会重复加价）
	if got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 16, RefImages: 1}); err != nil || got != 399000 {
		t.Fatalf("16s+img want 399000/nil, got %d/%v", got, err)
	}
	// 折扣在溢价之后末乘：(12000×16×2) × 0.8 = 307200
	sg := videoSnapWith(extra)
	sg.Discount = 800_000
	if got, err := Charge(sg, Usage{VideoResolution: "720p", VideoSeconds: 16}); err != nil || got != 307200 {
		t.Fatalf("discount want 307200/nil, got %d/%v", got, err)
	}
	// 半配：只有阈值没有倍率 → 不生效
	half := videoSnapWith(`"long_after_seconds":15`)
	if got, err := Charge(half, Usage{VideoResolution: "720p", VideoSeconds: 16}); err != nil || got != 192000 {
		t.Fatalf("half-config want 192000/nil, got %d/%v", got, err)
	}
	// 倍率 = 1e6（正好 1 倍）视为不启用
	noop := videoSnapWith(`"long_after_seconds":15,"long_video_multiplier":1000000`)
	if got, err := Charge(noop, Usage{VideoResolution: "720p", VideoSeconds: 16}); err != nil || got != 192000 {
		t.Fatalf("mult=1e6 want 192000/nil, got %d/%v", got, err)
	}
}

// TestVideoChargeAllFree 全免费时返回 0（不因折扣变负或异常）。
func TestVideoChargeAllFree(t *testing.T) {
	// 秒价 0 + 素材价 0 = 全免费（新语义：免费用"单价 0"表达，不用 free 字段）
	s := videoSnapRaw(`{"kind":"per_video","second_price":{"480p":0,"720p":0,"1080p":0},"ref_image_price":0}`)
	got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 0, RefImages: 9})
	if err != nil || got != 0 {
		t.Fatalf("want 0/nil, got %d/%v", got, err)
	}
}

// TestMaxSecondsOnlyOnEstimate：同一份入参，Estimate 必须拒绝，Charge 必须成功 ——
// 否则已冻结的任务会因结算报错永远停在 running，产物白丢。
func TestMaxSecondsOnlyOnEstimate(t *testing.T) {
	s := videoSnapWith(`"max_seconds":30`)
	u := Usage{VideoResolution: "720p", VideoSeconds: 31}

	if _, err := Estimate(s, 0, 0, u); !errors.Is(err, ErrOverMaxSeconds) {
		t.Fatalf("Estimate want ErrOverMaxSeconds, got %v", err)
	}
	got, err := Charge(s, u)
	if err != nil {
		t.Fatalf("Charge 不得因 max_seconds 失败: %v", err)
	}
	if want := int64(12000 * 31); got != want {
		t.Fatalf("Charge want %d, got %d", want, got)
	}
	// 恰好等于上限时必须通过
	if _, err := Estimate(s, 0, 0, Usage{VideoResolution: "720p", VideoSeconds: 30}); err != nil {
		t.Fatalf("30s 应通过: %v", err)
	}
	// max_seconds <= 0 视为未配置
	off := videoSnapWith(`"max_seconds":0`)
	if _, err := Estimate(off, 0, 0, Usage{VideoResolution: "720p", VideoSeconds: 999}); err != nil {
		t.Fatalf("max_seconds=0 应放行: %v", err)
	}
}

// TestStrictLimitsOnlyAffectsVideo 确保 strictLimits 形参没有误伤图片计费两条路径。
func TestStrictLimitsOnlyAffectsVideo(t *testing.T) {
	s := imageSnap("4K")
	u := Usage{ImageSpec: "4K", ImageN: 2}
	est, err := Estimate(s, 0, 0, u)
	if err != nil {
		t.Fatalf("Estimate err: %v", err)
	}
	chg, err := Charge(s, u)
	if err != nil {
		t.Fatalf("Charge err: %v", err)
	}
	if est != chg || est != 56000 {
		t.Fatalf("图片两条路径必须等价: est=%d chg=%d", est, chg)
	}
}

// TestVideoChargeEstimateChargeAgree 确认未触发 max_seconds 时预扣与结算金额一致
// （ReclaimSettle 依赖此性质：不一致就会产生虚假差额流水）。
func TestVideoChargeEstimateChargeAgree(t *testing.T) {
	specs := []string{
		`"ref_image_price":15000,"ref_image_free":5`,
		`"ref_video_second_price":{"720p":12000},"ref_audio_price":25000`,
		`"ref_video_price":40000,"long_after_seconds":10,"long_video_multiplier":2000000`,
		`"max_seconds":30`,
	}
	u := Usage{VideoResolution: "720p", VideoSeconds: 12, RefImages: 7, RefVideos: 1, RefAudios: 2, RefVideoSeconds: 8}
	for _, sp := range specs {
		s := videoSnapWith(sp)
		s.Discount = 900_000 // 九折
		est, err := Estimate(s, 0, 0, u)
		if err != nil {
			t.Fatalf("%s Estimate err: %v", sp, err)
		}
		chg, err := Charge(s, u)
		if err != nil {
			t.Fatalf("%s Charge err: %v", sp, err)
		}
		if est != chg {
			t.Fatalf("%s 预扣与结算不一致: est=%d chg=%d", sp, est, chg)
		}
	}
}

// TestVideoCharge2KTier 锁定 2k 档位可直接命中（小写原样透传）。
func TestVideoCharge2KTier(t *testing.T) {
	s := videoSnapRaw(`{"kind":"per_video","second_price":{"480p":9600,"720p":12000,"1080p":18000,"2k":20834}}`)
	if got, err := Charge(s, Usage{VideoResolution: "2k", VideoSeconds: 10}); err != nil || got != 208340 {
		t.Fatalf("want 208340/nil, got %d/%v", got, err)
	}
	// 未配 2k 的模型 → ErrInvalidTier（不静默降档）
	no2k := videoSnapWith("")
	if _, err := Charge(no2k, Usage{VideoResolution: "2k", VideoSeconds: 10}); !errors.Is(err, ErrInvalidTier) {
		t.Fatalf("want ErrInvalidTier, got %v", err)
	}
}

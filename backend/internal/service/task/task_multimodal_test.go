package task

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// 本文件锁死「多模态提交校验」这条链路的**唯一不可回退项**：
// validateModality 必须把归一后的计费档位同时写回 `req.Resolution`（出站体用）
// 与 `req.Params.Resolution`（结算用）。只写前者会让任务"提交成功、跑完、每轮结算
// ErrInvalidTier"——worker 只 warn 不失败，一路卡到 deadline 全额退款、产物白送。
//
// 为什么用「Estimate 金额 == Charge 金额」来锁：冻结走 usageOfRequest（读 req.*），
// 结算走 usageOf（读落库 JSON）。两者不相等只有两种可能：档位没回写，或两处口径漂移，
// 两种都是账务事故。

// videoSpecFixed 是可灵式价目：上游没有 resolution 参数，画质由型号决定。
var videoSpecFixed = model.SpecPrices{
	Kind:            "per_video",
	SecondPrice:     map[string]int64{"480p": 9600, "720p": 12000, "1080p": 18000},
	ResolutionFixed: "720p",
	MaxSeconds:      i64ptr(10),
}

// videoSpecNoFixed 需要调用方显式传档位的普通视频模型。
var videoSpecNoFixed = model.SpecPrices{
	Kind:        "per_video",
	SecondPrice: map[string]int64{"480p": 9600, "720p": 12000, "1080p": 18000},
}

func videoSnap(sp model.SpecPrices) billing.Snapshot {
	return billing.Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: sp}
}

// TestValidateVideoFixedTierFallback 不传 resolution 时按 resolution_fixed 计价。
func TestValidateVideoFixedTierFallback(t *testing.T) {
	req := &SubmitRequest{Seconds: 5}
	snap := videoSnap(videoSpecFixed)

	if err := validateModality(req, snap, "video", nil); err != nil {
		t.Fatalf("配了固定档位的模型不传 resolution 必须放行: %v", err)
	}
	// 出站不发档位：这类模型的画质由型号决定，替它发一个 resolution 反而可能被拒。
	if req.Resolution != "" {
		t.Fatalf("用户没写档位时出站必须为空，实际 %q", req.Resolution)
	}
	// 这一条是防「静默退款」的关键：结算只读落库参数
	if req.Params.Resolution != "720p" {
		t.Fatalf("req.Params.Resolution 必须是 720p（否则结算每轮 ErrInvalidTier），实际 %q", req.Params.Resolution)
	}
}

// TestValidateVideoTierMissingFallsBack 没有固定档位又不传档位 → 不拒绝，按最高价档兜底。
//
// 参数不再被拦截：档位是计价维度，不是合法性判据。用户没写档位时上游按型号默认出片，
// 平台按「固定档 → 最高价档」计费并留 `video_tier_missing` 标记（否则按 0 元成交）。
func TestValidateVideoTierMissingFallsBack(t *testing.T) {
	req := &SubmitRequest{Seconds: 5}
	if err := validateModality(req, videoSnap(videoSpecNoFixed), "video", nil); err != nil {
		t.Fatalf("没写档位不应拒绝：%v", err)
	}
	if req.Params.Resolution == "" {
		t.Fatal("必须兜底出一个可计价的档位（否则结算拿不到单价）")
	}
	if !strings.Contains(req.Params.Anomaly, model.AnomalyVideoTierMissing) {
		t.Fatalf("必须留 video_tier_missing 标记，实际 %q", req.Params.Anomaly)
	}
	// 出站不该替用户编一个档位：没写就不发。
	if req.Resolution != "" {
		t.Fatalf("用户没写档位时 req.Resolution 必须为空（出站不下发），实际 %q", req.Resolution)
	}
}

// TestValidateVideoTierUnpricedFallsBack 请求了价目里没有的档位 → 不拒绝，按高价档计费。
//
// 现实场景：用户照官方文档传 `1440p`，而运营只配了 480p/720p/1080p。
// 以前这里 400（"本模型不支持"），可支持与否只有上游说了算。
func TestValidateVideoTierUnpricedFallsBack(t *testing.T) {
	req := &SubmitRequest{Seconds: 5, Resolution: "2k"}
	if err := validateModality(req, videoSnap(videoSpecNoFixed), "video", nil); err != nil {
		t.Fatalf("未配价的档位不应拒绝：%v", err)
	}
	if req.Resolution != "2k" {
		t.Fatalf("出站必须保留用户原话，实际 %q", req.Resolution)
	}
	if req.Params.Resolution == "" || req.Params.Resolution == "2k" {
		t.Fatalf("计费档必须换成价目里的档位，实际 %q", req.Params.Resolution)
	}
	if !strings.Contains(req.Params.Anomaly, model.AnomalyVideoTierUnpriced) {
		t.Fatalf("必须留 video_tier_unpriced 标记，实际 %q", req.Params.Anomaly)
	}
}

// TestValidateVideoZeroSecondsFallsBack 0 秒不再拒绝：按模型上限冻结，结算以上游为准。
//
// 为什么不能按 0 秒算：0 元冻结 = 白送一次生成。也不能拒绝：用户没写时长是
// 官方文档允许的写法（缺省即默认值）。所以冻结按最坏情况占额，结算按上游实际时长。
func TestValidateVideoZeroSecondsFallsBack(t *testing.T) {
	req := &SubmitRequest{}
	if err := validateModality(req, videoSnap(videoSpecFixed), "video", nil); err != nil {
		t.Fatalf("没写秒数不应拒绝：%v", err)
	}
	if req.Seconds <= 0 {
		t.Fatalf("必须冻结一个正数预算，实际 %d", req.Seconds)
	}
	if !req.Params.SecondsAuto {
		t.Fatal("未写秒数必须标记 seconds_auto（结算以上游为准）")
	}
	if !strings.Contains(req.Params.Anomaly, model.AnomalyVideoSecondsMissing) {
		t.Fatalf("必须留 video_seconds_missing 标记，实际 %q", req.Params.Anomaly)
	}
}

// TestFixedTierEstimateEqualsCharge 是 B2 的回归用例：冻结金额与结算金额必须相等。
//
// 构造顺序刻意贴合真实链路：ParseSubmit（真实请求体）→ validateModality（回写）
// → usageOfRequest 冻结 → 参数序列化进 tasks.request_params → usageOf 结算。
// 跳过 ParseSubmit 手工填字段会掩盖「Params.Seconds 没落库」这类真实缺陷。
func TestFixedTierEstimateEqualsCharge(t *testing.T) {
	cases := []struct {
		name string
		spec model.SpecPrices
		body string
	}{
		{"固定档位·不传 resolution", videoSpecFixed, `{"model":"kling-v3-std","prompt":"一只猫","seconds":5}`},
		{"固定档位·重复传同档", videoSpecFixed, `{"model":"kling-v3-std","prompt":"一只猫","seconds":10,"resolution":"720p"}`},
		{"显式档位·1080p", videoSpecNoFixed, `{"model":"seedance","prompt":"一只猫","seconds":5,"resolution":"1080p"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := ParseSubmit([]byte(c.body))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			snap := videoSnap(c.spec)
			if err := validateModality(req, snap, "video", nil); err != nil {
				t.Fatalf("校验失败: %v", err)
			}

			frozen, err := billing.Estimate(snap, 0, 0, usageOfRequest(req))
			if err != nil {
				t.Fatalf("冻结计价失败: %v", err)
			}
			// 与 Submit 落库的写法保持一致：整份 params 序列化进 request_params
			row := &model.Task{RequestParams: mustJSON(req.Params)}
			settled, err := billing.Charge(snap, usageOf(row))
			if err != nil {
				t.Fatalf("结算失败（档位没回写就会出现 ErrInvalidTier）: %v", err)
			}
			if frozen != settled {
				t.Fatalf("冻结 %d != 结算 %d（档位=%q 秒数=%d）", frozen, settled, req.Params.Resolution, req.Seconds)
			}
			if frozen <= 0 {
				t.Fatalf("金额必须为正，实际 %d", frozen)
			}
		})
	}
}

// TestValidateImageModality 图像：张数归一与上限、参考图上限、档位归一。
func TestValidateImageModality(t *testing.T) {
	snap := billing.Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: model.SpecPrices{
		Kind:            "per_image",
		TierPrice:       map[string]int64{"1K": 14000, "2K": 20000, "4K": 28000},
		ResolutionFixed: "1K",
		MaxN:            i64ptr(4),
	}}

	t.Run("未传张数归一为 1", func(t *testing.T) {
		req := &SubmitRequest{}
		if err := validateModality(req, snap, "image", nil); err != nil {
			t.Fatalf("校验失败: %v", err)
		}
		if req.Params.ImageN != 1 {
			t.Fatalf("ImageN 必须归一为 1，实际 %d", req.Params.ImageN)
		}
	})

	t.Run("超过 n_max 夹紧而不是拒绝", func(t *testing.T) {
		// 拒绝会让一次本该成功的生成彻底失败；夹紧后用户拿到的就是上限张数。
		req := &SubmitRequest{}
		req.Params.ImageN = 5
		if err := validateModality(req, snap, "image", nil); err != nil {
			t.Fatalf("超过 n_max 不应拒绝：%v", err)
		}
		if req.Params.ImageN != 4 {
			t.Fatalf("应夹到 max_n=4，实际 %d", req.Params.ImageN)
		}
		if !strings.Contains(req.Params.Anomaly, model.AnomalyImageNCapped) {
			t.Fatalf("夹紧必须留 image_n_capped，实际 %q", req.Params.Anomaly)
		}
	})

	// 像素写法在**两条链路**上同一口径：都收，并按模型价目吸附到计费档。
	// 只拒一边的后果是「同一个 size，一个入口能过、另一个报错」，用户无从判断该传什么。
	t.Run("像素写法照收并按模型档位归一", func(t *testing.T) {
		for raw, want := range map[string]string{
			"1024x1024": "1K", "1443x1090": "1K", "2048*2048": "2K", "1024×1024": "1K",
			// 这套价目只有 1K/2K/4K：2048x1152（2.36MP）吸附到 2K，而不是 1.5K
			"2048x1152": "2K",
		} {
			req := &SubmitRequest{}
			req.Params.SizeRaw = raw
			if err := validateModality(req, snap, "image", nil); err != nil {
				t.Fatalf("%q 不得被拒: %v", raw, err)
			}
			if req.Params.ImageSpec != want {
				t.Fatalf("%q 应归一为 %q，实际 %q", raw, want, req.Params.ImageSpec)
			}
			if !req.Params.ImageExplicit {
				t.Fatalf("%q 必须标记为「用户显式指定」（否则 tier_mismatch 永不比对）", raw)
			}
			if req.Params.SizeRaw != raw {
				t.Fatalf("原文必须保留（重放体要用它），实际 %q", req.Params.SizeRaw)
			}
		}
	})

	t.Run("档位与无尺寸写法", func(t *testing.T) {
		// auto / 空 → 默认档（default_price 反推），但不算用户显式指定。
		for raw, want := range map[string]string{"auto": "1K", "": "1K"} {
			req := &SubmitRequest{}
			req.Params.SizeRaw = raw
			if err := validateModality(req, snap, "image", nil); err != nil {
				t.Fatalf("%q 不得被拒: %v", raw, err)
			}
			if req.Params.ImageSpec != want {
				t.Fatalf("%q 应归一为 %q，实际 %q", raw, want, req.Params.ImageSpec)
			}
			if req.Params.ImageExplicit {
				t.Fatalf("%q 不得标记为显式（auto/空 由上游挑档，拿它比对必然假告警）", raw)
			}
			if req.Params.ImageSource != "auto" && req.Params.ImageSource != "empty" {
				t.Fatalf("%q 的来源应记 auto/empty，实际 %q", raw, req.Params.ImageSource)
			}
		}
	})

	t.Run("冲突不再拦截但按高价档计费", func(t *testing.T) {
		// 2048x2048 属于 2K 档，声明 1K 就是自相矛盾：不再 400，
		// 按两者中**单价更高**的档计费（绝不按低价糊弄），并留 anomaly。
		req := &SubmitRequest{}
		req.Params.SizeRaw = "2048x2048"
		req.Params.ResolutionRaw = "1K"
		if err := validateModality(req, snap, "image", nil); err != nil {
			t.Fatalf("冲突不应再 400：%v", err)
		}
		if req.Params.ImageSpec == "" {
			t.Fatal("必须仍然解释出一个可计价的档位")
		}
		// 只写档位（不写像素）时必须标记显式：否则 tier_mismatch 永远不比对。
		tierOnly := &SubmitRequest{}
		tierOnly.Params.SizeRaw = "2K"
		if err := validateModality(tierOnly, snap, "image", nil); err != nil {
			t.Fatalf("档位写法不得被拒: %v", err)
		}
		if !tierOnly.Params.ImageExplicit {
			t.Fatal("显式写档位必须标记 ImageExplicit")
		}
	})
}

// TestImageEstimateEqualsCharge 图像同样要求冻结与结算一致（档位×张数）。
func TestImageEstimateEqualsCharge(t *testing.T) {
	snap := billing.Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: model.SpecPrices{
		Kind:            "per_image",
		TierPrice:       map[string]int64{"1K": 14000, "2K": 20000},
		ResolutionFixed: "1K",
		MaxN:            i64ptr(4),
	}}

	req := &SubmitRequest{}
	req.Params.ImageSpec = "1K"
	req.Params.ImageN = 3
	if err := validateModality(req, snap, "image", nil); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	frozen, err := billing.Estimate(snap, 0, 0, usageOfRequest(req))
	if err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	settled, err := billing.Charge(snap, usageOf(&model.Task{RequestParams: mustJSON(req.Params)}))
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if frozen != settled {
		t.Fatalf("冻结 %d != 结算 %d", frozen, settled)
	}
	// 1K 单价 14000 × 3 张
	if frozen != 42000 {
		t.Fatalf("1K×3 应为 42000，实际 %d", frozen)
	}
}

// TestValidateAudioModality 音乐/歌词：按次计费，不看档位也不看张数。
//
// 这里**不拒绝任何参数**：一次出几个结果由上游决定（SUNO 历来一次 2 首）。
// 平台若在这里拦 n>1，等于替上游宣布一个并不存在的能力上限。
func TestValidateAudioModality(t *testing.T) {
	for _, n := range []int64{0, 1, 2, 8} {
		req := &SubmitRequest{}
		req.Params.ImageN = n
		if err := validateModality(req, billing.Snapshot{BillingMode: model.BillingModeCall}, "audio", nil); err != nil {
			t.Fatalf("n=%d 时音乐校验不该拒绝，实际 %v", n, err)
		}
	}
}

// TestImageTierByModel 锁死「同一串像素按模型档位集吸附」。
//
// 旧实现按长边分档（1024/2048 与 1536/2560 两套阈值），同一个 size 在同步与异步
// 链路落进不同价档。现在两边都调 billing.ResolveImageTier，可选项来自该模型的价目表。
func TestImageTierByModel(t *testing.T) {
	// pro 已不卖 1.5K；这里刻意保留一条存量键，守「折档后仍不出现第五档」。
	pro := tierPriceForTest(map[string]int64{"1K": 300000, "1.5K": 300000, "2K": 450000}, "2K")
	lite := tierPriceForTest(map[string]int64{"2K": 300000, "3K": 450000, "4K": 600000}, "2K")
	cases := []struct {
		size, pro, lite string // "" = 期望拒绝
	}{
		{"1024x1024", "1K", "2K"},
		{"1536x1024", "1.5K", "2K"}, // 1.57MP：pro 平局取更高档 1.5K；lite 无 1.5K 档 → 2K
		{"1536x1536", "1.5K", "2K"}, // 官方 1.5K 的 1:1；lite 没有 1.5K 就回到就近档
		{"2048x2048", "2K", "2K"},
		{"2816x1584", "2K", "2K"},
		{"3072x3072", "2K", "3K"}, // pro 无 3K 档 → 面积超窗仍按最高档 2K 收
		{"4096x4096", "2K", "4K"}, // pro 最高档就是 2K（超窗按最高档收）
	}
	for _, c := range cases {
		for _, m := range []struct {
			name string
			sp   model.SpecPrices
			want string
		}{{"pro", pro, c.pro}, {"lite", lite, c.lite}} {
			// 尺寸解释不再报错：解释不出档位时按最高价档计费（只留标记）。
			size := billing.ParseImageSize(billing.ImageDecl{Size: c.size})
			billing.ResolveImageTier(m.sp, &size)
			if size.Tier != m.want {
				t.Fatalf("%s/%s = %q，期望 %q", c.size, m.name, size.Tier, m.want)
			}
		}
	}
}

// tierPriceForTest 构造一个只含图像档位的价目。
func tierPriceForTest(tiers map[string]int64, defTier string) model.SpecPrices {
	return model.SpecPrices{Kind: "per_image", TierPrice: tiers, ResolutionFixed: defTier}
}

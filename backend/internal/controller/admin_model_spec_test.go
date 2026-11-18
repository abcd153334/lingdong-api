package controller

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// mustSpec 用**严格解码器**把测试 JSON 变成 model.SpecPrices。
//
// 用解码器而不是直接拼结构体：这样键名写错会立刻炸（旧键名 *_quota 一律拒绝），
// 而不是让用例悄悄测一个"结构体里本来就有"的字段。
func mustSpec(t *testing.T, raw string) model.SpecPrices {
	t.Helper()
	sp, err := model.DecodeSpecPrices([]byte(raw))
	if err != nil {
		t.Fatalf("测试数据本身不合法（%s）：%v", raw, err)
	}
	return sp
}

// TestValidSpecPricesVideo 覆盖视频价目的保存期校验（成对性 / 白名单 / 区间 / 越界字段）。
//
// 语义要点（与旧版不同）：
//   - 秒价档位可以是 0（该档免费），只拒负数；
//   - 超时倍率是**微比例**（1.5 → 1500000），等于 1 倍不算配置；
//   - per_task_price 在 per_video 上属于"越界字段"，必须报错而不是被忽略 ——
//     运营以为按次兜底生效、实际按秒算，是最难查的那种错账。
func TestValidSpecPricesVideo(t *testing.T) {
	base := `"kind":"per_video", "max_seconds": 10,"second_price":{"480p":9600,"720p":12000,"1080p":18000}`

	ok := []string{
		`{` + base + `}`,
		`{"kind":"per_video", "max_seconds": 10,"second_price":{"480p":9600,"720p":12000,"1080p":18000,"2k":20834}}`,
		`{` + base + `,"ref_image_free":0}`,
		`{` + base + `,"ref_image_free":5,"max_seconds":30}`,
		`{` + base + `,"ref_video_second_price":{"720p":12000,"2k":20000}}`,
		`{` + base + `,"long_after_seconds":15,"long_video_multiplier":2000000}`,
		// 不要求三档齐全：上游只卖 480p/720p 同样合法。强制齐全只会逼运营填假价。
		`{"kind":"per_video", "max_seconds": 10,"second_price":{"480p":9600,"720p":12000}}`,
		`{"kind":"per_video", "max_seconds": 10,"second_price":{"2k":20834}}`,
		// 0 价 = 该档免费（显式免费，不是"未配置"）。
		`{"kind":"per_video", "max_seconds": 10,"second_price":{"480p":0,"720p":0,"1080p":0}}`,
		// 4k 自 Seedance 2.0 起是合法档位（参考视频档位也同理）。
		`{"kind":"per_video", "max_seconds": 10,"second_price":{"720p":12000,"4k":32000}}`,
		`{` + base + `,"ref_video_second_price":{"4k":20000}}`,
		`{` + base + `,"ref_video_second_price":{"720p":0}}`,
	}
	for _, raw := range ok {
		if err := validSpecPrices("video", mustSpec(t, raw)); err != nil {
			t.Fatalf("应通过却被拒：%s → %v", raw, err)
		}
	}

	bad := map[string]string{
		// 至少一档：完全没有任何档位 = 无法定价。
		`{"kind":"per_video", "max_seconds": 10,"second_price":{}}`: "至少配置一个档位",
		`{"kind":"per_video", "max_seconds": 10}`:                   "至少配置一个档位",
		// 档位不再有白名单：8k 这类词表外的键可以存（支持与否由官方文档定）；
		// 不可落库的键才拒（空 / 含逗号 / 过长）。
		`{"kind":"per_video", "max_seconds": 10,"second_price":{"":20000}}`:      "不能为空",
		`{"kind":"per_video", "max_seconds": 10,"second_price":{"8k,4k":20000}}`: "逗号",
		`{` + base + `,"second_price":{"480p":-1,"720p":12000}}`:                 "不能为负数",
		`{` + base + `,"ref_video_second_price":{"":20000}}`:                     "不能为空",
		`{` + base + `,"ref_video_second_price":{"720p":-1}}`:                    "不能为负数",
		`{` + base + `,"ref_image_price":-1}`:                                    "不能为负数",
		`{` + base + `,"ref_image_free":-1}`:                                     "不能为负数",
		// 免费张数是**数量**，必须有上界：没有上界时前端一次单位错
		// （2 被当成 2 元写成 2000000）就能静默把素材费变成永久 0。
		`{` + base + `,"ref_image_free":2000000}`: "0–128",
		`{` + base + `,"max_seconds":0}`:          "1–3600",
		`{` + base + `,"max_seconds":9999}`:       "1–3600",
		// 阈值与倍率必须成对：只填一个会静默不生效。
		`{` + base + `,"long_after_seconds":15}`:                                 "必须同时填写",
		`{` + base + `,"long_video_multiplier":2000000}`:                         "必须同时填写",
		`{` + base + `,"long_after_seconds":15,"long_video_multiplier":1000000}`: "必须大于 1",
		`{` + base + `,"long_after_seconds":0,"long_video_multiplier":2000000}`:  "1–3600",
		// 按条与按秒互斥。
		`{` + base + `,"ref_video_price":40000,"ref_video_second_price":{"720p":12000}}`: "只能选一种",
		// 越界字段：per_task_price 在 per_video 上会被静默忽略 → 必须拒。
		`{"kind":"per_video", "max_seconds": 10,"second_price":{"720p":12000},"per_task_price":50000}`: "不支持字段",
		`{"kind":"per_image", "max_n": 4,"resolution_fixed":"1K","tier_price":{"1K":1}}`:               "必须是 per_video",
	}
	for raw, want := range bad {
		err := validSpecPrices("video", mustSpec(t, raw))
		if err == nil {
			t.Fatalf("应被拒：%s", raw)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s：文案应含 %q，实际 %q", raw, want, err.Error())
		}
	}
}

// TestValidSpecPricesImage 覆盖图片价目的档位键与兜底价校验。
//
// 与旧版的差别：默认价现在是**必填**（档位未命中必须有兜底，否则运行期 ErrInvalidTier）。
func TestValidSpecPricesImage(t *testing.T) {
	ok := []string{
		// 三档齐全 + 默认档（不传 size / auto 时按它计费）。
		`{"kind":"per_image", "max_n": 4,"tier_price":{"1K":28000,"2K":56000,"4K":112000},"resolution_fixed":"2K"}`,
		// 只卖一档也合法：配了哪些档就代表开放哪些档。
		`{"kind":"per_image", "max_n": 4,"tier_price":{"1K":28000},"resolution_fixed":"1K"}`,
		// 小写档位键/默认档按归一化处理（运营常从文档里抄小写）。
		`{"kind":"per_image", "max_n": 4,"tier_price":{"2k":56000},"resolution_fixed":"2k"}`,
		// 0 价合法（免费模型）。
		`{"kind":"per_image", "max_n": 4,"tier_price":{"1K":0,"2K":0,"4K":0},"resolution_fixed":"2K"}`,
		// 不配默认档也**可保存**（运营可能先配价、后定默认档），但运行期
		// 「不传 size」的请求会被拒绝 —— 这条 warning 就是保存时的提示。
		`{"kind":"per_image", "max_n": 4,"tier_price":{"1K":28000,"2K":56000}}`,
	}
	for _, raw := range ok {
		if err := validSpecPrices("image", mustSpec(t, raw)); err != nil {
			t.Fatalf("应通过却被拒：%s → %v", raw, err)
		}
	}

	bad := map[string]string{
		// 负数拒绝。
		`{"kind":"per_image", "max_n": 4,"tier_price":{"1K":-1}}`: "不能为负数",
		// 档位键过长（>32 字符）必须拒：它会落进 usages.image_spec 这一列。
		`{"kind":"per_image", "max_n": 4,"tier_price":{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA":28000}}`: "过长",
		`{"kind":"per_image", "max_n": 4}`: "至少配置一个分辨率档位",
		// 默认档指向价目外的键：硬错。运行期虽有兜底（最低价档 + 标记），
		// 但放行就等于允许「价格写着 2K、实际按 1K 收」的模型存在。
		`{"kind":"per_image", "max_n": 4,"tier_price":{"1K":28000},"resolution_fixed":"8K"}`: "不在已配置的分辨率档位里",
		`{"kind":"per_video", "max_seconds": 10,"tier_price":{"1K":1}}`:                      "必须是 per_image",
		// 越界字段：按秒字段配在图片模型上是意图错误。
		`{"kind":"per_image", "max_n": 4,"tier_price":{"1K":1},"second_price":{"720p":1}}`: "不支持字段",
	}
	for raw, want := range bad {
		err := validSpecPrices("image", mustSpec(t, raw))
		if err == nil {
			t.Fatalf("应被拒：%s", raw)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s：文案应含 %q，实际 %q", raw, want, err.Error())
		}
	}
}

// TestSharedSpecPayloadsAreValid 用**前后端共用**的 fixture 定义「什么是一个合法的
// spec_prices」。
//
// 存在的理由：后端测试一直证明 `max_seconds:30` 是合法的，前端却曾把它发成
// `30000000` —— 两边各自跑绿，合起来是 400。契约必须由同一份数据定义：
//
//	Go  ：解码 + 保存期校验必须全部通过
//	TS  ：specToForm → buildSpecPrices 往返必须深等于原值（web/src/test/spec-form.test.ts）
func TestSharedSpecPayloadsAreValid(t *testing.T) {
	raw, err := os.ReadFile("../../../testdata/spec_payloads.json")
	if err != nil {
		t.Fatalf("读不到共用 fixture：%v", err)
	}
	var cases []struct {
		Name     string          `json:"name"`
		Modality string          `json:"modality"`
		Spec     json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("fixture 不是合法 JSON：%v", err)
	}
	if len(cases) == 0 {
		t.Fatal("fixture 为空：两侧的契约测试会变成空转")
	}
	for _, c := range cases {
		sp, err := model.DecodeSpecPrices(c.Spec)
		if err != nil {
			t.Fatalf("%s：严格解码失败（键名或类型不对）：%v", c.Name, err)
		}
		if err := validSpecPrices(c.Modality, sp); err != nil {
			t.Fatalf("%s：应通过却被拒：%v", c.Name, err)
		}
	}
}

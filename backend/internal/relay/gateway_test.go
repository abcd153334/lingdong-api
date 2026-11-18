package relay

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

func TestEffWeightSmoothing(t *testing.T) {
	// 有效权重 = weight + 10；负值钳 0
	cases := []struct{ in, want int }{{0, 10}, {5, 15}, {90, 100}, {-3, 10}}
	for _, c := range cases {
		if got := effWeight(c.in); got != c.want {
			t.Fatalf("effWeight(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestGroupMatches(t *testing.T) {
	raw := model.JSON(`["default","vip"]`)
	if !groupMatches(raw, "") {
		t.Fatal("空分组应命中 default")
	}
	if !groupMatches(raw, "vip") {
		t.Fatal("应命中 vip")
	}
	if groupMatches(raw, "svip") {
		t.Fatal("不应命中 svip")
	}
	// 渠道未配置分组 → 仅命中 default
	if !groupMatches(nil, "default") || groupMatches(nil, "vip") {
		t.Fatal("空分组配置应仅命中 default")
	}
	if !groupMatches(model.JSON(`bad-json`), "default") {
		t.Fatal("非法 JSON 应回落 default 命中")
	}
}

func TestShuffleWeightedKeepsElements(t *testing.T) {
	list := []Candidate{
		{Channel: &model.Channel{ID: 1}, Weight: 0},
		{Channel: &model.Channel{ID: 2}, Weight: 5},
		{Channel: &model.Channel{ID: 3}, Weight: 10},
	}
	shuffleWeighted(list)
	seen := map[uint64]bool{}
	for _, c := range list {
		seen[c.Channel.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("洗牌后元素丢失: %v", seen)
	}
	if shuffleWeighted([]Candidate{{Channel: &model.Channel{ID: 9}}}); true {
		// 单元素不应变化（仅验证不 panic）
	}
}

func TestRetryableStatus(t *testing.T) {
	if !retryableStatus(429) || !retryableStatus(500) || !retryableStatus(503) {
		t.Fatal("429/5xx 应可重试")
	}
	if retryableStatus(400) || retryableStatus(401) || retryableStatus(404) {
		t.Fatal("4xx（除 429）不应重试")
	}
}

func TestParseUsage(t *testing.T) {
	body := []byte(`{"id":"x","usage":{"prompt_tokens":120,"completion_tokens":30,
		"prompt_tokens_details":{"cached_tokens":40,"audio_tokens":7},
		"completion_tokens_details":{"audio_tokens":3}}}`)
	u := ParseUsage(body)
	if u.PromptTokens != 120 || u.CompletionTokens != 30 || u.CacheTokens != 40 ||
		u.AudioInputTokens != 7 || u.AudioOutputTokens != 3 {
		t.Fatalf("解析用量不符: %+v", u)
	}
	if u := ParseUsage([]byte(`{"id":"x"}`)); u.PromptTokens != 0 {
		t.Fatal("无 usage 应返回零值")
	}
}

// TestParseImageSizeAcceptsPixels 锁死「像素写法不再被 400」。
//
// 三种写法（档位 / 像素 / 比例）都必须能解析出来；比例写法只能走 aspect_ratio 字段，
// 混进 size 会被明确拒绝（否则用户以为设了 16:9，实际只是一串认不出的字符）。
func TestParseImageSizeAcceptsPixels(t *testing.T) {
	for _, in := range []string{"1024x1024", "1024X1024", "2048*2048", "1443×1090", " 2816x1584 "} {
		got := billing.ParseImageSize(billing.ImageDecl{Size: in})
		if got.Source != billing.ImageSizeSourcePixels || got.Width == 0 {
			t.Fatalf("ParseImageSize(%q) = %+v", in, got)
		}
	}
	// size 里写比例不再报错：按画幅理解并原样保留（上游认不认由它决定）。
	if got := billing.ParseImageSize(billing.ImageDecl{Size: "16:9"}); got.Ratio != "16:9" {
		t.Fatalf("size 写比例应识别为画幅，实际 %+v", got)
	}
	if got := billing.ParseImageSize(billing.ImageDecl{AspectRatio: "16:9"}); got.Source != billing.ImageSizeSourceRatio || got.Ratio != "16:9" {
		t.Fatalf("aspect_ratio 解析不符：%+v", got)
	}
}

func TestSpecUsageOf(t *testing.T) {
	// 图片：档位由 resolveImageSize 归一后传进来，specUsageOf 只做快照。
	img := specUsageOf(kindImagesGenerations, relayHead{N: 3, Quality: "high"},
		billing.ImageSize{Tier: "1K", Source: billing.ImageSizeSourcePixels, Explicit: true}, 15, imageCaps{})
	if img.ImageSpec != "1K" || img.ImageN != 3 || img.ImageQuality != "high" {
		t.Fatalf("图片快照不符: %+v", img)
	}
	// 比例与来源必须一起进快照：它们决定适配器下发什么、以及要不要做 tier_mismatch。
	ratio := specUsageOf(kindImagesEdits, relayHead{},
		billing.ImageSize{Tier: "2K", Ratio: "16:9", Source: billing.ImageSizeSourceTier, Explicit: true}, 15, imageCaps{})
	if ratio.ImageSpec != "2K" || ratio.ImageRatio != "16:9" || !ratio.ImageSpecExplicit {
		t.Fatalf("画幅快照不符: %+v", ratio)
	}
	tts := specUsageOf(kindAudioSpeech, relayHead{Input: json.RawMessage(`"abcdef"`)}, billing.ImageSize{}, 15, imageCaps{})
	if tts.AudioChars != 6 {
		t.Fatalf("TTS 字符数不符: %+v", tts)
	}
	rr := specUsageOf(kindRerank, relayHead{Documents: json.RawMessage(`["a","b","c"]`)}, billing.ImageSize{}, 15, imageCaps{})
	if rr.RerankDocs != 3 {
		t.Fatalf("重排文档数不符: %+v", rr)
	}
}

// multipartHead 构造一段 multipart 请求体（含 model/size/n 表单值与一个文件 part）。
func multipartHead(t *testing.T) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"model": "gpt-image-1", "size": "2K", "n": "4"} {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := w.CreateFormFile("image", "a.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("PNGDATA")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

func TestParseMultipartHead(t *testing.T) {
	ctype, body := multipartHead(t)
	head, err := parseMultipartHead(ctype, body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if head.Model != "gpt-image-1" || head.Size != "2K" || head.N != 4 {
		t.Fatalf("表单字段提取不符: %+v", head)
	}
	if _, err := parseMultipartHead("multipart/form-data", body); err == nil {
		t.Fatal("缺少 boundary 应报错")
	}
}

func TestEstimateTokens(t *testing.T) {
	msgs, _ := json.Marshal([]map[string]string{{"role": "user", "content": "12345678"}})
	// CJK 感知估算：content 8 个 ASCII 字符 → ceil(8/3.5)=3 token；1 条消息 → +4 开销；共 7
	p, c := estimateTokens(relayHead{Model: "gpt-4o", Messages: msgs, MaxTokens: 500}, kindChat, 0)
	if p != 7 || c != 500 {
		t.Fatalf("估算不符: prompt=%d completion=%d", p, c)
	}
	// 请求与模型都没写输出上限 → defaultMaxOutputTokens
	if _, c := estimateTokens(relayHead{}, kindChat, 0); c != defaultMaxOutputTokens {
		t.Fatalf("缺省 completion 应为 %d, got %d", defaultMaxOutputTokens, c)
	}
	// 超上限钳制
	if _, c := estimateTokens(relayHead{MaxTokens: 99999}, kindChat, 0); c != maxCompletionCap {
		t.Fatalf("应钳制到 %d, got %d", maxCompletionCap, c)
	}
	// embeddings 取 input
	in := json.RawMessage(`"abcdefghijklmnopqrstuvwxyz"`)
	p, _ = estimateTokens(relayHead{Input: in}, kindEmbeddings, 0)
	if want := estimateTextTokens("abcdefghijklmnopqrstuvwxyz"); p != want {
		t.Fatalf("embedding 估算不符: got %d want %d", p, want)
	}
}

func TestEffectiveGroup(t *testing.T) {
	if g := effectiveGroup(&model.APIKey{Group: "vip"}, &model.User{Group: "default"}); g != "vip" {
		t.Fatalf("KEY 分组应优先, got %s", g)
	}
	if g := effectiveGroup(&model.APIKey{}, &model.User{Group: "svip"}); g != "svip" {
		t.Fatalf("应回落用户分组, got %s", g)
	}
	if g := effectiveGroup(&model.APIKey{}, &model.User{}); g != "default" {
		t.Fatalf("应回落 default, got %s", g)
	}
}

func TestDigest(t *testing.T) {
	a := digest([]byte("abc"))
	b := digest([]byte("abd"))
	if len(a) != 16 || a == b {
		t.Fatalf("摘要长度/唯一性不符: %q %q", a, b)
	}
}

// TestImageCountHint 锁定图片计费的**预冻结张数**（唯一口径）。
//
// 三条都会造成真实差额，且都不会报错：
//   - 图层拆分按 n=1 冻结、按 17 张结算 → 16 张落在冻结额之外（平台承担）；
//   - 组图请求按 n=1 冻结 → 同样穿透（缺省 max_images=15）；
//   - 参考图占满 15 的额度时不夹紧 → 冻结超过上游允许的张数，用户预扣更久。
func TestImageCountHint(t *testing.T) {
	// 上界来自模型价目（max_n），不再有写死的 15/17：图层拆分与组图缺省都取 max_n。
	cases := []struct {
		name string
		h    relayHead
		maxN int64
		want int64
	}{
		{"无参数 → 1 张", relayHead{}, 15, 1},
		{"显式 n", relayHead{N: 4}, 15, 4},
		{"图层拆分（布尔）", relayHead{LayerDecomp: json.RawMessage(`true`)}, billing.ImageLayerDecompMax, billing.ImageLayerDecompMax},
		{"图层拆分（字符串写法）", relayHead{LayerDecomp: json.RawMessage(`"true"`)}, billing.ImageLayerDecompMax, billing.ImageLayerDecompMax},
		{"图层拆分关闭", relayHead{LayerDecomp: json.RawMessage(`false`), N: 2}, 15, 2},
		{"组图缺省取 max_n", relayHead{SeqImageGen: "auto"}, 15, 15},
		{"组图显式 max_images", relayHead{SeqImageGen: "auto", SeqOptions: struct {
			MaxImages int64 `json:"max_images"`
		}{MaxImages: 4}}, 15, 4},
		{"组图夹紧参考图额度", relayHead{SeqImageGen: "auto", Image: json.RawMessage(`["a","b"]`)}, 15, 13},
		// 超上界一律饱和到 max_n：宁可按上限冻结，也不能让结算越过冻结额。
		{"组图额度超 max_n", relayHead{SeqImageGen: "auto", SeqOptions: struct {
			MaxImages int64 `json:"max_images"`
		}{MaxImages: 40}}, 15, 15},
		// 图层拆分的优先级高于组图：它是一次调用的产物形态（底图+图层）。
		{"图层拆分压过组图", relayHead{LayerDecomp: json.RawMessage(`true`), SeqImageGen: "auto", N: 1}, billing.ImageLayerDecompMax, billing.ImageLayerDecompMax},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := imageCountHint(c.h, c.maxN, imageCaps{Known: true, Group: true, Layer: true}); got != c.want {
				t.Fatalf("imageCountHint = %d，期望 %d", got, c.want)
			}
		})
	}
}

// TestDeclaredImageCount 锁死「结算上限只认用户真实下单张数」。
//
// 历史缺陷：结算侧直接传 head.N。客户端写 `n:1` + `sequential_image_generation:"auto"`
// 时，冻结按 15 张、结算却把张数收敛回 1 张 —— 上游出了 15 张，平台只收 1 张的钱，
// 且只留一个 image_n_over_delivered 标记，没有任何告警会响。
func TestDeclaredImageCount(t *testing.T) {
	seq := func(max int64) relayHead {
		h := relayHead{SeqImageGen: "auto"}
		h.SeqOptions.MaxImages = max
		return h
	}
	layered := relayHead{LayerDecomp: json.RawMessage(`"true"`)}
	cases := []struct {
		name string
		h    relayHead
		want int64
	}{
		{"没写 n（edits/variations）", relayHead{}, 0},
		{"显式 n", relayHead{N: 2}, 2},
		{"n=0 视为没写", relayHead{N: 0}, 0},
		{"组图 auto 缺省取 max_n", seq(0), billing.ImageLayerDecompMax},
		{"组图 auto 指定 max_images", seq(5), 5},
		{"组图 auto 且写了 n：以 max_images 为准", func() relayHead { h := seq(5); h.N = 10; return h }(), 5},
		{"图层拆分按 max_n", layered, billing.ImageLayerDecompMax},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := declaredImageCount(c.h, billing.ImageLayerDecompMax, true); got != c.want {
				t.Fatalf("declaredImageCount = %d，期望 %d", got, c.want)
			}
		})
	}
}

package task

import (
	"bytes"
	"encoding/json"
	"testing"
)

// normalizeSubmitBody 的 new-api 别名归一（D2）：duration→seconds、width×height→resolution、
// image→input_reference 首元素；未知字段与 metadata 必须原样保留（透传契约）。
func TestNormalizeSubmitBodyAliases(t *testing.T) {
	in := []byte(`{"model":"kling-v2-master","prompt":"x","duration":"5","width":1280,"height":720,
		"image":"https://cdn.example.com/a.png","metadata":{"negative_prompt":"模糊"},"fps":24}`)
	out, err := normalizeSubmitBody(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := m["seconds"].(float64); !ok || v != 5 {
		t.Fatalf("seconds = %v, want 5", m["seconds"])
	}
	if v, _ := m["resolution"].(string); v != "1280x720" {
		t.Fatalf("resolution = %v, want 1280x720", m["resolution"])
	}
	refs, ok := m["input_reference"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("input_reference = %v, want 1 项", m["input_reference"])
	}
	first, _ := refs[0].(map[string]any)
	if first["image_url"] != "https://cdn.example.com/a.png" || first["type"] != "image" {
		t.Fatalf("首帧参考未注入: %v", first)
	}
	if _, ok := m["metadata"].(map[string]any); !ok {
		t.Fatalf("metadata 丢失（透传契约被破坏）")
	}
	if _, ok := m["fps"]; !ok {
		t.Fatalf("fps 丢失")
	}
}

// 规范请求不得被归一改动：seconds/resolution/input_reference 原样保留。
func TestNormalizeSubmitBodyCanonicalUntouched(t *testing.T) {
	in := []byte(`{"model":"m","prompt":"p","seconds":5,"resolution":"720p",
		"input_reference":[{"type":"audio","audio_url":"https://a.mp3"}]}`)
	out, err := normalizeSubmitBody(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if v, _ := m["seconds"].(float64); v != 5 {
		t.Fatalf("seconds 被改写: %v", m["seconds"])
	}
	if v, _ := m["resolution"].(string); v != "720p" {
		t.Fatalf("resolution 被改写: %v", m["resolution"])
	}
	refs, _ := m["input_reference"].([]any)
	if len(refs) != 1 {
		t.Fatalf("input_reference 被改写: %v", m["input_reference"])
	}
	if _, ok := m["duration"]; ok {
		t.Fatalf("规范请求不应产生 duration")
	}
}

// image 与 input_reference 同时出现时，image 应插入队首（首帧优先）。
func TestNormalizeSubmitBodyImagePrepend(t *testing.T) {
	in := []byte(`{"model":"m","seconds":"5","resolution":"720p",
		"image":"https://i.png","input_reference":[{"type":"audio","audio_url":"https://a.mp3"}]}`)
	out, _ := normalizeSubmitBody(in)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	refs, _ := m["input_reference"].([]any)
	if len(refs) != 2 {
		t.Fatalf("input_reference = %d 项, want 2", len(refs))
	}
	first, _ := refs[0].(map[string]any)
	if first["image_url"] != "https://i.png" {
		t.Fatalf("image 未插到队首: %v", first)
	}
	second, _ := refs[1].(map[string]any)
	if second["audio_url"] != "https://a.mp3" {
		t.Fatalf("原参考被打乱: %v", second)
	}
}

// 厂商原生体（content[]）必须**逐字节原样**返回：一个别名都不许注入。
//
// 原生通道的语义是"除 model 外逐字节转发"。注入 seconds/resolution 等于往 Ark
// 请求体里塞它不认识的顶层字段；注入 image 更糟 —— 凭空多一份素材，
// 而平台侧不会为它计费（原生体的素材从 content 抽取）。
func TestNormalizeLeavesNativeBodyAlone(t *testing.T) {
	in := []byte(`{"model":"doubao-seedance-2-5-260628","content":[{"type":"text","text":"一只猫"},` +
		`{"type":"image_url","image_url":{"url":"https://cdn.example.com/f.png"},"role":"first_frame"}],` +
		`"duration":5,"width":1280,"height":720,"image":"https://cdn.example.com/x.png"}`)
	out, err := normalizeSubmitBody(in)
	if err != nil {
		t.Fatalf("normalize 失败：%v", err)
	}
	if string(out) != string(in) {
		t.Fatalf("原生体被改写：\n in=%s\nout=%s", in, out)
	}
}

// 归一不能把大整数/大种子改写成科学计数法（float64 只有 53 位精度）。
//
// 症状极隐蔽：用户指定了同一个 seed，却拿到不同结果 —— 因为 9007199254740993
// 在归一阶段被变成了 9007199254740992，而上游收到的就是后者。
func TestNormalizeKeepsExactNumbers(t *testing.T) {
	in := []byte(`{"model":"m","prompt":"p","duration":5,"seed":9007199254740993,"width":1280,"height":720}`)
	out, err := normalizeSubmitBody(in)
	if err != nil {
		t.Fatalf("normalize 失败：%v", err)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, _ := m["seed"].(json.Number); v.String() != "9007199254740993" {
		t.Fatalf("seed 被改写为 %v", m["seed"])
	}
	if v, _ := m["seconds"].(json.Number); v.String() != "5" {
		t.Fatalf("duration→seconds 失败：%v", m["seconds"])
	}
	if v, _ := m["resolution"].(string); v != "1280x720" {
		t.Fatalf("width×height 未归一：%v", m["resolution"])
	}
}

// ───────────────────────── 视频入口：官方 image_urls ─────────────────────────

// 官方 image_urls 必须在**解析期**折叠成 input_reference。
//
// 不折叠的两种后果都是事故级的：
//   - SSRF 校验只遍历 req.links（来自 input_reference）—— 顶层数组整条绕过；
//   - 结算读的是落库的 input_reference —— 参考图张数不落库 ⇒ 全部计 0。
func TestParseVideoSubmitFoldsImageURLs(t *testing.T) {
	body := []byte(`{"model":"muse-video","prompt":"广告","aspect_ratio":"9:16","duration":10,
		"image_urls":["https://cdn.example.com/1.png","https://cdn.example.com/2.png"]}`)
	req, err := ParseVideoSubmit(body)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(req.Params.InputRef) != 2 {
		t.Fatalf("应折叠出 2 个参考图，实际 %d：%+v", len(req.Params.InputRef), req.Params.InputRef)
	}
	for i, r := range req.Params.InputRef {
		if r.Type != "image" {
			t.Fatalf("第 %d 个 type = %q，期望 image", i, r.Type)
		}
	}
	// links 是 SSRF 校验的唯一来源：折叠没落到 links 上等于没折。
	if len(req.links) != 2 {
		t.Fatalf("SSRF 待校验链接应为 2 条，实际 %d", len(req.links))
	}
	// 出站体里不能同时留两套写法（适配器会各读一份，行为不可控）。
	var out map[string]any
	if err := json.Unmarshal(req.Body, &out); err != nil {
		t.Fatalf("归一后的 body 非法：%v", err)
	}
	if _, has := out["image_urls"]; has {
		t.Fatalf("image_urls 应被删除，实际 %s", req.Body)
	}
	// duration 是兼容别名，必须折成 seconds 并**删掉原键**（见 video_ref.go）。
	// 留着它的后果是：对未知顶层键直接拒绝的渠道会把 duration 判成未知键并 400，
	// 而文档承诺"自动映射"—— 一条只在该渠道暴露的裂缝。
	if _, has := out["duration"]; has {
		t.Fatalf("duration 应折成 seconds 并删除，实际 %s", req.Body)
	}
	if out["seconds"] != float64(10) {
		t.Fatalf("duration 未折成 seconds：%s", req.Body)
	}
	// aspect_ratio 不是别名而是规范字段，必须原样保留。
	if out["aspect_ratio"] != "9:16" {
		t.Fatalf("aspect_ratio 被改写了：%s", req.Body)
	}
}

// 两种素材写法并存 ⇒ 400：没有隐式优先级（与 content[] / input_reference 同一口径）。
func TestParseVideoSubmitRejectsMixedRefStyles(t *testing.T) {
	body := []byte(`{"model":"m","prompt":"p","image_urls":["https://cdn.example.com/1.png"],
		"input_reference":[{"type":"image","image_url":"https://cdn.example.com/2.png"}]}`)
	if _, err := ParseVideoSubmit(body); err == nil {
		t.Fatal("image_urls 与 input_reference 同时出现应 400")
	}
}

// 元素不是字符串 ⇒ 400，绝不静默跳过（跳过 = 少算参考图且用户以为传上了）。
func TestParseVideoSubmitRejectsBadImageURLsElement(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","prompt":"p","image_urls":[123]}`,
		`{"model":"m","prompt":"p","image_urls":[""]}`,
		`{"model":"m","prompt":"p","image_urls":"https://cdn.example.com/1.png"}`,
	} {
		if _, err := ParseVideoSubmit([]byte(body)); err == nil {
			t.Fatalf("非法 image_urls 应 400：%s", body)
		}
	}
}

// 空数组 = 没给参考图：清掉键，交给适配器报「需要 1–N 张」。
func TestParseVideoSubmitEmptyImageURLsDropped(t *testing.T) {
	req, err := ParseVideoSubmit([]byte(`{"model":"m","prompt":"p","image_urls":[]}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(req.Params.InputRef) != 0 {
		t.Fatalf("空数组不该造出参考图：%+v", req.Params.InputRef)
	}
	var out map[string]any
	_ = json.Unmarshal(req.Body, &out)
	if _, has := out["image_urls"]; has {
		t.Fatalf("空数组也应删键：%s", req.Body)
	}
}

// 通用入口（图像/音乐）不折叠：image_urls 在那边的语义是适配器原生字段。
func TestParseSubmitKeepsImageURLsForOtherModalities(t *testing.T) {
	req, err := ParseSubmit([]byte(`{"model":"m","prompt":"p","image_urls":["https://cdn.example.com/1.png"]}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(req.Params.InputRef) != 0 {
		t.Fatalf("通用入口不该折叠 image_urls：%+v", req.Params.InputRef)
	}
}

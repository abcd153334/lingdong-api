package task

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 素材归一的两类测试：
//
//  1. **黄金用例**（表里 legacy 那一半）：改造前就存在的 6 种写法，归一后的
//     `Refs[].Kind` 与 `Params.InputRef[].Type` 必须与改造前逐字段相同。
//     这是"新增写法不改账"的证明 —— 计费读的是 Kind，重放读的是 Type，
//     这两处一旦漂移，账单和重试都会错，而且错得看不出来。
//  2. **新写法**（role 那一半）：role 四值翻译成 type 之后，kind 必须落在
//     正确的计费类别上。特别注意 first_frame/last_frame 必须落 frame（免费），
//     落成 image 就是每单多收一张参考图的钱。

// refOf 返回素材数组里每一项的 (kind, storedType) 序列，便于整体比较。
func refOf(t *testing.T, body string) ([]string, []string) {
	t.Helper()
	req, err := ParseVideoSubmit([]byte(body))
	if err != nil {
		t.Fatalf("解析失败 %s: %v", body, err)
	}
	kinds := make([]string, 0, len(req.Refs))
	for _, r := range req.Refs {
		kinds = append(kinds, r.Kind)
	}
	types := make([]string, 0, len(req.Params.InputRef))
	for _, r := range req.Params.InputRef {
		types = append(types, r.Type)
	}
	return kinds, types
}

// TestVideoRefLegacyWritingsUnchanged 是**黄金用例**：旧写法的计费类别与落库类型一字不变。
//
// 覆盖 type 的 6 个值 + 三种链接 + 别名折叠。任何一条变了都意味着存量账目口径变了。
func TestVideoRefLegacyWritingsUnchanged(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind []string
		typ  []string
	}{
		{
			name: "type=image",
			body: `{"model":"m","prompt":"p","input_reference":[{"type":"image","image_url":"https://cdn.example.com/1.png"}]}`,
			kind: []string{"image"}, typ: []string{"image"},
		},
		{
			name: "type=audio",
			body: `{"model":"m","prompt":"p","input_reference":[{"type":"audio","audio_url":"https://cdn.example.com/1.mp3"}]}`,
			kind: []string{"audio"}, typ: []string{"audio"},
		},
		{
			name: "type=video",
			body: `{"model":"m","prompt":"p","input_reference":[{"type":"video","video_url":"https://cdn.example.com/1.mp4"}]}`,
			kind: []string{"video"}, typ: []string{"video"},
		},
		{
			// 首尾帧是 frame（免费），不是 image（按张收费）。
			name: "type=first_frame",
			body: `{"model":"m","prompt":"p","input_reference":[{"type":"first_frame","image_url":"https://cdn.example.com/f.png"}]}`,
			kind: []string{"frame"}, typ: []string{"first_frame"},
		},
		{
			name: "type=last_frame",
			body: `{"model":"m","prompt":"p","input_reference":[{"type":"last_frame","image_url":"https://cdn.example.com/l.png"}]}`,
			kind: []string{"frame"}, typ: []string{"last_frame"},
		},
		{
			// 连字符写法是文档里的历史写法，必须继续认。
			name: "type=first-frame",
			body: `{"model":"m","prompt":"p","input_reference":[{"type":"first-frame","image_url":"https://cdn.example.com/f.png"}]}`,
			kind: []string{"frame"}, typ: []string{"first_frame"},
		},
		{
			// base_video 的 kind 是 video（按条/按秒计费），但落库类型必须保住
			// base_video —— videoRefURLs 只探 type=="video"，落成 video 会对
			// 再生成的底片再收一次参考视频秒费。
			name: "type=base_video",
			body: `{"model":"m","prompt":"p","input_reference":[{"type":"base_video","video_url":"https://cdn.example.com/b.mp4"}]}`,
			kind: []string{"video"}, typ: []string{"base_video"},
		},
		{
			// 不写 type 时按链接推断（refKind 的兜底），行为不变。
			name: "无 type 只有 image_url",
			body: `{"model":"m","prompt":"p","input_reference":[{"image_url":"https://cdn.example.com/1.png"}]}`,
			kind: []string{"image"}, typ: []string{"image"},
		},
		{
			// 厂商原生 role 原样落库（重放需要它）。
			name: "厂商 role=reference_image",
			body: `{"model":"m","prompt":"p","input_reference":[{"type":"reference_image","image_url":"https://cdn.example.com/1.png"}]}`,
			kind: []string{"image"}, typ: []string{"reference_image"},
		},
		{
			// image 别名折叠到队首，且是 image（收费）不是首帧。
			name: "image 别名",
			body: `{"model":"m","prompt":"p","image":"https://cdn.example.com/x.png"}`,
			kind: []string{"image"}, typ: []string{"image"},
		},
		{
			// first_frame 别名是 frame（免费）。
			name: "first_frame 别名",
			body: `{"model":"m","prompt":"p","first_frame":"https://cdn.example.com/f.png"}`,
			kind: []string{"frame"}, typ: []string{"first_frame"},
		},
		{
			name: "首尾帧别名同时给",
			body: `{"model":"m","prompt":"p","first_frame":"https://cdn.example.com/f.png","last_frame":"https://cdn.example.com/l.png"}`,
			kind: []string{"frame", "frame"}, typ: []string{"first_frame", "last_frame"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kinds, types := refOf(t, c.body)
			if strings.Join(kinds, ",") != strings.Join(c.kind, ",") {
				t.Fatalf("计费类别 %v，期望 %v（改了这里就是改了存量账目）", kinds, c.kind)
			}
			if strings.Join(types, ",") != strings.Join(c.typ, ",") {
				t.Fatalf("落库类型 %v，期望 %v（重放会用这个值）", types, c.typ)
			}
		})
	}
}

// TestVideoRefRejectsUnknownType 守住「未知 type 不再静默当图片计费」。
func TestVideoRefRejectsUnknownType(t *testing.T) {
	for _, typ := range []string{"imag", "image_url", "reference", "IMAGE2"} {
		body := `{"model":"m","prompt":"p","input_reference":[{"type":"` + typ + `","image_url":"https://cdn.example.com/1.png"}]}`
		_, err := ParseVideoSubmit([]byte(body))
		if !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("type=%q 应报参数错误，实际 %v（拼错一个字母就多收钱，必须拦住）", typ, err)
		}
	}
}

// TestVideoRefSingleLinkOnly 守住「一项素材只能填一个地址」。
func TestVideoRefSingleLinkOnly(t *testing.T) {
	cases := []string{
		`{"image_url":"https://cdn.example.com/1.png","audio_url":"https://cdn.example.com/1.mp3"}`,
		`{"type":"image","image_url":"https://cdn.example.com/1.png","audio_url":"https://cdn.example.com/1.mp3"}`,
		`{"type":"image"}`,
	}
	for _, ref := range cases {
		body := `{"model":"m","prompt":"p","input_reference":[` + ref + `]}`
		if _, err := ParseVideoSubmit([]byte(body)); !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("%s 应报错，实际 %v", ref, err)
		}
	}
}

// TestVideoRefAtMostOneBase 守住「底片至多一个」。
//
// 旧代码只取第一个，其余静默丢弃 —— 用户以为换了一条视频。
func TestVideoRefAtMostOneBase(t *testing.T) {
	body := `{"model":"m","prompt":"p","input_reference":[` +
		`{"type":"base_video","video_url":"https://cdn.example.com/1.mp4"},` +
		`{"type":"base_video","video_url":"https://cdn.example.com/2.mp4"}]}`
	if _, err := ParseVideoSubmit([]byte(body)); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("两个底片应报错，实际 %v", err)
	}
}

// TestVideoAliasesAreDeletedFromBody 守住「别名映射后删原键」。
//
// 不删的表现：AutoDL 的未知顶层键拒绝会把 duration/size/width/height/image
// 判成未知键并 400，而文档承诺它们是"自动映射"的兼容写法 ——
// 一条只在该渠道暴露的裂缝。
func TestVideoAliasesAreDeletedFromBody(t *testing.T) {
	req, err := ParseVideoSubmit([]byte(
		`{"model":"m","prompt":"p","duration":6,"width":1280,"height":720,"image":"https://cdn.example.com/1.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"duration", "width", "height", "size", "image", "first_frame", "last_frame"} {
		if _, ok := body[k]; ok {
			t.Fatalf("别名 %q 仍留在出站请求体里：%s", k, req.Body)
		}
	}
	if body["seconds"] == nil {
		t.Fatalf("duration 没有折成 seconds：%s", req.Body)
	}
	if body["resolution"] != "1280x720" {
		t.Fatalf("width×height 没有折成 resolution：%s", req.Body)
	}
}

// TestVideoNativeContentUntouched 守住「原生 content[] 体一个字节都不改」。
func TestVideoNativeContentUntouched(t *testing.T) {
	body := `{"model":"m","content":[{"type":"text","text":"hi"},{"type":"image_url",` +
		`"image_url":{"url":"https://cdn.example.com/1.png"},"role":"reference_image"}]}`
	req, err := ParseVideoSubmit([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(req.Body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["input_reference"]; ok {
		t.Fatalf("原生体被注入了 input_reference：%s", req.Body)
	}
	if _, ok := got["seconds"]; ok {
		t.Fatalf("原生体被注入了 seconds：%s", req.Body)
	}
}

// TestVideoRefTypeLinkConsistency 守住「type 与链接键必须自洽」。
//
// 错配的后果是"按图片计费、给上游发音频链接"：两边都说不通，且账单上只看到
// 一张参考图。改造前这条路只在带 role 时被拦，不带 role 时就漏过去了。
func TestVideoRefTypeLinkConsistency(t *testing.T) {
	cases := []string{
		`{"type":"image","audio_url":"https://cdn.example.com/1.mp3"}`,
		`{"type":"audio","image_url":"https://cdn.example.com/1.png"}`,
		`{"type":"base_video","image_url":"https://cdn.example.com/1.png"}`,
	}
	for _, ref := range cases {
		body := `{"model":"m","prompt":"p","input_reference":[` + ref + `]}`
		if _, err := ParseVideoSubmit([]byte(body)); !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("%s 应报 type/链接错配，实际 %v", ref, err)
		}
	}
}

// TestVideoRefRoleIsRejected 守住「role 已废弃」。
//
// 不能静默忽略：`role=first_frame` 一旦退化成"按链接推断=image"，就会把免费的
// 首帧按参考图收费，而账单上看不出原因。所以宁可 400 并指路。
func TestVideoRefRoleIsRejected(t *testing.T) {
	body := `{"model":"m","prompt":"p","input_reference":[` +
		`{"role":"first_frame","image_url":"https://cdn.example.com/f.png"}]}`
	_, err := ParseVideoSubmit([]byte(body))
	if !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("role 应被拒绝，实际 %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "first_frame") {
		t.Fatalf("错误文案应指路顶层字段：%v", err)
	}
}

// TestVideoMaterialFieldsFold 覆盖 5 个顶层素材字段的折叠结果。
func TestVideoMaterialFieldsFold(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind []string
		typ  []string
	}{
		{
			name: "image 字符串",
			body: `{"model":"m","prompt":"p","image":"https://cdn.example.com/a.png"}`,
			kind: []string{"image"}, typ: []string{"image"},
		},
		{
			name: "image 数组保持顺序",
			body: `{"model":"m","prompt":"p","image":["https://cdn.example.com/a.png","https://cdn.example.com/b.png"]}`,
			kind: []string{"image", "image"}, typ: []string{"image", "image"},
		},
		{
			name: "audio 数组",
			body: `{"model":"m","prompt":"p","image":["https://cdn.example.com/a.png"],"audio":["https://cdn.example.com/1.mp3","https://cdn.example.com/2.mp3"]}`,
			kind: []string{"image", "audio", "audio"}, typ: []string{"image", "audio", "audio"},
		},
		{
			name: "video 数组",
			body: `{"model":"m","prompt":"p","video":["https://cdn.example.com/1.mp4"]}`,
			kind: []string{"video"}, typ: []string{"video"},
		},
		{
			name: "首尾帧 + 参考图",
			body: `{"model":"m","prompt":"p","first_frame":"https://cdn.example.com/f.png",` +
				`"last_frame":"https://cdn.example.com/l.png","image":"https://cdn.example.com/r.png"}`,
			kind: []string{"image", "frame", "frame"}, typ: []string{"image", "first_frame", "last_frame"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kinds, types := refOf(t, c.body)
			if strings.Join(kinds, ",") != strings.Join(c.kind, ",") {
				t.Fatalf("计费类别 %v，期望 %v", kinds, c.kind)
			}
			if strings.Join(types, ",") != strings.Join(c.typ, ",") {
				t.Fatalf("落库类型 %v，期望 %v", types, c.typ)
			}
		})
	}
}

// TestVideoMaterialFieldsFoldRejectsBadInput 守住折叠的输入校验。
func TestVideoMaterialFieldsFoldRejectsBadInput(t *testing.T) {
	cases := []struct{ name, body string }{
		{"数组里混了数字", `{"model":"m","prompt":"p","image":["https://cdn.example.com/a.png",3]}`},
		{"数组里有空串", `{"model":"m","prompt":"p","audio":[""]}`},
		{"给了对象", `{"model":"m","prompt":"p","video":{"url":"https://cdn.example.com/1.mp4"}}`},
		{"image_urls 与 image 并存", `{"model":"m","prompt":"p","image_urls":["https://cdn.example.com/a.png"],"image":"https://cdn.example.com/b.png"}`},
		{"首帧给了两个不同链接", `{"model":"m","prompt":"p","first_frame":"https://cdn.example.com/f.png",` +
			`"input_reference":[{"type":"first_frame","image_url":"https://cdn.example.com/g.png"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseVideoSubmit([]byte(c.body)); !errors.Is(err, ErrInvalidParam) {
				t.Fatalf("%s 应报参数错误，实际 %v", c.name, err)
			}
		})
	}
}

// TestVideoMaterialFieldsDedup 守住幂等：顶层字段与 input_reference 同款同链接不重复计费。
func TestVideoMaterialFieldsDedup(t *testing.T) {
	body := `{"model":"m","prompt":"p","first_frame":"https://cdn.example.com/f.png",` +
		`"input_reference":[{"type":"first_frame","image_url":"https://cdn.example.com/f.png"}]}`
	kinds, types := refOf(t, body)
	if len(kinds) != 1 || kinds[0] != "frame" || len(types) != 1 || types[0] != "first_frame" {
		t.Fatalf("同款首帧被记了两次：kind=%v type=%v", kinds, types)
	}
}

// TestVideoRefFirstFrameAlone 守住「单首帧合法」。
//
// 秘塔允许只给首帧（AutoDL 会明确 400 要求成对）。网关不拦：拦了会误杀
// 秘塔这条合法能力，而 AutoDL 的报错已经足够清楚。
func TestVideoRefFirstFrameAlone(t *testing.T) {
	kinds, types := refOf(t, `{"model":"m","prompt":"p","first_frame":"https://cdn.example.com/f.png"}`)
	if len(kinds) != 1 || kinds[0] != "frame" || types[0] != "first_frame" {
		t.Fatalf("单首帧应落 frame/first_frame，实际 %v/%v", kinds, types)
	}
}

// TestVideoAliasSizeFoldsToResolution 守住 `size` 的折算。
//
// 真机 D6 抓到的洞：归一函数把 `size` 删了却没写回 `resolution`，
// 于是 `size=480p` 到了适配器手里 resolution 是空的 → 400「不支持的分辨率「」」。
// 而文档承诺 `size` 是兼容写法「平台会自动改写」—— 删键不等于改写。
func TestVideoAliasSizeFoldsToResolution(t *testing.T) {
	req, err := ParseVideoSubmit([]byte(`{"model":"m","prompt":"p","duration":5,"size":"480p"}`))
	if err != nil {
		t.Fatalf("size 别名应被接受：%v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["resolution"] != "480p" {
		t.Fatalf("size 没有折成 resolution：%s", req.Body)
	}
	if _, ok := body["size"]; ok {
		t.Fatalf("size 应删键（否则未知键闸门会 400）：%s", req.Body)
	}
	if body["seconds"] != float64(5) {
		t.Fatalf("duration 没有折成 seconds：%s", req.Body)
	}
}

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
)

// 本文件覆盖 B6-2 三个新模态端点（图像异步 / 音乐 / 歌词）的**入口守卫**。
//
// 这些守卫必须发生在 task.Submit 之前：任何一条漏掉，都会先冻结额度再被上游拒绝，
// 用户看到的是 502/500 而不是「你的参数不对」，而平台要多一次退款与一次渠道误熔断。
// 因此以下用例全部在**无数据库**环境下运行——能跑通本身就证明守卫前置。

// callMediaHandler 构造一个带 KEY/用户上下文的 POST 调用。
func callMediaHandler(t *testing.T, handler gin.HandlerFunc, body, ctype string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/x", func(c *gin.Context) {
		c.Set("api_key_record", &model.APIKey{ID: 1, UserID: 1})
		c.Set("user_record", &model.User{ID: 1})
		handler(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// callImageTask 直接调用「落任务」函数，跳过 ImagesCreate 的分流。
//
// 这些用例只想验证**落任务之前**的守卫，不需要经过 async 判定；分流本身的
// 行为由 TestParseImageMode / TestStripAsyncKey 覆盖。
func callImageTask(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return callMediaHandler(t, func(c *gin.Context) { imageSubmitTask(c, []byte(body)) }, body, "application/json")
}

// decodeOpenAIError 取出 OpenAI 形状错误体里的 code 与 message。
func decodeOpenAIError(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var b struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("错误体不是 JSON：%v（%s）", err, w.Body.String())
	}
	return b.Error.Code, b.Error.Message
}

func TestImageAsyncRejectsB64(t *testing.T) {
	w := callImageTask(t, `{"model":"gpt-image-2","prompt":"a","response_format":"b64_json"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("b64_json 应 400，实际 %d", w.Code)
	}
	if code, _ := decodeOpenAIError(t, w); code != "b64_not_supported_async" {
		t.Fatalf("错误码应为 b64_not_supported_async，实际 %q", code)
	}
}

func TestImageAsyncRejectsInlineImage(t *testing.T) {
	cases := []string{
		`{"model":"gpt-image-2","prompt":"a","image":"data:image/png;base64,AAA"}`,
		`{"model":"gpt-image-2","prompt":"a","image":["https://cdn/a.png","data:image/png;base64,AAA"]}`,
		`{"model":"gpt-image-2","prompt":"a","mask_url":"data:image/png;base64,AAA"}`,
	}
	for _, body := range cases {
		w := callImageTask(t, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("data: 内联应 400，实际 %d（%s）", w.Code, body)
		}
		if code, _ := decodeOpenAIError(t, w); code != "inline_image_not_supported" {
			t.Fatalf("错误码应为 inline_image_not_supported，实际 %q（%s）", code, body)
		}
	}
}

// n 不再本地判定上限：支持几张由上游决定，平台按上游实际交付的张数结算。
//
// 判据取「同一请求里另一个确定的拒绝原因」：带 data: 内联的 image 必然 400
// inline_image_not_supported。若 n 仍被本地拦，会先撞上 n_not_supported。
func TestImageAsyncDoesNotRejectNLocally(t *testing.T) {
	body := `{"model":"gpt-image-2","prompt":"a","n":3,"image":"data:image/png;base64,AAA"}`
	w := callImageTask(t, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("data: 内联应 400，实际 %d", w.Code)
	}
	if code, msg := decodeOpenAIError(t, w); code != "inline_image_not_supported" {
		t.Fatalf("n>1 不该再被本地拒绝，期望先撞到 inline_image_not_supported，实际 %q（%s）", code, msg)
	}
}

func TestImageAsyncRejectsOversizeBody(t *testing.T) {
	big := `{"model":"gpt-image-2","prompt":"` + strings.Repeat("a", maxTaskBodyBytes+16) + `"}`
	w := callImageTask(t, big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超 1MiB 应 413，实际 %d", w.Code)
	}
}

func TestMusicRejectsInstrumentalWithLyrics(t *testing.T) {
	w := callMediaHandler(t, MusicCreate,
		`{"model":"chirp-v4","prompt":"jazz","lyrics":"歌词","instrumental":true}`, "application/json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("互斥参数应 400，实际 %d", w.Code)
	}
	if code, _ := decodeOpenAIError(t, w); code != "instrumental_lyrics_conflict" {
		t.Fatalf("错误码应为 instrumental_lyrics_conflict，实际 %q", code)
	}
}

func TestMusicRequiresPromptOrLyrics(t *testing.T) {
	w := callMediaHandler(t, MusicCreate, `{"model":"chirp-v4"}`, "application/json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 prompt 与 lyrics 应 400，实际 %d", w.Code)
	}
}

func TestLyricsRejectsOversizePrompt(t *testing.T) {
	// 2001 个汉字（字节数远超上限，但判定必须按字符数）。
	long := strings.Repeat("秋", maxLyricsPromptRunes+1)
	w := callMediaHandler(t, LyricsCreate, `{"model":"suno-lyrics","prompt":"`+long+`"}`, "application/json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超长 prompt 应 400，实际 %d", w.Code)
	}
	if code, _ := decodeOpenAIError(t, w); code != "prompt_too_long" {
		t.Fatalf("错误码应为 prompt_too_long，实际 %q", code)
	}
}

func TestLyricsRequiresPrompt(t *testing.T) {
	w := callMediaHandler(t, LyricsCreate, `{"model":"suno-lyrics"}`, "application/json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 prompt 应 400，实际 %d", w.Code)
	}
}

func TestMediaEndpointsRequireAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, h := range []gin.HandlerFunc{ImagesCreate, MusicCreate, LyricsCreate} {
		r := gin.New()
		r.POST("/x", h) // 不注入 api_key_record / user_record
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"model":"m","prompt":"a"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("未鉴权应 401，实际 %d", w.Code)
		}
	}
}

// TestLooseIntAndInlineHelpers 覆盖两个小工具的三态行为（守卫的基础设施）。
func TestLooseIntAndInlineHelpers(t *testing.T) {
	if n, ok := looseInt(nil); !ok || n != 0 {
		t.Fatalf("缺失应返回 0/true，实际 %d/%v", n, ok)
	}
	if n, ok := looseInt(json.RawMessage(`null`)); !ok || n != 0 {
		t.Fatalf("null 应返回 0/true，实际 %d/%v", n, ok)
	}
	if n, ok := looseInt(json.RawMessage(`"2"`)); !ok || n != 2 {
		t.Fatalf("数字串应解析为 2，实际 %d/%v", n, ok)
	}
	if _, ok := looseInt(json.RawMessage(`"abc"`)); ok {
		t.Fatal("非数字应返回 ok=false")
	}
	if duomiHasInlineImage(json.RawMessage(`"https://cdn/a.png"`)) {
		t.Fatal("公网链接不应判为内联")
	}
	if !duomiHasInlineImage(json.RawMessage(`["https://a","data:x"]`)) {
		t.Fatal("数组里的 data: 必须被识别")
	}
}

// TestParseImageMode 守「同路径两分支」的分流判据。
//
// async 只认 JSON 布尔：宽松转换（把非空串当真）会让 `"false"` 也被判成 true ——
// 客户端多写一对引号就从「拿到图片」变成「拿到任务号」，而且不报错。
// 报文本身不是 JSON 对象时必须返回 valid=true：结构错误交给 relay 统一报，
// 否则同一件事会出现两种错误文案。
func TestParseImageMode(t *testing.T) {
	cases := []struct {
		name                       string
		body                       string
		async, present, stream, ok bool
	}{
		{"无 async", `{"model":"m"}`, false, false, false, true},
		{"async=false", `{"model":"m","async":false}`, false, true, false, true},
		{"async=true", `{"model":"m","async":true}`, true, true, false, true},
		{"async=null", `{"model":"m","async":null}`, false, true, false, true},
		{"async 字符串", `{"model":"m","async":"true"}`, false, true, false, false},
		{"async 数字", `{"model":"m","async":1}`, false, true, false, false},
		{"stream=true", `{"model":"m","stream":true}`, false, false, true, true},
		{"报文非对象", `[1,2]`, false, false, false, true},
		{"空体", ``, false, false, false, true},
	}
	for _, c := range cases {
		m := parseImageMode([]byte(c.body))
		if m.async != c.async || m.present != c.present || m.stream != c.stream || m.valid != c.ok {
			t.Fatalf("%s：得到 async=%v present=%v stream=%v valid=%v，期望 %v/%v/%v/%v",
				c.name, m.async, m.present, m.stream, m.valid, c.async, c.present, c.stream, c.ok)
		}
	}
}

// TestStripAsyncKey 守「async 不能透传上游」。
//
// ForwardUnmapped 会把未映射键原样发给厂商，厂商对未知字段普遍直接 400；
// 同时不能把 prompt 里的尖括号转义掉（落库报文与用户原文不一致会误导排障）。
func TestStripAsyncKey(t *testing.T) {
	out := stripAsyncKey([]byte(`{"model":"m","async":true,"prompt":"a<b&c"}`))
	var got struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Async  any    `json:"async"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("剥离后不是合法 JSON：%v（%s）", err, out)
	}
	if got.Async != nil {
		t.Fatalf("async 键必须被删除，实际 %v", got.Async)
	}
	if got.Prompt != "a<b&c" {
		t.Fatalf("prompt 被改写：%q", got.Prompt)
	}
	if strings.Contains(string(out), "\\u003c") {
		t.Fatalf("不应转义尖括号：%s", out)
	}
}

// TestImagesCreateRejectsStringAsync 守入口级的 400：`"true"` 不是布尔。
func TestImagesCreateRejectsStringAsync(t *testing.T) {
	w := callMediaHandler(t, ImagesCreate, `{"model":"m","prompt":"a","async":"true"}`, "application/json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("字符串 async 应 400，实际 %d", w.Code)
	}
	if code, _ := decodeOpenAIError(t, w); code != "invalid_param" {
		t.Fatalf("错误码应为 invalid_param，实际 %q", code)
	}
}

// TestImagesCreateRejectsAsyncWithStream 守互斥：任务模式不返回逐帧结果。
func TestImagesCreateRejectsAsyncWithStream(t *testing.T) {
	w := callMediaHandler(t, ImagesCreate, `{"model":"m","prompt":"a","async":true,"stream":true}`, "application/json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("async+stream 应 400，实际 %d", w.Code)
	}
	if code, _ := decodeOpenAIError(t, w); code != "async_stream_conflict" {
		t.Fatalf("错误码应为 async_stream_conflict，实际 %q", code)
	}
}

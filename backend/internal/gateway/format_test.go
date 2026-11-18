package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/wireformat"
)

// 本文件原在 pkg/response（协议面与控制面共用一个包时）。协议面收口到 gateway 后
// 整体搬过来：形状断言必须跟着**写出口**走，否则改了实现而测试还在测死的旧包。

// exec 跑一次 handler 并返回响应体。
func exec(t *testing.T, format wireformat.Format, status int, code, message string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if format != "" {
		SetFormat(c, format)
	}
	Fail(c, status, code, message)
	return w, w.Body.String()
}

// TestErrorBodyShapes 逐档校验两种线格式的错误体。
//
// 形状错了客户端会显示"未知错误"：OpenAI SDK 找 error.message，
// Anthropic SDK 找 error.type + error.message，Gemini 找 error.code。
func TestErrorBodyShapes(t *testing.T) {
	statuses := []int{400, 401, 402, 413, 429, 502, 503}
	for _, st := range statuses {
		// OpenAI 形状：三个字段都在，code 是内部业务码。
		_, body := exec(t, wireformat.OpenAI, st, "some_code", "出错了")
		var oa struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &oa); err != nil {
			t.Fatalf("%d: OpenAI 体不是合法 JSON: %s", st, body)
		}
		if oa.Error.Message != "出错了" || oa.Error.Code != "some_code" || oa.Error.Type == "" {
			t.Fatalf("%d: OpenAI 体字段不全: %s", st, body)
		}

		// Claude 形状：顶层 type=error，error.type 用 Anthropic 词表，且没有 code 字段。
		_, cbody := exec(t, wireformat.Claude, st, "some_code", "出错了")
		var cl struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(cbody), &cl); err != nil {
			t.Fatalf("%d: Claude 体不是合法 JSON: %s", st, cbody)
		}
		if cl.Type != "error" {
			t.Fatalf("%d: 顶层 type 应为 error，实际 %q", st, cl.Type)
		}
		if cl.Error.Message != "出错了" {
			t.Fatalf("%d: Claude 体 message 不符: %s", st, cbody)
		}
		if cl.Error.Type == "" {
			t.Fatalf("%d: Claude 体缺 error.type", st)
		}
	}
}

// TestOpenAIErrorBodyShape 锁死 OpenAI 错误体的**键与顺序**。
//
// 加 param 是一次有意的形状变更（OpenAI 与 new-api 的客户端都读这个键，
// 缺字段与空串在部分 SDK 里是两种分支），因此这条断言从"与改造前逐字节一致"
// 改成"形状就是这五个键、顺序固定"。
func TestOpenAIErrorBodyShape(t *testing.T) {
	_, body := exec(t, wireformat.OpenAI, 400, "invalid_param", "参数不合法")
	want := `{"error":{"message":"参数不合法","type":"invalid_request_error","param":"","code":"invalid_param"}}`
	if body != want {
		t.Fatalf("OpenAI 错误体形状不符:\n got=%s\nwant=%s", body, want)
	}
}

// TestNewAPIErrorBodyShape 锁死 new-api 封套：code 是字符串、data 恒为 null。
func TestNewAPIErrorBodyShape(t *testing.T) {
	_, body := exec(t, wireformat.NewAPI, 404, "not_found", "任务不存在")
	want := `{"code":"not_found","message":"任务不存在","data":null}`
	if body != want {
		t.Fatalf("new-api 错误体形状不符:\n got=%s\nwant=%s", body, want)
	}
}

// execAt 在指定路径上跑一次失败写出（用于路径相关的码表投影）。
func execAt(t *testing.T, path string, format wireformat.Format, status int, code, message string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, path, nil)
	if format != "" {
		SetFormat(c, format)
	}
	Fail(c, status, code, message)
	return w.Body.String()
}

// TestSunoWireTable 守住 `/suno/*` 的码表投影（唯一实现）：状态码与码一起收敛。
//
// 中间件层的失败不走 controller：无效 Key、限速、跨域都在中间件里直接写出，
// 带的是内部码。按 SUNO 协议写的客户端只认七个码，其余一律当"未知错误"，
// 于是「Key 错了」会显示成「请重试」—— 真机踩到过。
//
// 两处投影的边界曾经不同：controller 那份把 413/405/422 压成 400，
// gateway 那份漏了它们，同一个错误会因「谁先发现」而得到不同状态码。
func TestSunoWireTable(t *testing.T) {
	cases := []struct {
		status     int
		wantStatus int
		want       string
	}{
		{401, 401, "authentication_error"},
		{403, 403, "permission_denied"},
		{402, 403, "permission_denied"}, // 额度不足与没权限在 SUNO 共用一个码
		{404, 404, "task_not_found"},
		{409, 409, "request_conflict"},
		{413, 413, "invalid_request"}, // 保留 413：与 400 是两种改法
		{422, 422, "invalid_request"},
		{405, 405, "invalid_request"},
		{429, 429, "rate_limit_exceeded"},
		{502, 502, "server_error"}, // 5xx 保留原状态：503 与 500 是两种动作
		{503, 503, "server_error"},
	}
	for _, c := range cases {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/suno/fetch/task_x", nil)
		SetFormat(ctx, wireformat.NewAPI)
		Fail(ctx, c.status, "internal_code", "x")

		if w.Code != c.wantStatus {
			t.Fatalf("status %d 应投影成 %d，实际 %d（body=%s）", c.status, c.wantStatus, w.Code, w.Body.String())
		}
		if body := w.Body.String(); !strings.Contains(body, `"code":"`+c.want+`"`) {
			t.Fatalf("status %d 应投影成码 %q，实际 %s", c.status, c.want, body)
		}
	}
	// 同一状态码在 /v1 上必须保持内部码：视频兼容面用的是 new-api 自己的词表。
	body := execAt(t, "/v1/video/generations/task_x", wireformat.NewAPI, 401, "invalid_api_key", "x")
	if !strings.Contains(body, `"code":"invalid_api_key"`) {
		t.Fatalf("/v1 不该被 SUNO 码表改写，实际 %s", body)
	}
}

// TestClaudeErrorTypeMapping 校验 HTTP → Anthropic error.type 的单表映射。
func TestClaudeErrorTypeMapping(t *testing.T) {
	cases := map[int]string{
		400: "invalid_request_error", 401: "authentication_error", 403: "permission_error",
		404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error",
		500: "api_error", 502: "api_error", 503: "overloaded_error", 504: "overloaded_error",
		402: "api_error",
	}
	for st, want := range cases {
		if got := claudeErrorType(st, ""); got != want {
			t.Fatalf("%d → %q，期望 %q", st, got, want)
		}
	}
}

// TestErrorBodyIsTracedForCallIO 保证留痕通道拿到的是**实际写出**的字节。
//
// 门户详情里的「响应内容」在失败调用上就是这一段：留痕缺失等于告诉用户
// 「你没有收到任何返回」，而客户端明明收到了 JSON。
func TestErrorBodyIsTracedForCallIO(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	FailDef(c, errs.InvalidParam)

	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type 与改造前不一致：%q", ct)
	}
	v, ok := c.Get(ClientErrorBodyKey)
	if !ok {
		t.Fatal("错误体没有留痕：失败调用的详情里将看不到客户端收到的内容")
	}
	traced, _ := v.([]byte)
	if string(traced) != w.Body.String() {
		t.Fatalf("留痕字节与写出字节不一致:\n trace=%s\n wrote=%s", traced, w.Body.String())
	}
}

// TestDefaultFormatIsOpenAI 未设置格式时必须回落 OpenAI（既有端点零行为变化）。
func TestDefaultFormatIsOpenAI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if got := FormatOf(c); got != wireformat.OpenAI {
		t.Fatalf("默认格式应为 openai，实际 %q", got)
	}
}

// TestNoRetryHeaderOnAmbiguous 守「可能已计费的失败绝不能被自动重试」。
//
// 504 与 502 都在 OpenAI/Anthropic SDK 的默认重试白名单里：上游已经受理并计费，
// 客户端自动重试会再提交一次、再计一次费，用户拿到两份产物。
// 409 + X-Should-Retry: false 是双保险（后者是两家官方 SDK 都认的约定）。
func TestNoRetryHeaderOnAmbiguous(t *testing.T) {
	cases := []struct {
		def        errs.Def
		wantStatus int
		wantHeader bool
	}{
		{errs.UpstreamResultUnknown, 409, true},
		{errs.UpstreamSubmitAmbiguous, 502, true},
		// 纯超时（未提交成功）仍然可以重试：不该被这条规则波及。
		{errs.UpstreamTimeout, 504, false},
	}
	for _, c := range cases {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		SetFormat(ctx, wireformat.OpenAI)

		FailDef(ctx, c.def)

		if w.Code != c.wantStatus {
			t.Fatalf("%s 应为 %d，实际 %d", c.def.Code, c.wantStatus, w.Code)
		}
		got := w.Header().Get("X-Should-Retry")
		if c.wantHeader && got != "false" {
			t.Fatalf("%s 必须下发 X-Should-Retry: false，实际 %q", c.def.Code, got)
		}
		if !c.wantHeader && got != "" {
			t.Fatalf("%s 不应带 X-Should-Retry，实际 %q", c.def.Code, got)
		}
	}
}

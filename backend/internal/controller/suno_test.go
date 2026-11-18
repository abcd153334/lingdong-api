package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/wireformat"
)

// SUNO 兼容面的**协议翻译**测试。
//
// 这一层没有库里可断言的东西：它只把 new-api 的报文改写成平台契约。所以测试
// 全部落在纯函数上 —— 一旦有人「顺手简化」normalizeSunoBody，这里先红。

func TestSunoActionOf(t *testing.T) {
	cases := []struct {
		raw, want string
		ok        bool
	}{
		{"music", "music", true},
		{"MUSIC", "music", true},
		{" Lyrics ", "lyrics", true},
		// 未知动作必须拒：猜一个默认动作就等于「写词请求被当作曲收钱」。
		{"sing", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := SunoActionOf(c.raw)
		if got != c.want || ok != c.ok {
			t.Fatalf("SunoActionOf(%q) = (%q,%v)，期望 (%q,%v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

func TestNormalizeSunoBodyTranslations(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]any
	}{
		{
			name: "gpt_description_prompt 落到 prompt",
			in:   `{"gpt_description_prompt":"轻快的民谣"}`,
			want: map[string]any{"prompt": "轻快的民谣"},
		},
		{
			name: "prompt 与它同时给时以 prompt 为准",
			in:   `{"prompt":"A","gpt_description_prompt":"B"}`,
			want: map[string]any{"prompt": "A"},
		},
		{
			name: "自定义模式把 prompt 搬进 lyrics",
			in:   `{"custom_mode":true,"prompt":"[Verse]\n路灯"}`,
			want: map[string]any{"custom_mode": true, "lyrics": "[Verse]\n路灯"},
		},
		{
			name: "已经有 lyrics 时不被 prompt 覆盖",
			in:   `{"custom_mode":true,"prompt":"风格","lyrics":"[Verse]\n原词"}`,
			want: map[string]any{"lyrics": "[Verse]\n原词"},
		},
		{
			name: "make_instrumental 缺省补 false",
			in:   `{"prompt":"钢琴独奏"}`,
			want: map[string]any{"make_instrumental": false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := normalizeSunoBody([]byte(c.in))
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			m, derr := decodeJSONMap(out)
			if derr != nil {
				t.Fatalf("输出不是合法 JSON: %v", derr)
			}
			for k, want := range c.want {
				if got := m[k]; got != want {
					t.Fatalf("%s = %#v，期望 %#v（完整输出 %s）", k, got, want, out)
				}
			}
		})
	}
}

// 显式拒绝的四个字段：静默接受会让用户拿着「我明明设了回调」来对账。
func TestNormalizeSunoBodyRejectsUnsupported(t *testing.T) {
	for _, f := range unsupportedSunoFields {
		body := `{"prompt":"x","` + f.Key + `":"y"}`
		_, err := normalizeSunoBody([]byte(body))
		if err == nil {
			t.Fatalf("%s 必须被拒绝", f.Key)
		}
		if !strings.Contains(err.Error(), f.Key) {
			t.Fatalf("%s 的报错要带上字段名，实际 %q", f.Key, err.Error())
		}
	}
}

func TestNormalizeSunoBodyKeepsNumbers(t *testing.T) {
	// 大整数若走了 float64，会在这里被改写成 1.2345678901234568e+18。
	out, err := normalizeSunoBody([]byte(`{"prompt":"x","seed":1234567890123456789}`))
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if !strings.Contains(string(out), "1234567890123456789") {
		t.Fatalf("整数被改写了：%s", out)
	}
}

func TestIsSunoPlaceholderModel(t *testing.T) {
	for _, s := range []string{"suno_music", "SUNO-LYRICS", " suno_lyrics "} {
		if !isSunoPlaceholderModel(s) {
			t.Fatalf("%q 应识别为占位模型名", s)
		}
	}
	// 真实模型名不能被当成占位名：那会把用户指定的模型悄悄换成默认模型。
	for _, s := range []string{"chirp-hawk", "suno-lyrics-v4", ""} {
		if isSunoPlaceholderModel(s) {
			t.Fatalf("%q 不是占位名", s)
		}
	}
}

// 文档写的 action 取值必须与控制器认的一致：文档说 music/lyrics 而控制器只认大写，
// 就是「照文档发请求 400」的经典来源。
func TestSunoDocActionMatchesController(t *testing.T) {
	var doc *apidoc.EndpointDoc
	for _, ent := range apidoc.AllEndpointDocs() {
		if ent.Doc.Kind == apidoc.KindSunoSubmit {
			d := ent.Spec
			doc = &d
			break
		}
	}
	if doc == nil {
		t.Fatal("找不到 /suno/submit 的接口文档")
	}
	var action *apidoc.Field
	for i := range doc.Fields {
		if doc.Fields[i].Name == "action" {
			action = &doc.Fields[i]
			break
		}
	}
	if action == nil {
		t.Fatal("提交页必须声明 action 字段")
	}
	if len(action.Enum) != len(sunoActionEndpoints) {
		t.Fatalf("文档写了 %d 个 action，控制器认 %d 个", len(action.Enum), len(sunoActionEndpoints))
	}
	for _, v := range action.Enum {
		got, ok := SunoActionOf(v)
		if !ok {
			t.Fatalf("文档里的 action %q 控制器不认", v)
		}
		if _, has := sunoActionEndpoints[got]; !has {
			t.Fatalf("action %q 没有对应的内部端点", v)
		}
	}
}

// 七个码必须逐字对齐 new-api：客户端按 code 分流，换词就等于全部落进 default。
func TestSunoErrorCodes(t *testing.T) {
	want := map[errs.Def]string{
		errs.SunoInvalidRequest: "invalid_request",
		errs.SunoAuth:           "authentication_error",
		errs.SunoPermission:     "permission_denied",
		errs.SunoTaskNotFound:   "task_not_found",
		errs.SunoConflict:       "request_conflict",
		errs.SunoRateLimited:    "rate_limit_exceeded",
		errs.SunoServerError:    "server_error",
	}
	for def, code := range want {
		if def.Code != code {
			t.Fatalf("%q 的 code 应为 %q，实际 %q", code, code, def.Code)
		}
		if def.HTTP < 400 || def.HTTP >= 600 {
			t.Fatalf("%q 的状态码不合法：%d", code, def.HTTP)
		}
	}
}

// TestSunoFailProjectsCodes 守住 controller 出口的码投影（投影本身在 gateway.sunoWire）。
//
// HTTP 403 必须走 permission_denied 而不是 invalid_request：403 是「换 Key/开权限」，
// 400 是「改参数」，处置完全不同。413 必须保留 413：与 400 是两种改法。
func TestSunoFailProjectsCodes(t *testing.T) {
	cases := []struct {
		in         errs.Def
		wantCode   string
		wantStatus int
	}{
		{errs.Def{Code: "whatever", HTTP: 400}, errs.CodeSunoInvalidRequest, 400},
		{errs.Def{Code: "whatever", HTTP: 401}, errs.CodeSunoAuth, 401},
		{errs.Def{Code: "whatever", HTTP: 403}, errs.CodeSunoPermission, 403},
		{errs.Def{Code: "whatever", HTTP: 402}, errs.CodeSunoPermission, 403},
		{errs.Def{Code: "whatever", HTTP: 404}, errs.CodeSunoTaskNotFound, 404},
		{errs.Def{Code: "whatever", HTTP: 409}, errs.CodeSunoConflict, 409},
		{errs.Def{Code: "whatever", HTTP: 413}, errs.CodeSunoInvalidRequest, 413},
		{errs.Def{Code: "whatever", HTTP: 429}, errs.CodeSunoRateLimited, 429},
		{errs.Def{Code: "whatever", HTTP: 502}, errs.CodeSunoServerError, 502},
		{errs.Def{Code: "whatever", HTTP: 503}, errs.CodeSunoServerError, 503},
	}
	for _, c := range cases {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/suno/fetch/task_x", nil)
		gateway.SetFormat(ctx, wireformat.NewAPI)

		sunoFail(ctx, c.in)

		if w.Code != c.wantStatus {
			t.Fatalf("HTTP %d 应回 %d，实际 %d（body=%s）", c.in.HTTP, c.wantStatus, w.Code, w.Body.String())
		}
		if body := w.Body.String(); !strings.Contains(body, `"code":"`+c.wantCode+`"`) {
			t.Fatalf("HTTP %d 应映射成 %q，实际 %s", c.in.HTTP, c.wantCode, body)
		}
	}
}

// TestNoModelOnSaleIs503 守住「没有在售模型」的状态码。
//
// 这条曾经是 500（SunoServerError）。500 的语义是"服务端有 bug，别重试"，
// 但真实原因是运营没上架/没绑渠道 —— 客户端应当退避重试或换模型，也就是 503。
// 同时它必须留在 SUNO 的七码里：HTTP 503 + body code=server_error。
func TestNoModelOnSaleIs503(t *testing.T) {
	if errs.ModelUnavailable.HTTP != 503 {
		t.Fatalf("ModelUnavailable 应为 503，实际 %d", errs.ModelUnavailable.HTTP)
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/suno/submit/music", nil)
	gateway.SetFormat(ctx, wireformat.NewAPI)

	sunoFail(ctx, errs.ModelUnavailable)

	if w.Code != 503 {
		t.Fatalf("HTTP 不应被改写，实际 %d", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, `"code":"`+errs.CodeSunoServerError+`"`) {
		t.Fatalf("503 在 /suno 上应投影成 server_error，实际 %s", body)
	}
}

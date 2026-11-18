package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
)

// 本文件覆盖 multipart 直链化的**前置判定**（不涉及上传落库）。
//
// 真正把文件落成直链需要 DB + 存储，属于集成测试范畴；
// 这里锁死三件最容易写错的事：不该转的时候别转、非法请求要 400、文件字段名要白名单。

// mpAdapter 是「声明需要直链」的假适配器。
//
// 嵌入 Adapter 接口而不是实现全部方法：这里只需要满足类型断言，
// 未实现的方法在测试里不会被调用（调用会 nil panic，反而能暴露误用）。
type mpAdapter struct {
	Adapter
	need map[EndpointKind]bool
}

func (a *mpAdapter) NeedsPublicURL(kind EndpointKind) bool { return a.need[kind] }

// plainAdapter 不实现 MultipartURLSupporter（模拟 OpenAI 兼容渠道）。
type plainAdapter struct{ Adapter }

// TestMaterializerSkipsNonMultipartEndpoint 确认 JSON 端点原样透传。
//
// 若这里误转，所有普通 JSON 请求都会被当成 multipart 解析并 400。
func TestMaterializerSkipsNonMultipartEndpoint(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2"}`)
	m := &multipartMaterializer{body: body, contentType: "application/json"}
	got, ct, err := m.forAdapter(context.Background(), nil, &mpAdapter{need: map[EndpointKind]bool{
		KindImagesEdits: true,
	}}, kindImagesGenerations)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if !bytes.Equal(got, body) || ct != "application/json" {
		t.Fatalf("JSON 端点必须原样透传，实际 ct=%q body=%s", ct, got)
	}
}

// TestMaterializerSkipsAdapterWithoutInterface 确认不声明直链的适配器拿到原始 multipart。
//
// OpenAI 兼容渠道必须收到原始 multipart：替它转成 JSON 会直接被上游 400。
func TestMaterializerSkipsAdapterWithoutInterface(t *testing.T) {
	body := []byte("--x\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\nhi\r\n--x--\r\n")
	ct := "multipart/form-data; boundary=x"
	m := &multipartMaterializer{body: body, contentType: ct}
	got, gotCT, err := m.forAdapter(context.Background(), nil, &plainAdapter{}, kindImagesEdits)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if !bytes.Equal(got, body) || gotCT != ct {
		t.Fatalf("未声明直链的适配器应拿到原始 multipart")
	}
}

// TestMaterializerRejectsMissingBoundary 确认非法 multipart 是调用方错误（400）。
func TestMaterializerRejectsMissingBoundary(t *testing.T) {
	m := &multipartMaterializer{body: []byte("garbage"), contentType: "multipart/form-data"}
	_, _, err := m.forAdapter(context.Background(), nil, &mpAdapter{need: map[EndpointKind]bool{
		KindImagesEdits: true,
	}}, kindImagesEdits)
	if !errors.Is(err, ErrCallerParam) {
		t.Fatalf("缺 boundary 应归因为 ErrCallerParam，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("文案应点明 boundary，实际 %q", err.Error())
	}
}

// TestMaterializerKeepsUnknownTextFields 文本字段不再按名字过滤：
// 平台不认识的键由上游决定收不收，吞掉的代价是「用户以为参数生效了」。
func TestMaterializerKeepsUnknownTextFields(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, kv := range [][2]string{
		{"model", "gpt-image-2"}, {"prompt", "a cat"},
		{"n", "2"}, {"seed", "42"}, {"output_format", "png"},
	} {
		_ = w.WriteField(kv[0], kv[1])
	}
	_ = w.Close()

	m := &multipartMaterializer{body: buf.Bytes(), contentType: w.FormDataContentType()}
	out, ct, err := m.forAdapter(context.Background(), nil, &mpAdapter{need: map[EndpointKind]bool{
		KindImagesEdits: true,
	}}, kindImagesEdits)
	if err != nil {
		t.Fatalf("纯文本 multipart 不该失败：%v", err)
	}
	if ct != "application/json" {
		t.Fatalf("应转成 JSON，实际 %q", ct)
	}
	var got map[string]any
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("输出不是合法 JSON：%s", out)
	}
	for k, want := range map[string]any{
		"model": "gpt-image-2", "prompt": "a cat", "seed": "42", "output_format": "png",
	} {
		if got[k] != want {
			t.Fatalf("字段 %q 应原样保留，实际 %#v（全部=%v）", k, got[k], got)
		}
	}
	// n 必须是数字：上游若收到字符串 "2" 有的网关会当类型错。
	if got["n"] != float64(2) {
		t.Fatalf("n 应归一成数字，实际 %#v", got["n"])
	}
}

// TestMaterializerUnknownFileFieldChecksFileType 未知文件字段不再因为「名字不认识」被拒，
// 但仍要按魔数判族 —— 文案必须说的是文件类型，不是字段名。
func TestMaterializerUnknownFileFieldChecksFileType(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("reference", "a.png")
	if err != nil {
		t.Fatalf("构造 multipart 失败：%v", err)
	}
	_, _ = part.Write([]byte("not-a-real-png"))
	_ = w.Close()

	m := &multipartMaterializer{body: buf.Bytes(), contentType: w.FormDataContentType()}
	_, _, ferr := m.forAdapter(context.Background(), nil, &mpAdapter{need: map[EndpointKind]bool{
		KindImagesEdits: true,
	}}, kindImagesEdits)
	if !errors.Is(ferr, ErrCallerParam) {
		t.Fatalf("非图片文件应归因为 ErrCallerParam，实际 %v", ferr)
	}
	if !strings.Contains(ferr.Error(), "reference") {
		t.Fatalf("文案应点出字段名，实际 %q", ferr.Error())
	}
	if strings.Contains(ferr.Error(), "不支持的文件字段") {
		t.Fatalf("字段名本身不该被拒，实际 %q", ferr.Error())
	}
}

// TestMaterializerRejectsNonImageFile 确认「image 字段收到的不是图片」当场 400。
//
// 真实故障（2026-11-04 实测）：上传 plain.txt 时，通用上传白名单（txt/csv/pdf 都合法）
// 放行了它，随后对象存储 Put 失败 → 503「服务暂时不可用」，耗时 17.8 秒。
// 调用方的错被报成平台故障，还白占一次上传并发闸 —— 必须在落存储之前按魔数判族。
func TestMaterializerRejectsNonImageFile(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("image", "a.png")
	if err != nil {
		t.Fatalf("构造 multipart 失败：%v", err)
	}
	_, _ = part.Write([]byte("hello, not an image"))
	_ = w.Close()

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/images/edits", nil)
	c.Set("api_key_record", &model.APIKey{ID: 1})
	c.Set("user_record", &model.User{ID: 1})

	m := &multipartMaterializer{body: buf.Bytes(), contentType: w.FormDataContentType()}
	_, _, ferr := m.forAdapter(context.Background(), c, &mpAdapter{need: map[EndpointKind]bool{
		KindImagesEdits: true,
	}}, kindImagesEdits)

	if !errors.Is(ferr, ErrCallerParam) {
		t.Fatalf("非图片文件应归因为 ErrCallerParam，实际 %v", ferr)
	}
	if !strings.Contains(ferr.Error(), "只接受图片") {
		t.Fatalf("文案应说明只收图片，实际 %q", ferr.Error())
	}
}

// TestMaterializerRejectsPrivateBaseURL 确认「对外地址是本机/内网」时**不提交上游任务**。
//
// 这是本轮真实故障的根因：站点基址配的是前端域，`/f/*` 没转发，上游回取拿到的是
// index.html，于是任务失败且「未返回原因」——白冻结一次额度、白付一次上游成本，
// 而错误完全不可归因。必须在提交之前拦住并说清该改哪里。
func TestMaterializerRejectsPrivateBaseURL(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("image", "a.png")
	if err != nil {
		t.Fatalf("构造 multipart 失败：%v", err)
	}
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	_ = w.Close()

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/images/edits", nil)
	c.Set("api_key_record", &model.APIKey{ID: 1})
	c.Set("user_record", &model.User{ID: 1})

	m := &multipartMaterializer{body: buf.Bytes(), contentType: w.FormDataContentType()}
	_, _, ferr := m.forAdapter(context.Background(), c, &mpAdapter{need: map[EndpointKind]bool{
		KindImagesEdits: true,
	}}, kindImagesEdits)

	if ferr == nil {
		t.Fatalf("本机地址应被拦下")
	}
	// 平台配置问题不能算调用方的错，否则用户会一直改请求参数。
	if errors.Is(ferr, ErrCallerParam) {
		t.Fatalf("不应归因为 ErrCallerParam：%v", ferr)
	}
	if !strings.Contains(ferr.Error(), "公网") {
		t.Fatalf("文案应指出需要公网地址，实际 %q", ferr.Error())
	}
}

// TestPublicBaseReachable 覆盖可达性判定（只拦本机/内网，不解析域名）。
func TestPublicBaseReachable(t *testing.T) {
	cases := []struct {
		base string
		want bool
	}{
		{"http://127.0.0.1:8080", false},
		{"http://localhost:8080", false},
		{"http://foo.localhost", false},
		{"http://10.1.2.3", false},
		{"http://192.168.1.5:8080", false},
		{"http://172.16.0.9", false},
		{"http://0.0.0.0:8080", false},
		{"http://169.254.1.1", false},
		{"https://api.example.com", true},
		{"https://lapi.example.com", true},
		{"https://8.8.8.8", true},
		{"", false},
		{"not-a-url", false},
	}
	for _, c := range cases {
		if got := publicBaseReachable(c.base); got != c.want {
			t.Fatalf("publicBaseReachable(%q) 应为 %v，实际 %v", c.base, c.want, got)
		}
	}
}

// TestMaterializerResultCached 确认一份请求只转换一次（failover 不重复上传）。
//
// 重复上传会消耗用户的每日上传次数与总容量，是「换个渠道重试一次，配额就少两次」的根因。
func TestMaterializerResultCached(t *testing.T) {
	m := &multipartMaterializer{body: []byte("garbage"), contentType: "multipart/form-data; boundary=x"}
	a := &mpAdapter{need: map[EndpointKind]bool{KindImagesEdits: true}}

	_, _, err1 := m.forAdapter(context.Background(), nil, a, kindImagesEdits)
	_, _, err2 := m.forAdapter(context.Background(), nil, a, kindImagesEdits)
	if err1 == nil || err2 == nil {
		t.Fatalf("两次都应返回同一批错误（该用例用非法 body 触发确定性失败）")
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("第二次应复用首次结果，实际 %q vs %q", err1.Error(), err2.Error())
	}
}

// TestMultipartFileFamilyMessageMatchesBehavior 守「提示与实际放行一致」。
//
// 缺陷现场：判定按魔数表（png/jpeg/gif/bmp/webp 都算 image），文案却只写
// 「png / jpg / jpeg / webp」。用户看到提示会以为 gif 不支持，而它其实能过；
// 反过来运维看到 gif 通过又会怀疑校验没生效。
func TestMultipartFileFamilyMessageMatchesBehavior(t *testing.T) {
	build := func(header []byte) ([]byte, string) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		part, err := w.CreateFormFile("image", "a.bin")
		if err != nil {
			t.Fatalf("构造 multipart 失败：%v", err)
		}
		_, _ = part.Write(header)
		_ = w.Close()
		return buf.Bytes(), w.FormDataContentType()
	}
	gin.SetMode(gin.TestMode)
	ctx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/images/edits", nil)
		c.Set("api_key_record", &model.APIKey{ID: 1})
		c.Set("user_record", &model.User{ID: 1})
		return c
	}
	run := func(body []byte, ct string) error {
		m := &multipartMaterializer{body: body, contentType: ct}
		_, _, err := m.forAdapter(context.Background(), ctx(), &mpAdapter{need: map[EndpointKind]bool{
			KindImagesEdits: true,
		}}, kindImagesEdits)
		return err
	}

	// ① 文本文件：拒绝，且提示列出实际放行的格式（含 gif / bmp）。
	body, ct := build([]byte("hello, not an image"))
	err := run(body, ct)
	if !errors.Is(err, ErrCallerParam) {
		t.Fatalf("非图片应归因为 ErrCallerParam，实际 %v", err)
	}
	for _, want := range []string{"gif", "bmp", "png", "webp"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("提示应列出 %q（与实际放行一致），实际 %q", want, err.Error())
		}
	}

	// ② GIF 头：族判定必须通过（错误应来自后续「本机地址」检查，而不是「只接受图片」）。
	body, ct = build([]byte("GIF89a\x01\x00\x01\x00fake"))
	err = run(body, ct)
	if err == nil {
		t.Fatal("测试环境是本机地址，应在站点地址检查处失败")
	}
	if strings.Contains(err.Error(), "只接受图片") {
		t.Fatalf("GIF 属于 image 族，不应被族校验拒绝：%v", err)
	}
	if !strings.Contains(err.Error(), "公网") {
		t.Fatalf("应停在本机地址检查，实际 %q", err.Error())
	}
}

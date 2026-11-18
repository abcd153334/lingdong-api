package controller

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 本文件覆盖 P5（video multipart 判定）：
//
// readSubmitBody / submitReqSource 的 Content-Type 判定必须 ToLower + TrimSpace。
// 修复前裸 HasPrefix 会把 "Multipart/Form-Data; boundary=..."（部分 SDK 首字母
// 大写）判成 JSON，文件 part 被静默丢弃，返回一个与真实原因毫不相干的 400。
// 正确范式 = media_task.readJSONOnlyBody（ToLower+TrimSpace）。

// buildVideoMultipart 构造一个带表单字段与文件 part 的 multipart 报文。
func buildVideoMultipart(t *testing.T) (body string, boundary string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("model", "pixel-01"); err != nil {
		t.Fatalf("写表单字段失败：%v", err)
	}
	if err := w.WriteField("width", "480"); err != nil {
		t.Fatalf("写表单字段失败：%v", err)
	}
	fw, err := w.CreateFormFile("image", "ref.png")
	if err != nil {
		t.Fatalf("建文件 part 失败：%v", err)
	}
	// 1x1 PNG 魔数头（这里只测入口分支，不需要合法图片）。
	if _, err := fw.Write([]byte("\x89PNG\r\n\x1a\n")); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("收尾 multipart 失败：%v", err)
	}
	return buf.String(), w.Boundary()
}

// callReadSubmitBody 以指定 Content-Type 调 readSubmitBody。
func callReadSubmitBody(t *testing.T, body, ctype string) ([]byte, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	c.Request = req
	return readSubmitBody(c)
}

func TestVideoSubmitBodyMultipartCasingVariants(t *testing.T) {
	raw, boundary := buildVideoMultipart(t)
	// 三种变体都必须走 multipart 分支：标准小写、首字母大写、前置空格。
	for _, ctype := range []string{
		"multipart/form-data; boundary=" + boundary,
		"Multipart/Form-Data; boundary=" + boundary,
		" MULTIPART/FORM-DATA; boundary=" + boundary,
	} {
		got, err := callReadSubmitBody(t, raw, ctype)
		if err != nil {
			t.Fatalf("ctype=%q 应走 multipart 分支且无错：%v", ctype, err)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("ctype=%q 产物不是 JSON：%v（%s）", ctype, err, got)
		}
		if m["model"] != "pixel-01" {
			t.Errorf("ctype=%q 表单字段未合成：model=%v", ctype, m["model"])
		}
		// 数字字段被 ParseFloat 归一。
		if m["width"] != float64(480) {
			t.Errorf("ctype=%q 数字字段未归一：width=%v", ctype, m["width"])
		}
		img, _ := m["image"].(string)
		// CreateFormFile 不写 Content-Type，处理器按约定回落 application/octet-stream；
		// 这里只断言「文件 part 被编码成了 data:URL」这个分支行为本身。
		if !strings.HasPrefix(img, "data:") || !strings.Contains(img, ";base64,") {
			t.Errorf("ctype=%q 文件 part 未转 data:URL：image=%v", ctype, m["image"])
		}
	}
}

func TestVideoSubmitBodyJSONBranchUnchanged(t *testing.T) {
	// JSON 分支必须原样透传（不是 multipart 判定收紧的对象）。
	body := `{"model":"pixel-01","prompt":"a cat"}`
	got, err := callReadSubmitBody(t, body, "application/json")
	if err != nil {
		t.Fatalf("JSON 分支不应出错：%v", err)
	}
	if !strings.Contains(string(got), `"pixel-01"`) {
		t.Errorf("JSON 应透传，实际 %s", got)
	}
}

func TestVideoSubmitReqSourceCasingVariants(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	for _, tc := range []struct{ ctype, want string }{
		{"multipart/form-data; boundary=b", "rebuilt"},
		{"Multipart/Form-Data; boundary=b", "rebuilt"},
		{"application/json", "raw"},
	} {
		c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
		c.Request.Header.Set("Content-Type", tc.ctype)
		if got := submitReqSource(c); got != tc.want {
			t.Errorf("ctype=%q 的 ReqSource = %q，期望 %q", tc.ctype, got, tc.want)
		}
	}
}

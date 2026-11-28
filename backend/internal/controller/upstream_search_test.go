package controller

// 上游参考价查询的 handler 契约：
// 空/超长关键词必须 400 且带可操作文案；上游失败必须 503 且文案指向"稍后重试"；
// 成功时空结果必须是 []（前端把它当"没收录"，不能是 null 让表格崩）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/service/upstream"
)

func upstreamCtx(t *testing.T, target string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", target, nil)
	return c, w
}

type upstreamBody struct {
	Code      int              `json:"code"`
	Message   string           `json:"message"`
	ErrorCode string           `json:"error_code"`
	Data      *upstream.Result `json:"data"`
}

func decodeUpstreamBody(t *testing.T, w *httptest.ResponseRecorder) upstreamBody {
	t.Helper()
	var b upstreamBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, w.Body.String())
	}
	return b
}

// 必须能在测试里换掉真实拉取：这条 handler 不能被网络绑住。
func withSearcher(t *testing.T, fn func(context.Context, string, string) (upstream.Result, error)) {
	t.Helper()
	prev := searchUpstream
	searchUpstream = fn
	t.Cleanup(func() { searchUpstream = prev })
}

func TestSearchUpstreamRejectsEmptyKeyword(t *testing.T) {
	called := false
	withSearcher(t, func(context.Context, string, string) (upstream.Result, error) {
		called = true
		return upstream.Result{}, nil
	})
	c, w := upstreamCtx(t, "/api/admin/upstream/search?keyword=%20%20")
	SearchUpstream(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空关键词应为 400，实际 %d", w.Code)
	}
	b := decodeUpstreamBody(t, w)
	if b.ErrorCode != "invalid_param" || !strings.Contains(b.Message, "模型名称") {
		t.Fatalf("错误码/文案不符合预期: %+v", b)
	}
	if called {
		t.Fatal("空关键词不应触达服务层（会白拉一次 7MB）")
	}
}

func TestSearchUpstreamRejectsOverlongKeyword(t *testing.T) {
	withSearcher(t, func(context.Context, string, string) (upstream.Result, error) {
		return upstream.Result{}, nil
	})
	// 65 个汉字 = 65 rune（不能用字节数判：UTF-8 下 65 个汉字是 195 字节）
	long := strings.Repeat("模", 65)
	c, w := upstreamCtx(t, "/api/admin/upstream/search?keyword="+long)
	SearchUpstream(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超长关键词应为 400，实际 %d", w.Code)
	}
}

func TestSearchUpstreamMapsUpstreamFailureTo503(t *testing.T) {
	withSearcher(t, func(context.Context, string, string) (upstream.Result, error) {
		return upstream.Result{}, errors.New("dial tcp: timeout")
	})
	c, w := upstreamCtx(t, "/api/admin/upstream/search?keyword=gpt-4o")
	SearchUpstream(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("上游失败应为 503，实际 %d", w.Code)
	}
	b := decodeUpstreamBody(t, w)
	if !strings.Contains(b.Message, "稍后重试") {
		t.Fatalf("503 文案应可操作（稍后重试），实际 %q", b.Message)
	}
	// 内部错误细节不外泄
	if strings.Contains(b.Message, "dial tcp") {
		t.Fatalf("不应把内部错误抛给前端: %q", b.Message)
	}
}

func TestSearchUpstreamPassesTrimmedKeywordAndVendor(t *testing.T) {
	var gotKeyword, gotVendor string
	withSearcher(t, func(_ context.Context, kw, vendor string) (upstream.Result, error) {
		gotKeyword, gotVendor = kw, vendor
		return upstream.Result{
			Items: []upstream.Item{},
			Meta:  upstream.Meta{Source: "models.dev", Total: 0, WithPrice: 0, Currency: "CNY", FxRate: 7.2},
		}, nil
	})
	c, w := upstreamCtx(t, "/api/admin/upstream/search?keyword=%20gpt-4o%20&vendor=openai")
	SearchUpstream(c)

	if w.Code != http.StatusOK {
		t.Fatalf("成功应为 200，实际 %d：%s", w.Code, w.Body.String())
	}
	if gotKeyword != "gpt-4o" || gotVendor != "openai" {
		t.Fatalf("参数应 trim 后透传，实际 kw=%q vendor=%q", gotKeyword, gotVendor)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("管理面数据不能被代理缓存，实际 Cache-Control=%q", got)
	}
	// 空结果必须序列化成 []：前端拿 null 时表格会崩，且"没收录"会被读成"请求失败"。
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("空结果应序列化为 []，实际 %s", w.Body.String())
	}
}

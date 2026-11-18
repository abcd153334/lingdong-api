package relay

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件覆盖 /v1/usage/summary 与 /api/portal/usage/summary：与明细同源口径、KEY/用户作用域隔离、
// key_id 二次过滤，以及鉴权拒绝。

func recCtx(method, target string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, nil)
	return c, w
}

func envelopeItems(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body struct {
		Code int `json:"code"`
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, w.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("期望成功响应: %s", w.Body.String())
	}
	return body.Data.Items
}

// TestUsageSummaryByCurrentKey 覆盖按当前 KEY 作用域 + 按模型聚合 + 忽略客户端 key_id。
func TestUsageSummaryByCurrentKey(t *testing.T) {
	withUsageDB(t)
	seedUsage(t, db.DB, 1, 7, "chat", 300, "settled")
	seedUsage(t, db.DB, 1, 7, "chat", 200, "settled")
	seedUsage(t, db.DB, 1, 8, "chat", 900, "settled")   // 其它 KEY 必须隔离
	seedUsage(t, db.DB, 2, 77, "chat", 9999, "settled") // 其它用户必须隔离

	c, w := recCtx("GET", "/v1/usage/summary?key_id=8")
	c.Set("api_key_record", &model.APIKey{ID: 7, UserID: 1})
	UsageSummary(c)

	items := envelopeItems(t, w)
	if len(items) != 1 {
		t.Fatalf("按模型聚合应只剩 1 组: %+v", items)
	}
	if items[0]["model"] != "m-chat" || items[0]["requests"] != float64(2) ||
		items[0]["charge"] != float64(500) || items[0]["prompt_tokens"] != float64(20) {
		t.Fatalf("当前 KEY 聚合口径不符: %+v", items[0])
	}
}

// TestPortalUsageSummaryByUser 覆盖用户作用域、key_id 二次过滤与排序。
func TestPortalUsageSummaryByUser(t *testing.T) {
	withUsageDB(t)
	seedUsage(t, db.DB, 1, 7, "chat", 300, "settled")
	seedUsage(t, db.DB, 1, 8, "image", 900, "settled")
	seedUsage(t, db.DB, 2, 77, "chat", 9999, "settled") // 他人数据必须隔离

	c, w := recCtx("GET", "/api/portal/usage/summary")
	c.Set("user_id", uint64(1))
	PortalUsageSummary(c)
	items := envelopeItems(t, w)
	if len(items) != 2 {
		t.Fatalf("用户维度应聚合出 2 个模型: %+v", items)
	}
	if items[0]["model"] != "m-image" || items[0]["charge"] != float64(900) {
		t.Fatalf("应按 charge 倒序: %+v", items)
	}

	c2, w2 := recCtx("GET", "/api/portal/usage/summary?key_id=8")
	c2.Set("user_id", uint64(1))
	PortalUsageSummary(c2)
	items2 := envelopeItems(t, w2)
	if len(items2) != 1 || items2[0]["model"] != "m-image" {
		t.Fatalf("key_id 二次过滤不符: %+v", items2)
	}
}

// TestUsageSummaryRequiresKey 缺 API KEY 上下文时按 invalid_api_key 拒绝。
func TestUsageSummaryRequiresKey(t *testing.T) {
	c, w := recCtx("GET", "/v1/usage/summary")
	UsageSummary(c)
	if w.Code != 401 {
		t.Fatalf("应 401，实际 %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "invalid_api_key" {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
}

// TestPortalUsageSummaryRequiresUser 缺 JWT 用户上下文时拒绝（401 unauthorized）。
func TestPortalUsageSummaryRequiresUser(t *testing.T) {
	c, w := recCtx("GET", "/api/portal/usage/summary")
	PortalUsageSummary(c)
	if w.Code != 401 {
		t.Fatalf("应 401，实际 %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.ErrorCode != "unauthorized" {
		t.Fatalf("错误码不符: %s", w.Body.String())
	}
}

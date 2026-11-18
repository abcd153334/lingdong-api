package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
)

// TestArtifactContentURLSignsOrFallsBack 守产物地址的两种形态。
//
// 能签就签成免鉴权能力 URL（<video> 直接能播）；签不出来（没配密钥）就回落到
// 带鉴权的正式端点。**不能**给一个必然 403 的地址，也不能给空串 ——
// 客户端会把空串渲染成"产物没了"。
func TestArtifactContentURLSignsOrFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/tasks/task_x/artifacts", nil)
	c.Request.Host = "api.example.com"

	task := &model.Task{ID: 9, PublicID: "task_01HZZZZZZZZZZZZZZZZZZZZZZZ"}

	// 没有密钥：回落带鉴权端点。
	t.Setenv("APP_ENCRYPTION_KEY", "")
	conf.Load()
	got := artifactContentURL(c, task, 2)
	if !strings.HasSuffix(got, "/v1/tasks/"+task.PublicID+"/artifacts/2/content") {
		t.Fatalf("无密钥时应回落带鉴权端点，实际 %q", got)
	}

	// 有密钥：签成 /v1/media/ 能力 URL，且带的是**公开号**（不是自增 id）。
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	conf.Load()
	got = artifactContentURL(c, task, 2)
	if !strings.Contains(got, "/v1/media/") {
		t.Fatalf("有密钥时应签出能力 URL，实际 %q", got)
	}
	if strings.Contains(got, "/tasks/9/") {
		t.Fatalf("地址里不该出现自增 id: %q", got)
	}
}

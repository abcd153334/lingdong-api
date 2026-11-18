package controller

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// decodeData 解析统一响应体里的 data 段。
func decodeHealthData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v (%s)", err, recorder.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("code 应为 0，实际 %d", body.Code)
	}
	return body.Data
}

// TestHealthIsMinimal 匿名健康检查只允许五个键。
//
// 这条是「匿名端点零内部拓扑」的硬约束：有人往 Health 里加 worker/redis/plugin
// 之类字段，这里必须立刻红。
//
// mode/node 是**部署事实**（这台机器提供不提供前端、它是哪一台），不是内部拓扑：
// 不含渠道、密钥、队列、失败率，因此可以匿名下发 —— 它也是多节点排障的第一手信息。
func TestHealthIsMinimal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/health", nil)

	Health(c)

	data := decodeHealthData(t, w)
	want := map[string]struct{}{"status": {}, "version": {}, "uptime": {}, "mode": {}, "node": {}}
	if len(data) != len(want) {
		t.Fatalf("/api/health 只允许 status/version/uptime/mode/node 五个键，实际 %d 个：%v", len(data), data)
	}
	for k := range data {
		if _, ok := want[k]; !ok {
			t.Fatalf("/api/health 出现多余键 %q（内部拓扑不得匿名下发）", k)
		}
	}
	if data["status"] != "ok" {
		t.Fatalf("status 应为 ok，实际 %v", data["status"])
	}
	if m, _ := data["mode"].(string); m != "full" && m != "api" {
		t.Fatalf("mode 必须是 full 或 api，实际 %v", data["mode"])
	}
	if n, _ := data["node"].(string); n == "" {
		t.Fatal("node 不能为空（多节点排障需要它确认落在哪台）")
	}
}

// TestAdminHealthKeepsDiagnostics 管理台健康检查保留 Worker 运行态。
func TestAdminHealthKeepsDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/admin/system/health", nil)

	AdminHealth(c)

	data := decodeHealthData(t, w)
	if _, ok := data["worker"]; !ok {
		t.Fatal("管理台健康检查必须保留 worker 运行态（否则定时任务停摆无从判断）")
	}
	// 插件执行服务已下线：不得再出现 plugin_runner 字段（前端与排障脚本已不再读它）。
	if _, ok := data["plugin_runner"]; ok {
		t.Fatal("插件系统已移除，管理台健康检查不得再下发 plugin_runner")
	}
	worker, ok := data["worker"].(map[string]any)
	if !ok {
		t.Fatalf("worker 段不是对象：%T", data["worker"])
	}
	// 六个字段名保持不变（排障脚本/文档按名字读）。
	for _, k := range []string{"leader", "redis_ready", "strict_leader", "polling_paused", "reason", "jobs"} {
		if _, ok := worker[k]; !ok {
			t.Fatalf("worker 缺少字段 %q", k)
		}
	}
}

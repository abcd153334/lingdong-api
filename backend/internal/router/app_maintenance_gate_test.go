package router_test

// 维护模式闸的 HTTP 级用例。
//
// 它必须挂在 TokenAuth 里、且在 rpm/并发之前：否则"维护期间每分钟 600 次"
// 会把用户的 RPM 额度烧光，维护结束后仍被限流；同时每被拒一次还会落一条
// usages 记录，把调用记录变成噪声。

import (
	"net/http"
	"strings"
	"testing"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	appsvc "lingdong-api/internal/service/app"
)

func countUsages(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := db.DB.Model(&model.Usage{}).Count(&n).Error; err != nil {
		t.Fatalf("统计 usages 失败: %v", err)
	}
	return n
}

func TestMaintenanceBlocksHostedKeyWithoutSideEffects(t *testing.T) {
	r, token := setupUserAPI(t)
	app := seedApp(t, "maint-app")
	plain := issueHosted(t, r, token, app.Slug)

	// 开启维护：写库 + 让本实例的 10s 快照立即失效。
	if err := db.DB.Model(&model.App{}).Where("id = ?", app.ID).
		Updates(map[string]any{"maintenance_mode": true, "maintenance_message": "今晚 02:00 升级"}).Error; err != nil {
		t.Fatalf("开启维护失败: %v", err)
	}
	appsvc.InvalidateMaintenance()

	// 测试 schema 未必迁移了 usages（取决于脚手架），有表才做"不落记录"的断言。
	hasUsages := db.DB.Migrator().HasTable("usages")
	before := int64(0)
	if hasUsages {
		before = countUsages(t)
	}
	rec := doAPI(t, r, http.MethodGet, "/v1/models", plain, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("维护中应 503，实际 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "今晚 02:00 升级") {
		t.Fatalf("应下发运营配置的维护文案，实际 %s", rec.Body.String())
	}
	if hasUsages {
		if after := countUsages(t); after != before {
			t.Fatalf("维护拒绝不应落 usages（before=%d after=%d）", before, after)
		}
	}

	// 关闭维护后恢复：闸不能把应用永久锁死。
	if err := db.DB.Model(&model.App{}).Where("id = ?", app.ID).
		Update("maintenance_mode", false).Error; err != nil {
		t.Fatalf("关闭维护失败: %v", err)
	}
	appsvc.InvalidateMaintenance()
	if rec := doAPI(t, r, http.MethodGet, "/v1/models", plain, nil); rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("关闭维护后不应再 503: %s", rec.Body.String())
	}
}

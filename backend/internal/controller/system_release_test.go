package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

const releaseTestSchema = "ld_test_release_ctrl"

const releaseIndexDDL = `
CREATE UNIQUE INDEX IF NOT EXISTS uq_release_update_active
  ON release_update_jobs ((true))
  WHERE status IN ('queued','fetching','building','backing_up','switching','verifying','rolling_back');
CREATE UNIQUE INDEX IF NOT EXISTS uq_release_update_request
  ON release_update_jobs (request_id);
`

func withReleaseDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		if os.Getenv("LD_REQUIRE_TEST_DSN") == "1" {
			t.Fatal("LD_REQUIRE_TEST_DSN=1 但未设置 LD_TEST_DSN：集成用例不允许被静默跳过")
		}
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + releaseTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + releaseTestSchema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+releaseTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.ReleaseUpdateJob{}, &model.ReleaseAgentState{},
		&model.AdminAudit{}, &model.Task{}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if err := gdb.Exec(releaseIndexDDL).Error; err != nil {
		t.Fatalf("建索引失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		db.DB = prev
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + releaseTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

func releaseCtx(t *testing.T, method, path string, body any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	c.Request = httptest.NewRequest(method, path, &buf)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", uint64(1))
	return c, w
}

func TestReleasePayloadNotConfigured(t *testing.T) {
	gdb := withReleaseDB(t)
	_ = gdb
	// "未配置" 在加了出厂默认清单地址（releasecheck.DefaultManifestURL）之后有了新含义：
	// 空配置 = 用官方清单（算已配置）。要表达"这台实例不检查更新"只能显式写 off。
	// 旧用例把"空 = 未配置"当成了不变量，默认值一落地它就恒红。
	conf.System().Reload(map[string]json.RawMessage{
		"release_manifest_url": json.RawMessage(`"off"`),
	})
	t.Cleanup(func() { conf.System().Reload(map[string]json.RawMessage{}) })

	c, w := releaseCtx(t, http.MethodGet, "/api/admin/system/release", nil)
	AdminReleaseGet(c)
	if w.Code != 200 {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	var resp struct {
		Data ReleasePayload `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data.Manifest.Configured {
		t.Fatal("未配置清单地址时 configured 必须为 false")
	}
	if resp.Data.Update.BlockedReason != "not_configured" {
		t.Fatalf("应给出 not_configured，实际 %q", resp.Data.Update.BlockedReason)
	}
	if resp.Data.Update.CanUpdate {
		t.Fatal("未配置时不允许更新")
	}
	if resp.Data.Current.Version != Version {
		t.Fatalf("当前版本必须来自后端自身（%s），实际 %s", Version, resp.Data.Current.Version)
	}
}

// nextPatchVersion 把 "vX.Y.Z" 的 Z 加一（解析失败时退回一个必然更高的值）。
//
// 存在的理由：发布版本的用例只能相对当前版本写，任何绝对版本号都会在下次发版时翻红。
func nextPatchVersion(v string) string {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return "v99.0.0"
	}
	z, err := strconv.Atoi(parts[2])
	if err != nil {
		return "v99.0.0"
	}
	return fmt.Sprintf("v%s.%s.%d", parts[0], parts[1], z+1)
}

func TestReleaseUpdateRejectsBadTag(t *testing.T) {
	withReleaseDB(t)
	for _, tag := range []string{"", "latest", "v2.1.0;ls", "../x", "v2.1"} {
		c, w := releaseCtx(t, http.MethodPost, "/api/admin/system/release/update",
			map[string]any{"target_tag": tag, "request_id": "req-abcdefgh"})
		AdminReleaseUpdate(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("tag=%q 应 400，实际 %d（%s）", tag, w.Code, w.Body.String())
		}
	}
}

func TestReleaseUpdateRejectsDowngrade(t *testing.T) {
	withReleaseDB(t)
	c, w := releaseCtx(t, http.MethodPost, "/api/admin/system/release/update",
		map[string]any{"target_tag": "v0.9.0", "request_id": "req-downgrade"})
	AdminReleaseUpdate(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("降级未确认应 409，实际 %d（%s）", w.Code, w.Body.String())
	}
}

func TestReleaseUpdateInProgressThenCurrent(t *testing.T) {
	gdb := withReleaseDB(t)

	// 目标版本必须**从 Version 派生**：写死 "v2.2.0" 时它会随发布变成"降级"，
	// 于是这条用例在 v2.2.11 那天开始恒红 —— 而它真正要测的是"第二条 active 冲突"。
	first := nextPatchVersion(Version)
	c1, w1 := releaseCtx(t, http.MethodPost, "/api/admin/system/release/update",
		map[string]any{"target_tag": first, "request_id": "req-first-0001"})
	AdminReleaseUpdate(c1)
	if w1.Code != 200 {
		t.Fatalf("首次提交应 200（目标 %s，当前 %s），实际 %d（%s）",
			first, Version, w1.Code, w1.Body.String())
	}

	c2, w2 := releaseCtx(t, http.MethodPost, "/api/admin/system/release/update",
		map[string]any{"target_tag": nextPatchVersion(first), "request_id": "req-second-001"})
	AdminReleaseUpdate(c2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("第二条 active 应 409，实际 %d（%s）", w2.Code, w2.Body.String())
	}

	// GET current 必须能看到这条进行中的任务（管理员刷新页面后要接得上）。
	c3, w3 := releaseCtx(t, http.MethodGet, "/api/admin/system/release/update/current", nil)
	AdminReleaseCurrent(c3)
	var resp struct {
		Data struct {
			Active *map[string]any `json:"active"`
			Last   *map[string]any `json:"last"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w3.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data.Active == nil {
		t.Fatalf("current 应返回进行中的任务，body=%s", w3.Body.String())
	}
	if got := (*resp.Data.Active)["status"]; got != "queued" {
		t.Fatalf("新任务应为 queued，实际 %v", got)
	}

	// 审计必须已经落库（提交那一刻，而不是更新完成之后）。
	var n int64
	if err := gdb.Model(&model.AdminAudit{}).
		Where("action = ?", "release.update.request").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("提交应写一条 release.update.request 审计，实际 %d", n)
	}
}

// ReleasePayload 是测试用的响应结构（只取断言需要的字段）。
type ReleasePayload struct {
	Current struct {
		Version string `json:"version"`
		Node    string `json:"node"`
		Mode    string `json:"mode"`
	} `json:"current"`
	Agent struct {
		Online bool `json:"online"`
	} `json:"agent"`
	Manifest struct {
		Configured      bool   `json:"configured"`
		ErrorKind       string `json:"error_kind"`
		UpdateAvailable bool   `json:"update_available"`
	} `json:"manifest"`
	Update struct {
		CanUpdate     bool   `json:"can_update"`
		BlockedReason string `json:"blocked_reason"`
	} `json:"update"`
	RunningTasks int64 `json:"running_tasks"`
}

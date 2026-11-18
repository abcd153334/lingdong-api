package controller

import (
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

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件覆盖门户同步产物代理端点 GET /api/portal/usage/:id/media/:index。
//
// 关键点不是"能取到图"，而是四条边界：越权、越界、产物过期、以及出站安全
// （Content-Type 白名单降级、体积上限、Range 透传）。

const portalMediaSchema = "ld_test_portal_media"

// withPortalMediaDB 在独立 schema 里准备 usages / call_io 等表并指向 db.DB。
func withPortalMediaDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过门户媒体代理集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + portalMediaSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+portalMediaSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Usage{}, &model.CallIO{}, &model.Channel{},
		&model.User{}, &model.APIKey{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		db.DB = prev
		if sqlDB, cerr := gdb.DB(); cerr == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// insertUsageWithMedia 造一条带 media 清单的同步调用记录，返回 usage id。
func insertUsageWithMedia(t *testing.T, gdb *gorm.DB, userID uint64, medias []model.MediaItem) uint64 {
	t.Helper()
	u := model.Usage{
		UserID: userID, KeyID: 22, Model: "gpt-image-1", Modality: "image",
		Kind: "relay", Status: "settled", CallKind: "sync",
	}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("插入 usage 失败: %v", err)
	}
	raw, err := json.Marshal(medias)
	if err != nil {
		t.Fatalf("序列化 media 失败: %v", err)
	}
	io := model.CallIO{UsageID: u.ID, UserID: userID, Media: model.JSON(raw)}
	if err := gdb.Create(&io).Error; err != nil {
		t.Fatalf("插入 call_io 失败: %v", err)
	}
	return u.ID
}

// portalMediaEngine 只挂目标 handler，避免把整条中间件链拖进用例。
func portalMediaEngine(userID uint64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Next()
	})
	r.GET("/api/portal/usage/:id/media/:index", PortalUsageMedia)
	return r
}

// fakeUpstream 启动一个本地上游（凭 LD_UPSTREAM_ALLOW_PRIVATE 放行回环）。
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	mux := http.NewServeMux()
	mux.HandleFunc("/img.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<script>alert(1)</script>"))
	})
	mux.HandleFunc("/range", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("0123456789"))
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("0123"))
	})
	mux.HandleFunc("/octet", func(w http.ResponseWriter, _ *http.Request) {
		// 很多 CDN 对图片只给 octet-stream：不按库里记的 MIME 兜底就会变成
		// 附件下载，门户里点开是一张下不了也看不了的"文件"。
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("RIFFDATA"))
	})
	mux.HandleFunc("/huge", func(w http.ResponseWriter, _ *http.Request) {
		// 只声明长度、不写体：命中体积上限时必须在读体前就拒绝。
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(usageMediaMaxBytes+1024))
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func doPortalMedia(t *testing.T, r *gin.Engine, id uint64, index string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/portal/usage/%d/media/%s", id, index), nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPortalUsageMediaForeignUserNotFound(t *testing.T) {
	gdb := withPortalMediaDB(t)
	srv := fakeUpstream(t)
	id := insertUsageWithMedia(t, gdb, 11, []model.MediaItem{{Kind: "image", URL: srv.URL + "/img.png"}})

	w := doPortalMedia(t, portalMediaEngine(22), id, "0", nil)
	// 越权与"不存在"必须同码：回 403 等于确认这条 id 存在。
	if w.Code != http.StatusNotFound {
		t.Fatalf("越权应回 404，实际 %d：%s", w.Code, w.Body.String())
	}
}

func TestPortalUsageMediaIndexOutOfRange(t *testing.T) {
	gdb := withPortalMediaDB(t)
	srv := fakeUpstream(t)
	id := insertUsageWithMedia(t, gdb, 11, []model.MediaItem{{Kind: "image", URL: srv.URL + "/img.png"}})

	w := doPortalMedia(t, portalMediaEngine(11), id, "3", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("越界应回 404，实际 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "file_not_found") {
		t.Fatalf("越界应是 file_not_found：%s", w.Body.String())
	}
}

func TestPortalUsageMediaEmptyURLExpired(t *testing.T) {
	gdb := withPortalMediaDB(t)
	id := insertUsageWithMedia(t, gdb, 11, []model.MediaItem{{Kind: "image", Note: "上游内联数据未留存"}})

	w := doPortalMedia(t, portalMediaEngine(11), id, "0", nil)
	if w.Code != http.StatusGone {
		t.Fatalf("无链接应回 410，实际 %d：%s", w.Code, w.Body.String())
	}
}

func TestPortalUsageMediaStreamsInlineImage(t *testing.T) {
	gdb := withPortalMediaDB(t)
	srv := fakeUpstream(t)
	id := insertUsageWithMedia(t, gdb, 11, []model.MediaItem{{Kind: "image", URL: srv.URL + "/img.png"}})

	w := doPortalMedia(t, portalMediaEngine(11), id, "0", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type 应透出 image/png，实际 %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("必须带 nosniff，实际 %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); got != "inline" {
		t.Fatalf("图片应内联，实际 %q", got)
	}
	if w.Body.String() != "PNGDATA" {
		t.Fatalf("体不符：%q", w.Body.String())
	}
	// 上游直链不能出现在响应头里（Location/Content-Location 之类）。
	for k, vs := range w.Header() {
		for _, v := range vs {
			if strings.Contains(v, srv.URL) {
				t.Fatalf("响应头 %s 泄漏上游地址: %s", k, v)
			}
		}
	}
}

// TestPortalUsageMediaFallsBackToStoredMIME 上游只给 octet-stream 时，
// 用调用当时落库的 MIME，否则用户看到的是一张"文件"而不是图片。
func TestPortalUsageMediaFallsBackToStoredMIME(t *testing.T) {
	gdb := withPortalMediaDB(t)
	srv := fakeUpstream(t)
	id := insertUsageWithMedia(t, gdb, 11, []model.MediaItem{
		{Kind: "image", MIME: "image/webp", URL: srv.URL + "/octet", ArtifactIndex: -1},
	})

	w := doPortalMedia(t, portalMediaEngine(11), id, "0", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "image/webp" {
		t.Fatalf("应按已存 MIME 兜底成 image/webp，实际 %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); got != "inline" {
		t.Fatalf("兜底后应内联，实际 %q", got)
	}
}

func TestPortalUsageMediaRangePassthrough(t *testing.T) {
	gdb := withPortalMediaDB(t)
	srv := fakeUpstream(t)
	id := insertUsageWithMedia(t, gdb, 11, []model.MediaItem{{Kind: "video", URL: srv.URL + "/range"}})

	w := doPortalMedia(t, portalMediaEngine(11), id, "0", map[string]string{"Range": "bytes=0-3"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("应 206，实际 %d：%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 0-3/10" {
		t.Fatalf("Content-Range 不透传：%q", got)
	}
	if w.Body.String() != "0123" {
		t.Fatalf("区间体不符：%q", w.Body.String())
	}
}

func TestPortalUsageMediaNonWhitelistBecomesAttachment(t *testing.T) {
	gdb := withPortalMediaDB(t)
	srv := fakeUpstream(t)
	id := insertUsageWithMedia(t, gdb, 11, []model.MediaItem{{Kind: "text", URL: srv.URL + "/page"}})

	w := doPortalMedia(t, portalMediaEngine(11), id, "0", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	// text/html 在同源下内联就是存储型 XSS。
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("html 应降级 octet-stream，实际 %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); got != "attachment" {
		t.Fatalf("html 应为附件，实际 %q", got)
	}
}

func TestPortalUsageMediaTooLarge(t *testing.T) {
	gdb := withPortalMediaDB(t)
	srv := fakeUpstream(t)
	id := insertUsageWithMedia(t, gdb, 11, []model.MediaItem{{Kind: "image", URL: srv.URL + "/huge"}})

	w := doPortalMedia(t, portalMediaEngine(11), id, "0", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("超限应 502，实际 %d：%s", w.Code, w.Body.String())
	}
}

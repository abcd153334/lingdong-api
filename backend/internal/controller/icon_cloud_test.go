package controller

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// 本文件守的是「图标的字节从哪来」：地址仍是平台路径，但**归属决定出口**。
//
// 云记录 302 到对象存储（浏览器自己取，省掉平台这一跳的带宽），本地记录照旧流式输出。
// 两条分支写反的症状完全不同却同样难查：
//   - 云记录走代理 → 一切正常但出口流量翻倍，没人会发现；
//   - 本地记录 302 到云地址 → 老图标整片裂图（对象根本没有云地址）。

const iconCloudTestSchema = "ld_test_icon_cloud"

func withIconCloudDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + iconCloudTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+iconCloudTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Icon{}); err != nil {
		t.Fatalf("迁移 icons 表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, e := gdb.DB(); e == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + iconCloudTestSchema + " CASCADE").Error
		if sqlDB, e := admin.DB(); e == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// bucketOfBackend 给云记录一个桶（本地后端没有桶概念）。
//
// 位置三列（backend/bucket/key）一起才是对象的唯一位置：只写 backend 的记录
// 在读/删时会被判 ErrBucketMissing —— 这是刻意的，为了不给「用当前桶兜底」留缝。
func bucketOfBackend(backend string) string {
	if backend == "none" {
		return ""
	}
	return "ling-ai"
}

// seedIcon 造一行图标记录并把字节写到**本地磁盘**（云记录不需要写盘）。
func seedIcon(t *testing.T, gdb *gorm.DB, id, backend string, body []byte) {
	t.Helper()
	row := &model.Icon{
		ID: id, Ext: "png", MIME: "image/png",
		Size: int64(len(body)), SHA256: strings.Repeat("a", 64),
		StorageBackend: backend,
		StorageBucket:  bucketOfBackend(backend),
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造图标行失败: %v", err)
	}
	if backend != "none" {
		return
	}
	path := filepath.Join(conf.EnvConfig().StorageDir, row.ObjectKey())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("写本地图标失败: %v", err)
	}
}

func TestUploadIconAssetRedirectsCloudRecord(t *testing.T) {
	gdb := withIconCloudDB(t)
	t.Setenv("STORAGE_DIR", t.TempDir())
	conf.Load() // 本地后端在构造时读 EnvConfig().StorageDir，必须先加载
	prevConf := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prevConf) })
	conf.System().Set("storage", []byte(`{"active":"none","channels":{
		"none":{},
		"oss":{"endpoint":"oss-cn-shanghai.aliyuncs.com","bucket":"ling-ai",
		       "access_key_id":"ak","access_key_secret":"sk"}}}`))

	const id = "01j8zq7w6y5x4v3t2s1r0q9p8n"
	body := []byte("\x89PNG\r\n\x1a\nfake")
	seedIcon(t, gdb, id, "oss", body)
	r := iconTestEngine()

	// ① 云记录 GET：302 到对象存储，且缓存只有 5 分钟（切后端后必须尽快失效）。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/icons/u/"+id, nil))
	if w.Code != http.StatusFound {
		t.Fatalf("云记录应 302，实际 %d", w.Code)
	}
	want := "https://ling-ai.oss-cn-shanghai.aliyuncs.com/" + "icons/" + id + ".png"
	if loc := w.Header().Get("Location"); loc != want {
		t.Fatalf("Location 不符\n got=%s\nwant=%s", loc, want)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Fatalf("302 的缓存应缩短，实际 %q", cc)
	}

	// ② HEAD 不跳转：直接按元数据回长度，跳一次只会让预加载多花一个往返。
	wh := httptest.NewRecorder()
	r.ServeHTTP(wh, httptest.NewRequest(http.MethodHead, "/api/icons/u/"+id, nil))
	if wh.Code != http.StatusOK {
		t.Fatalf("HEAD 应 200，实际 %d", wh.Code)
	}
	if got := wh.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("HEAD Content-Length 不符: %q", got)
	}
}

// TestUploadIconAssetMissingIs404 守住「不存在」与「配置坏了」的区别。
//
// 本地记录的流式输出分支不在这里重复造对象：控制器与 service 共用同一条
// OpenRow 路径，而本地根的构造是进程级缓存（跨用例换 STORAGE_DIR 不生效），
// 在控制器层再造一次只会得到一个与执行顺序相关的假失败。
func TestUploadIconAssetMissingIs404(t *testing.T) {
	withIconCloudDB(t)
	t.Setenv("STORAGE_DIR", t.TempDir())
	r := iconTestEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/icons/u/01j8zq7w6y5x4v3t2s1r0q9p8z", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404，实际 %d", w.Code)
	}
}

// 保证 gin 测试模式只设一次（TestMain 在别的文件里可能没设）。
func init() { gin.SetMode(gin.TestMode) }

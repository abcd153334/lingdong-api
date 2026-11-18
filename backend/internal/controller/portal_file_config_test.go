package controller

// 门户上传配置下发：只给「后缀 → 类别 → 单文件上限」一份真相，
// 前端不再维护第二套映射，也不再有两个体积口径。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/uploadcfg"
)

const portalFilesTestSchema = "ld_test_h5_portalfiles"

func withPortalFilesDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + portalFilesTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+portalFilesTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Upload{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + portalFilesTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// TestPortalFileConfigShape 守住门户配置下发的形状。
//
// 关键点：不再有 max_size_mb / allowed_ext 两个「半套」字段 ——
// 留一个全局上限就会让「按后缀取上限」在前端退化成「全局上限」。
func TestPortalFileConfigShape(t *testing.T) {
	withPortalFilesDB(t)
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })

	s := uploadcfg.Defaults()
	s.Limits[uploadcfg.KindImage] = 9
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("构造配置失败: %v", err)
	}
	conf.System().Set("uploads", raw)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/portal/files/config", nil)
	c.Set("user_id", uint64(1))
	PortalFileConfig(c)

	var body struct {
		Data struct {
			Types      []uploadcfg.TypeInfo `json:"types"`
			Limits     map[string]int64     `json:"limits"`
			MaxSizeMB  *int64               `json:"max_size_mb"`
			AllowedExt *[]string            `json:"allowed_ext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.MaxSizeMB != nil {
		t.Fatalf("不应再下发全局 max_size_mb：%d", *body.Data.MaxSizeMB)
	}
	if body.Data.AllowedExt != nil {
		t.Fatalf("不应再下发裸 allowed_ext（上限信息会丢）：%v", *body.Data.AllowedExt)
	}
	if len(body.Data.Types) != len(s.Types) {
		t.Fatalf("types 条数应等于启用的后缀数：got=%d want=%d", len(body.Data.Types), len(s.Types))
	}
	for _, tp := range body.Data.Types {
		if tp.Ext == "png" {
			if tp.MaxMB != 9 {
				t.Fatalf("png 上限应取自图片类（9MB），实际 %d", tp.MaxMB)
			}
			if tp.Kind != uploadcfg.KindImage {
				t.Fatalf("png 类别应为 image，实际 %s", tp.Kind)
			}
			return
		}
	}
	t.Fatal("types 里缺少 png")
}

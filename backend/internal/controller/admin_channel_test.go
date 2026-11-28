package controller

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

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"

	// 空导入注册内置适配器：parseChannelReq 的 adapterRegistered 查的是注册表。
	_ "lingdong-api/internal/relay/adapters"
)

// 本文件覆盖「控制台创建渠道时 adapter_config 的透传与拦截」。
// controller 层此前完全没有渠道用例，而这里正是最容易漏字段的一层（DTO 少一个字段 =
// 配置永远存不进去，且没有任何报错）。

const declChannelTestSchema = "ld_test_decl_channel"

func withDeclChannelDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + declChannelTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+declChannelTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}, &model.AdminAudit{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + declChannelTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

func postCreateChannel(t *testing.T, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/channels", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	CreateAdminChannel(c)
	return w.Code, w.Body.String()
}

func channelBodyWith(config string) string {
	return `{"name":"声明式渠道","adapter_id":"openai","base_url":"https://api.example.com",
		"keys":["sk-1"],"groups":["default"],"multi_key_mode":"random"` + config + `}`
}

func TestCreateChannelPersistsAdapterConfig(t *testing.T) {
	withDeclChannelDB(t)

	code, body := postCreateChannel(t, channelBodyWith(`,"adapter_config":{"auth_header":"x-api-key","auth_prefix":"none"}`))
	if code != http.StatusOK {
		t.Fatalf("创建应成功，实际 %d：%s", code, body)
	}

	var payload struct {
		Data struct {
			Item struct {
				AdapterConfig struct {
					AuthHeader string `json:"auth_header"`
					AuthPrefix string `json:"auth_prefix"`
				} `json:"adapter_config"`
			} `json:"item"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON：%v / %s", err, body)
	}
	if payload.Data.Item.AdapterConfig.AuthHeader != "x-api-key" {
		t.Fatalf("DTO 未透传配置，实际：%s", body)
	}
}

// TestCreateChannelPersistsWebsite 守 DTO 的 website 字段透传。
//
// DTO 少一个字段时 ShouldBindJSON 不报错、字段静默丢失 —— 表现是"保存成功，
// 但官网按钮永远不出现"，只有打开这一段才能发现。
func TestCreateChannelPersistsWebsite(t *testing.T) {
	withDeclChannelDB(t)

	code, body := postCreateChannel(t, channelBodyWith(`,"website":"https://duomiapi.com"`))
	if code != http.StatusOK {
		t.Fatalf("创建应成功，实际 %d：%s", code, body)
	}
	var payload struct {
		Data struct {
			Item struct {
				Website    string `json:"website"`
				WebsiteURL string `json:"website_url"`
			} `json:"item"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON：%v / %s", err, body)
	}
	if payload.Data.Item.Website != "https://duomiapi.com" {
		t.Fatalf("website 未透传，实际：%s", body)
	}
	if payload.Data.Item.WebsiteURL != "https://duomiapi.com" {
		t.Fatalf("website_url 未派生，实际：%s", body)
	}
}

func TestCreateChannelRejectsBadAdapterConfig(t *testing.T) {
	withDeclChannelDB(t)

	code, body := postCreateChannel(t, channelBodyWith(`,"adapter_config":{"path_overrides":{"/v1/chat/completions":"https://evil.com/x"}}`))
	if code != http.StatusBadRequest {
		t.Fatalf("恶意路径应被拒绝，实际 %d：%s", code, body)
	}
	// 错误文案必须点名到字段，否则运营只知道"参数不合法"。
	if !strings.Contains(body, "path_overrides") {
		t.Fatalf("错误文案应包含字段名，实际：%s", body)
	}
}

func TestCreateChannelRejectsNonObjectConfig(t *testing.T) {
	withDeclChannelDB(t)

	code, body := postCreateChannel(t, channelBodyWith(`,"adapter_config":"not-an-object"`))
	if code != http.StatusBadRequest {
		t.Fatalf("非对象配置应被拒绝，实际 %d：%s", code, body)
	}
}

// TestCreateChannelWithoutConfigUnchanged 保证「不填高级配置」的请求与改造前完全一致。
func TestCreateChannelWithoutConfigUnchanged(t *testing.T) {
	withDeclChannelDB(t)

	code, body := postCreateChannel(t, channelBodyWith(""))
	if code != http.StatusOK {
		t.Fatalf("不配置时应正常创建，实际 %d：%s", code, body)
	}
	if !strings.Contains(body, `"adapter_config":{}`) {
		t.Fatalf("未配置时应回落为零值对象，实际：%s", body)
	}
}

// ───────────────────── 密钥池的唯一写路径 ─────────────────────

func putUpdateChannel(t *testing.T, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/channels/1", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	UpdateAdminChannel(c)
	return w.Code, w.Body.String()
}

func patchChannelKeys(t *testing.T, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/channels/1/keys", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	PatchAdminChannelKeys(c)
	return w.Code, w.Body.String()
}

// TestUpdateChannelRejectsKeys 守「PUT 不是密钥池的写入口」。
//
// 静默忽略比报错更糟：运营在编辑页填了密钥、点了保存、看到"已更新"，
// 而密钥池一行没变 —— 要过很久才会发现，而且会先怀疑上游。
func TestUpdateChannelRejectsKeys(t *testing.T) {
	code, body := putUpdateChannel(t, `{"name":"c","adapter_id":"openai","base_url":"https://api.example.com",
		"keys":["sk-1"],"groups":["default"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT 带 keys 应 400，实际 %d：%s", code, body)
	}
	if !strings.Contains(body, "密钥池接口") {
		t.Fatalf("文案必须指向正确入口，实际：%s", body)
	}
}

// TestPatchKeysRejectsTooManyOps 守单次提交的 ops 上限。
//
// 没有上限时，一次误粘贴会被逐条 Fernet 加密并塞进同一个事务，
// 保存请求会同步阻塞到超时。
func TestPatchKeysRejectsTooManyOps(t *testing.T) {
	ops := make([]string, 0, 201)
	for i := 0; i < 201; i++ {
		ops = append(ops, `{"op":"add","key":"sk-x"}`)
	}
	body := `{"version":1,"ops":[` + strings.Join(ops, ",") + `]}`

	code, resp := patchChannelKeys(t, body)
	if code != http.StatusBadRequest {
		t.Fatalf("201 个操作应 400，实际 %d：%s", code, resp)
	}
	if !strings.Contains(resp, "200") {
		t.Fatalf("文案应报出上限，实际：%s", resp)
	}
}

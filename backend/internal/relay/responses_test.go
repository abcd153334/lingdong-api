package relay

import (
	"encoding/json"
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
	"lingdong-api/internal/service/pricing"
)

// 本文件覆盖 /v1/responses：端点契约（路径/模态）、仅 OpenAI 兼容渠道限定，
// 以及非 openai 渠道返回 400 endpoint_unsupported、缺 KEY 拒绝。

const responsesTestSchema = "ld_test_s9_resp"

func withResponsesDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过 /v1/responses 集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + responsesTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+responsesTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Vendor{}, &model.Model{}, &model.ModelBinding{},
		&model.Channel{}, &model.APIKey{}, &model.User{}, &model.Usage{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	pricing.InvalidatePricingCache()
	t.Cleanup(func() {
		pricing.InvalidatePricingCache()
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + responsesTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// TestResponsesSpec 覆盖端点契约：上游路径 /v1/responses、默认模态 chat（§4.3.1 L383）。
func TestResponsesSpec(t *testing.T) {
	spec := specOf(kindResponses)
	if spec.path != "/v1/responses" || spec.modality != "chat" || spec.multipart || spec.binary {
		t.Fatalf("responses 契约不符: %+v", spec)
	}
	if endpointModality(kindResponses) != "chat" {
		t.Fatalf("responses 端点模态应为 chat: %s", endpointModality(kindResponses))
	}
}

// TestFilterCandidatesByAdapter 覆盖渠道适配器过滤（仅保留 openai）。
func TestFilterCandidatesByAdapter(t *testing.T) {
	cands := []Candidate{
		{Channel: &model.Channel{ID: 1, AdapterID: "openai"}},
		{Channel: &model.Channel{ID: 2, AdapterID: "volcengine"}},
		{Channel: &model.Channel{ID: 3, AdapterID: "openai"}},
		{Channel: nil},
	}
	out := filterCandidatesByAdapter(cands, "openai")
	if len(out) != 2 || out[0].Channel.ID != 1 || out[1].Channel.ID != 3 {
		t.Fatalf("适配器过滤不符: %+v", out)
	}
}

// TestResponsesRejectsNonOpenAIChannel 模型仅绑定非 openai 渠道时必须 400 endpoint_unsupported（不静默回落）。
func TestResponsesRejectsNonOpenAIChannel(t *testing.T) {
	gdb := withResponsesDB(t)
	if err := gdb.Create(&model.Vendor{ID: "openai", Name: "OpenAI"}).Error; err != nil {
		t.Fatalf("造供应商失败: %v", err)
	}
	inPrice := int64(1000000)
	if err := gdb.Create(&model.Model{ID: "gpt-4o", Vendor: "openai", Modality: "chat",
		BillingMode: model.BillingModeTokens, Prices: model.Prices{Input: &inPrice},
		Enabled: true}).Error; err != nil {
		t.Fatalf("造模型失败: %v", err)
	}
	if err := gdb.Create(&model.Channel{ID: 1, Name: "火山", AdapterID: "volcengine",
		Status: model.ChannelStatusEnabled, Priority: 1}).Error; err != nil {
		t.Fatalf("造渠道失败: %v", err)
	}
	if err := gdb.Create(&model.ModelBinding{ModelID: "gpt-4o", ChannelID: 1, Enabled: true,
		Weight: 1, Priority: 1}).Error; err != nil {
		t.Fatalf("造绑定失败: %v", err)
	}
	pricing.InvalidatePricingCache()

	r := gin.New()
	r.POST("/v1/responses", func(c *gin.Context) {
		c.Set("api_key_record", &model.APIKey{ID: 1, UserID: 1})
		c.Set("user_record", &model.User{ID: 1})
		Responses(c)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`)))

	if w.Code != 400 {
		t.Fatalf("非 openai 渠道应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "endpoint_unsupported" {
		t.Fatalf("错误码应为 endpoint_unsupported: %s", w.Body.String())
	}
	// 冻结前拦截：不得留下任何 usage 记录。
	var n int64
	if err := gdb.Model(&model.Usage{}).Count(&n).Error; err != nil {
		t.Fatalf("统计 usage 失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("路由预检失败不得建冻结/usage，实际 %d 条", n)
	}
}

// TestResponsesRequiresKey 缺 KEY 上下文时按 invalid_api_key 拒绝。
func TestResponsesRequiresKey(t *testing.T) {
	r := gin.New()
	r.POST("/v1/responses", Responses)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"x"}`)))
	if w.Code != 401 {
		t.Fatalf("应 401，实际 %d: %s", w.Code, w.Body.String())
	}
}

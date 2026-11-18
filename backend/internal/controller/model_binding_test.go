package controller

import (
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
	// 注册内置适配器：绑定校验会按 adapter_id 查适配器能力，
	// 没注册时任何绑定都会被判成「适配器不支持」。
	_ "lingdong-api/internal/relay/adapters"
)

const bindingTestSchema = "ld_test_model_binding"

func withBindingDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过绑定写入集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + bindingTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+bindingTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Model{}, &model.ModelBinding{}, &model.Channel{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + bindingTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// bindingRouter 只挂单条绑定的两条路由（与 router.go 的注册完全一致）。
func bindingRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PUT("/models/:id/bindings/:channelId", UpsertModelBinding)
	r.DELETE("/models/:id/bindings/:channelId", DeleteModelBinding)
	return r
}

// putOne 发一次单条绑定写入。
func putOne(t *testing.T, r *gin.Engine, modelID string, chID uint64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut,
		"/models/"+modelID+"/bindings/"+strconv.FormatUint(chID, 10), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// makeChannel 造一个渠道（status 列带 default:1，停用态必须补一次 UPDATE）。
func makeChannel(t *testing.T, gdb *gorm.DB, name string) model.Channel {
	t.Helper()
	ch := model.Channel{
		Name: name, AdapterID: "openai", BaseURL: "https://api.example.com",
		Keys: model.MustJSON([]string{"enc"}), MultiKeyMode: "random",
		Groups: model.MustJSON([]string{"default"}), Weight: 1, Priority: 1, Status: 1, AutoBan: true,
	}
	if err := gdb.Select("*").Create(&ch).Error; err != nil {
		t.Fatalf("造渠道失败: %v", err)
	}
	return ch
}

// TestUpsertBindingPersistsDisabledBinding 是 GORM 零值覆盖缺陷的回归用例。
//
// 背景：model_bindings.enabled 列声明 default:true，GORM 对零值字段会「不写入」，
// 于是「停用绑定」被静默保存成启用（实测：保存后选路仍命中该渠道）。
// 修复：单条写入走 map（upsertBindingRow 的上一半），不依赖结构体零值语义。
func TestUpsertBindingPersistsDisabledBinding(t *testing.T) {
	gdb := withBindingDB(t)
	m := model.Model{ID: "gpt-4o", DisplayName: "GPT-4o", Vendor: "openai", Modality: "chat", BillingMode: model.BillingModeTokens, Enabled: true,
		Endpoints: model.MustJSON([]string{"chat/completions"})}
	if err := gdb.Select("*").Create(&m).Error; err != nil {
		t.Fatalf("造模型失败: %v", err)
	}
	ch := makeChannel(t, gdb, "c1")
	r := bindingRouter(t)

	w := putOne(t, r, "gpt-4o", ch.ID, `{"weight":3,"enabled":false,"upstream_model":"gpt-4o"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("保存绑定应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var row model.ModelBinding
	if err := gdb.Where("model_id = ? AND channel_id = ?", "gpt-4o", ch.ID).First(&row).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if row.Enabled {
		t.Fatal("enabled=false 被 default:true 覆盖：停用绑定实际落库为启用")
	}
	if row.Weight != 3 {
		t.Fatalf("权重应为 3，实际 %d", row.Weight)
	}

	// 反向：enabled=true 仍应正常写入。
	if w := putOne(t, r, "gpt-4o", ch.ID, `{"weight":3,"enabled":true,"upstream_model":"gpt-4o"}`); w.Code != http.StatusOK {
		t.Fatalf("二次保存应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	row = model.ModelBinding{}
	if err := gdb.Where("model_id = ? AND channel_id = ?", "gpt-4o", ch.ID).First(&row).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if !row.Enabled {
		t.Fatal("enabled=true 应正常写入")
	}
}

// TestUpsertBindingValidatesInput 覆盖「写进去不报错、运行期才炸」的三类输入。
func TestUpsertBindingValidatesInput(t *testing.T) {
	gdb := withBindingDB(t)
	m := model.Model{ID: "gpt-4o-mini", DisplayName: "mini", Vendor: "openai", Modality: "chat", Enabled: true,
		Endpoints: model.MustJSON([]string{"chat/completions"})}
	if err := gdb.Select("*").Create(&m).Error; err != nil {
		t.Fatalf("造模型失败: %v", err)
	}
	ch1 := makeChannel(t, gdb, "c1")
	ch2 := makeChannel(t, gdb, "c2")
	r := bindingRouter(t)

	// ① 不存在的渠道 → 404（requireModel 过了，但 Upsert 的目标渠道不存在时
	// NormalizeReplace 报 FieldError「渠道 #x 不存在或已删除」→ 400）。
	w := putOne(t, r, "gpt-4o-mini", 999999, `{"upstream_model":"u"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("不存在的渠道应 400，实际 %d：%s", w.Code, w.Body.String())
	}
	var n int64
	if err := gdb.Model(&model.ModelBinding{}).Where("model_id = ?", m.ID).Count(&n).Error; err != nil {
		t.Fatalf("统计绑定失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("校验失败时不应落库，实际 %d 条", n)
	}

	// ② 权重越界 → 400
	if w := putOne(t, r, "gpt-4o-mini", ch2.ID, `{"weight":100000,"upstream_model":"u"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("权重越界应 400，实际 %d：%s", w.Code, w.Body.String())
	}

	// ③ 同一渠道重复 PUT = 覆盖（不新增行）—— 单条接口的幂等语义。
	if w := putOne(t, r, "gpt-4o-mini", ch1.ID, `{"weight":2,"upstream_model":"u"}`); w.Code != http.StatusOK {
		t.Fatalf("首次绑定应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	if w := putOne(t, r, "gpt-4o-mini", ch1.ID, `{"weight":5,"upstream_model":"u2"}`); w.Code != http.StatusOK {
		t.Fatalf("覆盖绑定应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var rows []model.ModelBinding
	if err := gdb.Where("model_id = ?", m.ID).Find(&rows).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Weight != 5 || rows[0].UpstreamModel != "u2" {
		t.Fatalf("应按渠道覆盖成 1 条且取最后一次的值，实际 %+v", rows)
	}

	// ④ 空上游名 → 400（唯一真相源是 upstream_model，空串会让上游收到 model:""）
	if w := putOne(t, r, "gpt-4o-mini", ch2.ID, `{"upstream_model":"  "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("空上游名应 400，实际 %d：%s", w.Code, w.Body.String())
	}
}

// TestUpsertBindingLeavesOtherChannelsUntouched 是「单条写」存在的理由。
//
// 整组替换是硬删重建：改 A 渠道的权重会把 B 渠道的行一起删掉重插，
// id 与 created_at 全变；两人同时改同一模型的不同渠道时，后写者会把对方的改动
// 覆盖回旧值。这里用 id + created_at 锁住"一个字节都不动"。
func TestUpsertBindingLeavesOtherChannelsUntouched(t *testing.T) {
	gdb := withBindingDB(t)
	m := model.Model{ID: "gpt-4o", DisplayName: "GPT-4o", Vendor: "openai", Modality: "chat", Enabled: true,
		Endpoints: model.MustJSON([]string{"chat/completions"})}
	if err := gdb.Select("*").Create(&m).Error; err != nil {
		t.Fatalf("造模型失败: %v", err)
	}
	ch1 := makeChannel(t, gdb, "c1")
	ch2 := makeChannel(t, gdb, "c2")
	r := bindingRouter(t)

	if w := putOne(t, r, "gpt-4o", ch1.ID, `{"weight":1,"upstream_model":"u1"}`); w.Code != http.StatusOK {
		t.Fatalf("绑定 ch1 应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	if w := putOne(t, r, "gpt-4o", ch2.ID, `{"weight":9,"upstream_model":"u2"}`); w.Code != http.StatusOK {
		t.Fatalf("绑定 ch2 应 200，实际 %d：%s", w.Code, w.Body.String())
	}

	var before2 model.ModelBinding
	if err := gdb.Where("model_id = ? AND channel_id = ?", "gpt-4o", ch2.ID).First(&before2).Error; err != nil {
		t.Fatalf("读取 ch2 绑定失败: %v", err)
	}

	// 只改 ch1
	if w := putOne(t, r, "gpt-4o", ch1.ID, `{"weight":4,"upstream_model":"u1"}`); w.Code != http.StatusOK {
		t.Fatalf("改 ch1 应 200，实际 %d：%s", w.Code, w.Body.String())
	}

	var after2 model.ModelBinding
	if err := gdb.Where("model_id = ? AND channel_id = ?", "gpt-4o", ch2.ID).First(&after2).Error; err != nil {
		t.Fatalf("读取 ch2 绑定失败: %v", err)
	}
	if after2.ID != before2.ID || !after2.CreatedAt.Equal(before2.CreatedAt) || after2.Weight != 9 {
		t.Fatalf("改 ch1 不应动 ch2：before=%+v after=%+v", before2, after2)
	}
}

// TestDeleteBindingRejectsLastUsable 解绑的两个边界。
//
// 「全部解绑」本身合法，但模型还在售时清空等于让它 100% 无法路由：
// 保存成功、目录还在挂、每次调用 503。所以必须挡住并给出可执行的下一步。
func TestDeleteBindingRejectsLastUsable(t *testing.T) {
	gdb := withBindingDB(t)
	m := model.Model{ID: "gpt-4o", DisplayName: "GPT-4o", Vendor: "openai", Modality: "chat", Enabled: true,
		Endpoints: model.MustJSON([]string{"chat/completions"})}
	if err := gdb.Select("*").Create(&m).Error; err != nil {
		t.Fatalf("造模型失败: %v", err)
	}
	ch := makeChannel(t, gdb, "c1")
	r := bindingRouter(t)
	if w := putOne(t, r, "gpt-4o", ch.ID, `{"upstream_model":"u"}`); w.Code != http.StatusOK {
		t.Fatalf("绑定应 200，实际 %d：%s", w.Code, w.Body.String())
	}

	del := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete,
			"/models/gpt-4o/bindings/"+strconv.FormatUint(ch.ID, 10), nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w := del()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("在售模型解绑唯一绑定应 400，实际 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "先停用该模型") {
		t.Fatalf("400 必须给出可执行建议，实际：%s", w.Body.String())
	}
	var still int64
	if err := gdb.Model(&model.ModelBinding{}).Where("model_id = ?", m.ID).Count(&still).Error; err != nil {
		t.Fatalf("统计绑定失败: %v", err)
	}
	if still != 1 {
		t.Fatalf("被拒的请求不应改动库内绑定，实际剩 %d 条", still)
	}

	// 先停用模型 → 再解绑：200 且真的删掉（硬删，不留墓碑占唯一键）。
	if err := gdb.Model(&model.Model{}).Where("id = ?", m.ID).Update("enabled", false).Error; err != nil {
		t.Fatalf("停用模型失败: %v", err)
	}
	if w := del(); w.Code != http.StatusOK {
		t.Fatalf("停用后解绑应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var left int64
	if err := gdb.Model(&model.ModelBinding{}).Where("model_id = ?", m.ID).Count(&left).Error; err != nil {
		t.Fatalf("统计绑定失败: %v", err)
	}
	if left != 0 {
		t.Fatalf("应硬删干净，实际剩 %d 条", left)
	}
	var ghosts int64
	if err := gdb.Unscoped().Model(&model.ModelBinding{}).Where("model_id = ?", m.ID).Count(&ghosts).Error; err != nil {
		t.Fatalf("统计墓碑失败: %v", err)
	}
	if ghosts != 0 {
		t.Fatalf("软删行会永久占用 (model_id, channel_id) 唯一键，实际留了 %d 条墓碑", ghosts)
	}

	// 再删一次 → 404（并发的第二次点击 / 前端拿了旧列表）。
	if w := del(); w.Code != http.StatusNotFound {
		t.Fatalf("重复解绑应 404，实际 %d：%s", w.Code, w.Body.String())
	}
}

package controller

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

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/pricing"
)

// 本文件覆盖 S9 新增的目录/内容只读端点：结构与 DB 同源、空态 200、默认模型兜底优先级、
// KEY 白名单 allowed 判定，以及协议正文「生效日期」单独下发。

const catalogTestSchema = "ld_test_s9_catalog"

// withCatalogDB 在独立 schema 中准备目录端点所需表并指向 db.DB。
func withCatalogDB(t *testing.T, schema string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过目录端点集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + schema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Vendor{}, &model.Model{}, &model.ModelBinding{},
		&model.Channel{}, &model.APIKey{}, &model.User{}); err != nil {
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
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

func setSysCfg(t *testing.T, key string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化配置失败: %v", err)
	}
	conf.System().Set(key, raw)
}

// seedCatalog 造 2 个供应商 + 5 个模型（含 1 个禁用模型用于验证兜底只取 enabled）。
//
// B2-6 之后目录只收「enabled 且存在启用绑定」的模型，因此这里必须同时造渠道与绑定：
// 少了绑定，前面所有目录断言都会变成「0 个模型」，而失败信息看上去像是目录坏了。
func seedCatalog(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	// 价格全部是**绝对金额**（微）：输入 1.25 元/百万 tokens = 1_250_000 微。
	models := []model.Model{
		{ID: "gpt-4o", DisplayName: "GPT-4o", Vendor: "openai", Modality: "chat", Enabled: true, IsDefault: true,
			BillingMode: model.BillingModeTokens,
			Prices:      model.Prices{Input: i64ptr(1_250_000), Output: i64ptr(3_750_000)},
			IsFree:      false},
		{ID: "gpt-4o-mini", Vendor: "openai", Modality: "chat", Enabled: true,
			BillingMode: model.BillingModeTokens, Prices: model.Prices{Input: i64ptr(75_000)}},
		{ID: "dall-e-3", Vendor: "openai", Modality: "image", Enabled: true,
			BillingMode: model.BillingModeSpec,
			SpecPrices:  model.SpecPrices{Kind: "per_image", TierPrice: map[string]int64{"1K": 50000}}},
		{ID: "kling-v1", Vendor: "kuaishou", Modality: "video", Enabled: true,
			BillingMode: model.BillingModeSpec},
		{ID: "aaa-video-old", Vendor: "kuaishou", Modality: "video", Enabled: false,
			BillingMode: model.BillingModeSpec},
	}
	for i := range models {
		if err := gdb.Create(&models[i]).Error; err != nil {
			t.Fatalf("造模型失败: %v", err)
		}
	}
	vendors := []model.Vendor{
		{ID: "openai", Name: "OpenAI", Color: "#111", Icon: "builtin:openai-gpt", Priority: 1, Sort: 1, Builtin: true},
		{ID: "kuaishou", Name: "快手", Color: "#222", Priority: 2, Sort: 2},
	}
	for i := range vendors {
		if err := gdb.Create(&vendors[i]).Error; err != nil {
			t.Fatalf("造供应商失败: %v", err)
		}
	}
	// 显式落禁用态：enabled 带 default:true，GORM 会忽略零值导致插入为启用。
	if err := gdb.Exec("UPDATE models SET enabled = false WHERE id = ?", "aaa-video-old").Error; err != nil {
		t.Fatalf("置禁用态失败: %v", err)
	}
	seedCatalogBindings(t, gdb)
	pricing.InvalidatePricingCache()
}

// seedCatalogBindings 给 4 个启用模型各挂一条启用绑定（渠道固定一条 openai）。
//
// 用 openai 适配器：它是唯一「未实现 EndpointSupporter = 全端点支持」的适配器，
// 因此 chat/image/video 三种模态都能覆盖，测试不需要为每个模态再造一个渠道。
func seedCatalogBindings(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	ch := model.Channel{
		Name: "c-catalog", AdapterID: "openai", BaseURL: "https://api.example.com",
		Keys: model.MustJSON([]string{"enc"}), MultiKeyMode: "random",
		Groups: model.MustJSON([]string{"default"}),
		Weight: 1, Priority: 1, Status: 1, AutoBan: true,
	}
	if err := gdb.Select("*").Create(&ch).Error; err != nil {
		t.Fatalf("造渠道失败: %v", err)
	}
	for _, id := range []string{"gpt-4o", "gpt-4o-mini", "dall-e-3", "kling-v1"} {
		row := model.ModelBinding{ModelID: id, ChannelID: ch.ID, Weight: 1, Priority: 1, Enabled: true}
		if err := gdb.Select("*").Create(&row).Error; err != nil {
			t.Fatalf("造绑定失败（%s）: %v", id, err)
		}
	}
}

func f64ptr(v float64) *float64 { return &v }
func i64ptr(v int64) *int64     { return &v }

// callCatalogHandler 以受控上下文调用只读 handler（set 注入鉴权上下文值）。
func callCatalogHandler(t *testing.T, handler gin.HandlerFunc, set func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		if set != nil {
			set(c)
		}
		handler(c)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	return w
}

type envBody struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	ErrorCode string          `json:"error_code"`
	Data      json.RawMessage `json:"data"`
}

func decodeData(t *testing.T, w *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	var b envBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("解析响应体失败: %v (%s)", err, w.Body.String())
	}
	if b.Code != 0 {
		t.Fatalf("期望成功响应，实际: %s", w.Body.String())
	}
	return b.Data
}

func decodeMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 data 失败: %v (%s)", err, string(raw))
	}
	return m
}

// TestPortalVendors 覆盖供应商列表与 DB 一致（§4.3.8 L496）。
func TestPortalVendors(t *testing.T) {
	gdb := withCatalogDB(t, catalogTestSchema)
	seedCatalog(t, gdb)

	w := callCatalogHandler(t, PortalVendors, nil)
	var arr []map[string]any
	if err := json.Unmarshal(decodeData(t, w), &arr); err != nil {
		t.Fatalf("vendors 应为数组: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("供应商数量不符: %+v", arr)
	}
	// 门户供应商只有 id/name/color/icon_url 四键：priority/sort/builtin 是运营排布，
	// model_count 是「该厂商有几个在售模型」，都属于运营数据。
	//
	// 下发的是 **icon_url**（可渲染地址）而不是 icon（存储令牌）：令牌是编辑回填用的
	// 内部表示，门户拿它既画不出图、也没法判断该拼什么前缀。
	first := arr[0]
	if len(first) != 4 || first["id"] != "openai" || first["name"] != "OpenAI" || first["color"] != "#111" {
		t.Fatalf("供应商字段必须只有 id/name/color/icon_url: %+v", first)
	}
	if _, ok := first["icon"]; ok {
		t.Fatalf("门户不得透出图标令牌 icon（应只给 icon_url）: %+v", first)
	}
	iconURL, _ := first["icon_url"].(string)
	if !strings.Contains(iconURL, "/api/icons/b/openai-gpt?v=") {
		t.Fatalf("供应商图标地址必须由后端下发（含内容指纹）: %q", iconURL)
	}
	// 有请求域名时必须给绝对地址：门户是独立前端，相对路径会拼到门户自己的域名上。
	if !strings.HasPrefix(iconURL, "http://example.com/") {
		t.Fatalf("图标地址必须是基于请求域名的绝对地址: %q", iconURL)
	}
	for _, banned := range []string{"priority", "sort", "builtin", "model_count"} {
		if _, ok := first[banned]; ok {
			t.Fatalf("门户供应商列表不得透出 %s: %+v", banned, first)
		}
	}
	if arr[1]["id"] != "kuaishou" || arr[1]["name"] != "快手" {
		t.Fatalf("第 2 个供应商不符: %+v", arr[1])
	}
}

// TestPortalVendorsEmpty 空数据必须 200 + 空数组（禁止 500 / null）。
func TestPortalVendorsEmpty(t *testing.T) {
	withCatalogDB(t, catalogTestSchema+"_empty")
	w := callCatalogHandler(t, PortalVendors, nil)
	if w.Code != 200 {
		t.Fatalf("空态应 200，实际 %d", w.Code)
	}
	var arr []map[string]any
	if err := json.Unmarshal(decodeData(t, w), &arr); err != nil {
		t.Fatalf("vendors 应为数组: %v", err)
	}
	if len(arr) != 0 {
		t.Fatalf("空态应为空数组: %+v", arr)
	}
}

// TestGatewayModels 覆盖 OpenAI list 形态与 §4.5.7 pricing 扩展、默认模型标记。
func TestGatewayModels(t *testing.T) {
	gdb := withCatalogDB(t, catalogTestSchema+"_models")
	seedCatalog(t, gdb)
	setSysCfg(t, "modality_defaults", map[string]string{"image": "dall-e-3"})

	w := callCatalogHandler(t, GatewayModels, nil)
	var body struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析失败: %v (%s)", err, w.Body.String())
	}
	if body.Object != "list" {
		t.Fatalf("object 应为 list: %s", body.Object)
	}
	if len(body.Data) != 4 { // 禁用模型必须被排除
		t.Fatalf("模型数量不符（应仅含 enabled 的 4 个）: %d", len(body.Data))
	}
	byID := map[string]map[string]any{}
	for _, m := range body.Data {
		byID[m["id"].(string)] = m
	}

	g4 := byID["gpt-4o"]
	if g4 == nil || g4["object"] != "model" || g4["owned_by"] != "openai" {
		t.Fatalf("OpenAI 基础字段不符: %+v", g4)
	}
	p := g4["pricing"].(map[string]any)
	if p["billing_mode"] != "tokens" || p["currency"] != "USD" {
		t.Fatalf("计费方式/币种不符: %+v", p)
	}
	// 站点是人民币：¥1.25/1M = 1_250_000 微 → 按 7.2 折成美元微 = 173611。
	// 断言的是「换算后的美元微」，不是金额文本：这份结构是给客户端程序读的。
	prices := p["prices"].(map[string]any)
	if prices["input"] != float64(173611) || prices["output"] != float64(520833) {
		t.Fatalf("美元价换算不符: %+v", prices)
	}
	if _, ok := prices["call"]; ok {
		t.Fatalf("按量模型不应透出按次价 call: %+v", prices)
	}
	if p["default"] != true {
		t.Fatalf("default 应为 true: %+v", p)
	}

	if mini := byID["gpt-4o-mini"]; mini["pricing"].(map[string]any)["default"] != false {
		t.Fatalf("非默认模型 default 应为 false: %+v", mini)
	}

	d3 := byID["dall-e-3"]["pricing"].(map[string]any)
	if d3["billing_mode"] != "spec" {
		t.Fatalf("图像模型计费方式应为 spec: %+v", d3)
	}
	sp, ok := d3["spec_prices"].(map[string]any)
	if !ok || sp["kind"] != "per_image" {
		t.Fatalf("已配置 spec_prices 应透出: %+v", d3)
	}
	// 档位单价同样必须换成美元微：¥0.05/张 = 50000 微 → 6944 美元微。
	// 只换算 prices 不换算 spec_prices，会让同一份 pricing 里出现两种币种。
	tiers, _ := sp["tier_price"].(map[string]any)
	if tiers["1K"] != float64(6944) {
		t.Fatalf("规格价必须换算成美元微: %+v", sp)
	}
	if d3["default"] != true {
		t.Fatalf("image 默认模型应命中 modality_defaults: %+v", d3)
	}
	if byID["kling-v1"]["pricing"].(map[string]any)["default"] != true {
		t.Fatalf("video 默认应回落到该模态首个 enabled 模型: %+v", byID["kling-v1"])
	}
}

// TestGatewayModelDefaults 覆盖按模态默认 + allowed 按当前 KEY 白名单（§4.3.11 L403）。
func TestGatewayModelDefaults(t *testing.T) {
	gdb := withCatalogDB(t, catalogTestSchema+"_defaults")
	seedCatalog(t, gdb)
	setSysCfg(t, "modality_defaults", map[string]string{"image": "dall-e-3"})

	k := &model.APIKey{ID: 1, UserID: 1, ModelsAllowed: model.MustJSON([]string{"gpt-4o"})}
	w := callCatalogHandler(t, GatewayModelDefaults, func(c *gin.Context) { c.Set("api_key_record", k) })
	data := decodeMap(t, decodeData(t, w))

	chat := data["chat"].(map[string]any)
	if chat["model"] != "gpt-4o" || chat["allowed"] != true {
		t.Fatalf("chat 默认/白名单不符: %+v", chat)
	}
	if img := data["image"].(map[string]any); img["model"] != "dall-e-3" || img["allowed"] != false {
		t.Fatalf("未在白名单内的默认模型 allowed 应为 false: %+v", img)
	}
	if vid := data["video"].(map[string]any); vid["model"] != "kling-v1" || vid["allowed"] != false {
		t.Fatalf("video 默认不符: %+v", vid)
	}
	for _, m := range []string{"audio", "embedding", "rerank", "moderation"} {
		if e := data[m].(map[string]any); e["model"] != "" || e["allowed"] != false {
			t.Fatalf("无默认模型的模态应为空且未授权: %s=%+v", m, e)
		}
	}
}

// TestGatewayModelDefaultsAuth 鉴权缺失时按 API KEY 拒绝。
func TestGatewayModelDefaultsAuth(t *testing.T) {
	w := callCatalogHandler(t, GatewayModelDefaults, nil)
	if w.Code != 401 {
		t.Fatalf("缺 KEY 应 401，实际 %d: %s", w.Code, w.Body.String())
	}
}

// TestPortalDefaults 覆盖 any_key_allowed = 用户任一 KEY 白名单含该模型（§4.3.8 L474）。
func TestPortalDefaults(t *testing.T) {
	gdb := withCatalogDB(t, catalogTestSchema+"_portal")
	seedCatalog(t, gdb)
	setSysCfg(t, "modality_defaults", map[string]string{"image": "dall-e-3"})

	keys := []model.APIKey{
		{ID: 1, UserID: 1, Status: 1, KeyHash: "h1", ModelsAllowed: model.MustJSON([]string{"gpt-4o"})},
		{ID: 2, UserID: 1, Status: 1, KeyHash: "h2", ModelsAllowed: model.MustJSON([]string{"other-model"})},
		{ID: 3, UserID: 9, Status: 1, KeyHash: "h3", ModelsAllowed: nil}, // 他人 KEY，不得计入
	}
	for i := range keys {
		if err := gdb.Create(&keys[i]).Error; err != nil {
			t.Fatalf("造 KEY 失败: %v", err)
		}
	}

	w := callCatalogHandler(t, PortalDefaults, func(c *gin.Context) { c.Set("user_id", uint64(1)) })
	data := decodeMap(t, decodeData(t, w))
	if chat := data["chat"].(map[string]any); chat["model"] != "gpt-4o" || chat["any_key_allowed"] != true {
		t.Fatalf("任一 KEY 命中白名单应为 true: %+v", chat)
	}
	if img := data["image"].(map[string]any); img["model"] != "dall-e-3" || img["any_key_allowed"] != false {
		t.Fatalf("无 KEY 命中应为 false: %+v", img)
	}
}

// TestPortalDefaultsNoKeys 用户无 KEY 时全部未授权。
func TestPortalDefaultsNoKeys(t *testing.T) {
	gdb := withCatalogDB(t, catalogTestSchema+"_portal_nokey")
	seedCatalog(t, gdb)

	w := callCatalogHandler(t, PortalDefaults, func(c *gin.Context) { c.Set("user_id", uint64(2)) })
	data := decodeMap(t, decodeData(t, w))
	if chat := data["chat"].(map[string]any); chat["model"] != "gpt-4o" || chat["any_key_allowed"] != false {
		t.Fatalf("无 KEY 用户应全部未授权: %+v", chat)
	}
}

// TestPortalContentAbout 覆盖正文按空行分段（§4.3.7 L486）。
func TestPortalContentAbout(t *testing.T) {
	setSysCfg(t, "about_content", "灵动 API 是一站式 LLM 网关。\n\n\n我们提供统一鉴权、计费与路由。\n")
	w := callCatalogHandler(t, PortalContentAbout, nil)
	data := decodeMap(t, decodeData(t, w))
	paras, _ := data["paragraphs"].([]any)
	if len(paras) != 2 {
		t.Fatalf("应按空行分成 2 段: %+v", paras)
	}
	if paras[0] != "灵动 API 是一站式 LLM 网关。" || paras[1] != "我们提供统一鉴权、计费与路由。" {
		t.Fatalf("分段内容不符: %+v", paras)
	}
}

// TestPortalContentAgreement 覆盖协议正文分段 + 生效日期单独下发（§4.3.7 L487）。
func TestPortalContentAgreement(t *testing.T) {
	setSysCfg(t, "agreement_content",
		"欢迎使用灵动 API。\n\n生效日期：2026-10-01\n\n第一条 服务说明\n\n第二条 计费规则")
	w := callCatalogHandler(t, PortalContentAgreement, nil)
	data := decodeMap(t, decodeData(t, w))
	if data["effective_date"] != "2026-10-01" {
		t.Fatalf("生效日期应单独下发: %+v", data)
	}
	paras, _ := data["paragraphs"].([]any)
	if len(paras) != 3 {
		t.Fatalf("生效日期段必须从正文剥离，应剩 3 段: %+v", paras)
	}
	for _, p := range paras {
		if strings.HasPrefix(p.(string), "生效日期") {
			t.Fatalf("正文不得残留生效日期段: %+v", paras)
		}
	}
}

// TestPortalContentEmpty 空内容返回空数组（200）。
func TestPortalContentEmpty(t *testing.T) {
	setSysCfg(t, "about_content", "")
	w := callCatalogHandler(t, PortalContentAbout, nil)
	data := decodeMap(t, decodeData(t, w))
	paras, _ := data["paragraphs"].([]any)
	if len(paras) != 0 {
		t.Fatalf("空内容应返回空数组: %+v", paras)
	}
}

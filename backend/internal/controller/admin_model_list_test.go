package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/pricing"
)

// withModelListDB 在绑定测试基座上补 vendors（vendorList 从定价快照取，快照要查这张表）。
func withModelListDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb := withBindingDB(t)
	if err := gdb.AutoMigrate(&model.Vendor{}); err != nil {
		t.Fatalf("迁移 vendors 失败: %v", err)
	}
	if err := gdb.Create(&model.Vendor{ID: "openai", Name: "OpenAI"}).Error; err != nil {
		t.Fatalf("造供应商失败: %v", err)
	}
	// 快照是全局单例：先失效，结束时再失效一次，避免把已 DROP 的 schema 留在缓存里
	// 让后续包级测试读到脏数据。
	pricing.InvalidatePricingCache()
	t.Cleanup(pricing.InvalidatePricingCache)
	return gdb
}

// TestListModelsFiltersByChannelAndEnabled 覆盖「模型服务」右栏的两个查询维度。
//
// 为什么这两个参数必须由后端过滤而不是前端筛：
//   - 渠道下可能有几百个模型，前端全量拉再筛会让首屏等一次全表；
//   - `channel_id` 的语义是「存在一条绑定（**不看绑定启停**）」——
//     若把它写成「绑定也启用才算」，停用一条绑定模型会从右栏凭空消失，
//     运营会以为解绑成功，实际还在库里。
func TestListModelsFiltersByChannelAndEnabled(t *testing.T) {
	gdb := withModelListDB(t)

	bound := model.Model{ID: "m-bound", DisplayName: "已绑定", Vendor: "openai", Modality: "chat", Enabled: true}
	other := model.Model{ID: "m-other", DisplayName: "未绑定", Vendor: "openai", Modality: "chat", Enabled: false}
	for _, m := range []*model.Model{&bound, &other} {
		if err := gdb.Select("*").Create(m).Error; err != nil {
			t.Fatalf("造模型失败: %v", err)
		}
	}
	// `enabled` 带 default:true，Create 会把零值换成列默认值（`Select("*")` 也拦不住，
	// 见铁律「GORM Create 把零值+default 列换成列默认值」）。要造停用模型只能再补一次 UPDATE。
	if err := gdb.Model(&model.Model{}).Where("id = ?", "m-other").Update("enabled", false).Error; err != nil {
		t.Fatalf("停用模型失败: %v", err)
	}
	ch := makeChannel(t, gdb, "右栏渠道")
	// 绑定本身设为**停用**：正好验证「channel_id 不看绑定启停」。
	if err := gdb.Table("model_bindings").Create(map[string]any{
		"model_id": "m-bound", "channel_id": ch.ID, "upstream_model": "up-bound",
		"weight": 2, "priority": 3, "enabled": false,
	}).Error; err != nil {
		t.Fatalf("造绑定失败: %v", err)
	}
	pricing.InvalidatePricingCache()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/models", ListAdminModels)
	type bindJSON struct {
		UpstreamModel string `json:"upstream_model"`
		Weight        int    `json:"weight"`
		Priority      int    `json:"priority"`
		Enabled       bool   `json:"enabled"`
	}
	type itemJSON struct {
		ID      string    `json:"id"`
		Enabled bool      `json:"enabled"`
		Binding *bindJSON `json:"binding"`
	}
	// 平台信封：{code,message,data:{items,total,...}}
	get := func(q string) []itemJSON {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/models"+q, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("列表应 200，实际 %d：%s", w.Code, w.Body.String())
		}
		var body struct {
			Data struct {
				Items []itemJSON `json:"items"`
				Total int        `json:"total"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析响应失败: %v（原文 %s）", err, w.Body.String())
		}
		if body.Data.Items == nil {
			t.Fatalf("items 必须是数组，不能是 null（原文 %s）", w.Body.String())
		}
		return body.Data.Items
	}

	// ① 按渠道过滤：只剩已绑定的那个，且带出这条绑定的全部字段
	items := get("?channel_id=" + strconv.FormatUint(ch.ID, 10))
	if len(items) != 1 || items[0].ID != "m-bound" {
		t.Fatalf("channel_id 应只返回已绑定的模型，实际 %+v", items)
	}
	b := items[0].Binding
	if b == nil {
		t.Fatal("带 channel_id 时必须回带 binding（右栏要显示上游名/权重/启停）")
	}
	if b.UpstreamModel != "up-bound" || b.Weight != 2 || b.Priority != 3 || b.Enabled {
		t.Fatalf("binding 内容不符: %+v", b)
	}

	// ② 渠道 + 模型启用态：停用的 m-other 被 enabled=true 排除，结果仍是空
	if got := get("?channel_id=" + strconv.FormatUint(ch.ID, 10) + "&enabled=false"); len(got) != 0 {
		t.Fatalf("m-bound 是启用的，enabled=false 应为空，实际 %+v", got)
	}

	// ③ 只按模型启用态：应只剩 m-bound
	got := get("?enabled=true")
	if len(got) != 1 || got[0].ID != "m-bound" {
		t.Fatalf("enabled=true 应只返回启用的模型，实际 %+v", got)
	}
	if got[0].Binding != nil {
		t.Fatal("不带 channel_id 时不应下发 binding（没有渠道上下文，binding 无意义）")
	}

	// ④ 非法参数明确 400，而不是静默当成"不过滤"
	for _, q := range []string{"?channel_id=abc", "?enabled=maybe"} {
		req := httptest.NewRequest(http.MethodGet, "/models"+q, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d", q, w.Code)
		}
	}
}

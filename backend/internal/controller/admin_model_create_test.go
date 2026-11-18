package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
)

/**
 * 创建模型时附带一条初始绑定（`initial_binding`）的契约。
 *
 * 这个字段存在的唯一场景：「在某个渠道下新建模型」。没有它，运营建完模型还要回到
 * 另一处把它绑上来，而那份列表是**按渠道过滤**的 —— 新模型根本不在里面，
 * 于是"我明明建了却看不见"。
 *
 * 两条必须钉住的语义：
 *  1. 建模型与建绑定在**同一个事务**里：任一失败整笔回滚，不留下"模型存在但不可路由"
 *     的半截状态（它在目录里会显示为在售，而每次调用必然 503）；
 *  2. 初始绑定必须是启用的：`enabled=false` 的新建请求要 400 并说清原因，
 *     而不是建出一个"有绑定但永远选不中"的模型。
 */
func TestCreateModelWithInitialBinding(t *testing.T) {
	gdb := withModelListDB(t)
	ch := model.Channel{Name: "主渠道", AdapterID: "openai", BaseURL: "https://api.openai.com", Status: 1}
	if err := gdb.Create(&ch).Error; err != nil {
		t.Fatalf("造渠道失败: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/models", CreateAdminModel)

	post := func(body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/models", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	// ① 正常：模型 + 绑定一起落库
	code, raw := post(`{
		"id":"m-new","display_name":"新模型","vendor":"openai","modality":"chat",
		"billing_mode":"tokens","input_price":1000000,"endpoints":["chat/completions"],
		"initial_binding":{"channel_id":` + strconv.FormatUint(ch.ID, 10) + `,"upstream_model":"m-new-up","weight":3,"enabled":true}
	}`)
	if code != http.StatusOK {
		t.Fatalf("创建应 200，实际 %d：%s", code, raw)
	}
	var got struct {
		Data struct {
			Item struct {
				ID       string `json:"id"`
				Bindings int64  `json:"bindings"`
			} `json:"item"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("解析响应失败: %v（原文 %s）", err, raw)
	}
	if got.Data.Item.Bindings != 1 {
		t.Fatalf("返回值里的绑定数应为 1，实际 %d（%s）", got.Data.Item.Bindings, raw)
	}
	var b model.ModelBinding
	if err := gdb.Where("model_id = ? AND channel_id = ?", "m-new", ch.ID).Take(&b).Error; err != nil {
		t.Fatalf("应写入一条绑定: %v", err)
	}
	if b.UpstreamModel != "m-new-up" || b.Weight != 3 {
		t.Fatalf("绑定字段不符：upstream=%q weight=%d", b.UpstreamModel, b.Weight)
	}

	// ② 上游名留空：**后端拒绝**（不能猜上游叫什么），由前端补成模型 ID。
	// 这条钉子很重要：任何"后端帮你想一个默认值"的改动都会让 AI 提案里漏写的
	// 上游名静默变成模型 ID —— 那是一次看不见的错误路由。
	code, raw = post(`{
		"id":"m-same","display_name":"同名","vendor":"openai","modality":"chat",
		"billing_mode":"tokens","input_price":1000000,"endpoints":["chat/completions"],
		"initial_binding":{"channel_id":` + strconv.FormatUint(ch.ID, 10) + `,"upstream_model":"","weight":1,"enabled":true}
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("上游名为空应 400，实际 %d：%s", code, raw)
	}
	if !strings.Contains(raw, "上游模型名") {
		t.Fatalf("400 必须点名缺的是上游模型名，实际：%s", raw)
	}

	// ③ 初始绑定停用：整笔拒绝（400 + 人话），且不留下半个模型
	code, raw = post(`{
		"id":"m-off","display_name":"停用绑定","vendor":"openai","modality":"chat",
		"billing_mode":"tokens","input_price":1000000,"endpoints":["chat/completions"],
		"initial_binding":{"channel_id":` + strconv.FormatUint(ch.ID, 10) + `,"upstream_model":"m-off","weight":1,"enabled":false}
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("停用绑定应 400，实际 %d：%s", code, raw)
	}
	if !strings.Contains(raw, "绑定") {
		t.Fatalf("400 必须说清是绑定的问题，实际：%s", raw)
	}
	var cnt int64
	if err := gdb.Model(&model.Model{}).Where("id = ?", "m-off").Count(&cnt).Error; err != nil {
		t.Fatalf("查模型失败: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("事务应回滚：模型 m-off 不应存在")
	}

	// ④ 渠道不存在：同样是 400（不是 500），且模型不落库
	code, raw = post(`{
		"id":"m-noch","display_name":"无渠道","vendor":"openai","modality":"chat",
		"billing_mode":"tokens","input_price":1000000,"endpoints":["chat/completions"],
		"initial_binding":{"channel_id":999999,"upstream_model":"","weight":1,"enabled":true}
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("渠道不存在应 400，实际 %d：%s", code, raw)
	}
	if err := gdb.Model(&model.Model{}).Where("id = ?", "m-noch").Count(&cnt).Error; err != nil {
		t.Fatalf("查模型失败: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("事务应回滚：模型 m-noch 不应存在")
	}
}

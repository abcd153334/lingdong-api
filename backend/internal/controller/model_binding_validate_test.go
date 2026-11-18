package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// 本文件覆盖 B2-6「绑定校验」的四条规则与存量豁免。
//
// 为什么这些用例必须存在：绑定是**唯一**能把「模型」与「渠道」连起来的配置，
// 一旦允许把 chat 模型绑到只做视频的适配器上，表现是「保存成功、每次调用都 400 →
// 记渠道失败 → 熔断」，运营在渠道页看到的是「渠道被自动封禁」，
// 根本不会想到根因是模型绑定。保存期拦住是成本最低的位置。

// upstreamJSON 是绑定必填的上游模型名片段。
//
// 收敛后 `upstream_model` 是硬性必填（它是「上游真名」的唯一真相源），
// 所以每个构造 payload 的用例都必须带上它 —— 以前这里缺省也能保存。
const upstreamJSON = `"upstream_model":"upstream-x"`

// putBindings 以受控上下文调用**单条**绑定接口，返回状态码与响应体。
//
// 参数沿用整组时代的 `{"bindings":[{...}]}` 字面量，只是为了让下面 10 个用例的
// payload 原样保留：解析出第一条的 channel_id 当路径参数，其余字段作为 body。
// 单条接口以路径为准，所以必须把 channel_id 从 body 里摘掉 —— 留着它也不会被读，
// 但会让"渠道由路径决定"这条语义在测试里变得含糊。
func putBindings(t *testing.T, modelID string, payload string) (int, string) {
	t.Helper()
	var wrap struct {
		Bindings []map[string]any `json:"bindings"`
	}
	if err := json.Unmarshal([]byte(payload), &wrap); err != nil || len(wrap.Bindings) == 0 {
		t.Fatalf("用例 payload 必须是 {\"bindings\":[{...}]}: %s", payload)
	}
	first := wrap.Bindings[0]
	var chID uint64
	switch v := first["channel_id"].(type) {
	case float64:
		chID = uint64(v)
	case json.Number:
		n, _ := v.Int64()
		chID = uint64(n)
	}
	if chID == 0 {
		t.Fatalf("用例 payload 缺少 channel_id: %s", payload)
	}
	delete(first, "channel_id")
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("重新序列化失败: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PUT("/models/:id/bindings/:channelId", UpsertModelBinding)
	req := httptest.NewRequest(http.MethodPut,
		"/models/"+modelID+"/bindings/"+strconv.FormatUint(chID, 10), strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// mustCreateModel 造一个模型（Select("*") 保证 enabled/is_free 等零值列真的落库）。
func mustCreateModel(t *testing.T, gdb *gorm.DB, m model.Model) {
	t.Helper()
	// 端点必填：模型没有端点就没有任何接口，保存期会拒掉（见 modelcfg.CheckCoverage）。
	// 夹具补一个与模态相符的端点，否则测的是一个"存不进管理台"的非法状态。
	if len(m.Endpoints) == 0 {
		m.Endpoints = model.MustJSON([]string{defaultEndpointOf(m.Modality)})
	}
	if err := gdb.Select("*").Create(&m).Error; err != nil {
		t.Fatalf("造模型失败: %v", err)
	}
}

// defaultEndpointOf 给测试夹具挑一个与模态相符的端点写法。
func defaultEndpointOf(modality string) string {
	switch modality {
	case "image":
		return "images/generations"
	case "video":
		return "video/generations"
	case "audio":
		return "audio/speech"
	case "music":
		return "audio/music/generations"
	case "embedding":
		return "embeddings"
	case "rerank":
		return "rerank"
	case "moderation":
		return "moderations"
	default:
		return "chat/completions"
	}
}

// mustCreateChannel 造一个渠道并返回其 ID。
func mustCreateChannel(t *testing.T, gdb *gorm.DB, name, adapterID string, status int16) uint64 {
	t.Helper()
	ch := model.Channel{
		Name: name, AdapterID: adapterID, BaseURL: "https://api.example.com",
		Keys: model.MustJSON([]string{"enc"}), MultiKeyMode: "random",
		Groups: model.MustJSON([]string{"default"}),
		Weight: 1, Priority: 1, Status: status, AutoBan: true,
	}
	if err := gdb.Select("*").Create(&ch).Error; err != nil {
		t.Fatalf("造渠道失败: %v", err)
	}
	// status 列声明 default:1，GORM 对零值不写 INSERT 并回填默认值 ——
	// 「造一个停用渠道」必须再补一条 UPDATE，否则测出来永远是启用态。
	if status != 1 {
		if err := gdb.Model(&model.Channel{}).Where("id = ?", ch.ID).
			Update("status", status).Error; err != nil {
			t.Fatalf("置渠道状态失败: %v", err)
		}
	}
	return ch.ID
}

// TestBindingRejectsModalityMismatch 断言「适配器能力与模型模态不符」在保存期被拒。
//
// autodl 的 SupportsEndpoint 全 false、只做异步视频任务，因此它不能承接 chat 模型。
func TestBindingRejectsModalityMismatch(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{ID: "gpt-4o", Vendor: "openai", Modality: "chat", Enabled: true})
	chID := mustCreateChannel(t, gdb, "视频渠道", "autodl", 1)

	code, body := putBindings(t, "gpt-4o",
		`{"bindings":[{"channel_id":`+strconv.FormatUint(chID, 10)+`,"weight":1,`+upstreamJSON+`}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("模态不匹配应 400，实际 %d：%s", code, body)
	}
	if !strings.Contains(body, "不支持") || !strings.Contains(body, "视频渠道") {
		t.Fatalf("错误应点名渠道与模态，实际：%s", body)
	}
	// 校验失败不得留下半条绑定。
	var n int64
	if err := gdb.Model(&model.ModelBinding{}).Where("model_id = ?", "gpt-4o").Count(&n).Error; err != nil {
		t.Fatalf("统计绑定失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("校验失败不应落库任何绑定，实际 %d 条", n)
	}
}

// TestBindingAllowsTaskAdapterForVideoModel 断言异步任务适配器能绑视频模型。
//
// 判定路径是 TaskEndpointSupported 而不是 SupportsEndpoint：视频是异步任务，没有同步端点。
// 这条用例同时锁死「模态探针走 task 分支」这个细节——写错成同步端点会把所有视频渠道拒掉。
func TestBindingAllowsTaskAdapterForVideoModel(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{ID: "kling-v1", Vendor: "kuaishou", Modality: "video", BillingMode: model.BillingModeSpec, Enabled: true})
	chID := mustCreateChannel(t, gdb, "视频渠道", "autodl", 1)

	code, body := putBindings(t, "kling-v1",
		`{"bindings":[{"channel_id":`+strconv.FormatUint(chID, 10)+`,"weight":1,`+upstreamJSON+`}]}`)
	if code != http.StatusOK {
		t.Fatalf("视频模型绑异步适配器应 200，实际 %d：%s", code, body)
	}
	var row model.ModelBinding
	if err := gdb.Where("model_id = ?", "kling-v1").First(&row).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if row.ChannelID != chID {
		t.Fatalf("绑定渠道不符: %+v", row)
	}
}

// TestBindingRejectsDisabledChannelForNewBinding 断言新增绑定必须渠道启用。
func TestBindingRejectsDisabledChannelForNewBinding(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{ID: "gpt-4o", Vendor: "openai", Modality: "chat", Enabled: true})
	chID := mustCreateChannel(t, gdb, "停用渠道", "openai", 0)

	code, body := putBindings(t, "gpt-4o",
		`{"bindings":[{"channel_id":`+strconv.FormatUint(chID, 10)+`,"weight":1,`+upstreamJSON+`}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("停用渠道新增绑定应 400，实际 %d：%s", code, body)
	}
	if !strings.Contains(body, "停用") {
		t.Fatalf("错误应说明渠道已停用，实际：%s", body)
	}
}

// TestBindingKeepsExistingBindingOnDisabledChannel 断言存量豁免：
// 渠道停用后，模型绑定页仍能保存（否则运营连模型名都改不了）。
//
// 这是整页 PUT 的语义：前端每次都把当前全部绑定回传，
// 只要有一条历史绑定指向停用渠道就整体 400，等于把模型配置页锁死。
func TestBindingKeepsExistingBindingOnDisabledChannel(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{ID: "gpt-4o", Vendor: "openai", Modality: "chat", Enabled: true})
	chID := mustCreateChannel(t, gdb, "渠道A", "openai", 1)
	idStr := strconv.FormatUint(chID, 10)

	if code, body := putBindings(t, "gpt-4o",
		`{"bindings":[{"channel_id":`+idStr+`,"weight":1,`+upstreamJSON+`}]}`); code != http.StatusOK {
		t.Fatalf("首次绑定应 200，实际 %d：%s", code, body)
	}
	// 渠道被停用（运营在渠道页关闭，或自动封禁）。
	if err := gdb.Model(&model.Channel{}).Where("id = ?", chID).Update("status", 0).Error; err != nil {
		t.Fatalf("停用渠道失败: %v", err)
	}
	// 再保存一次（只改权重）必须放行 —— 存量豁免。
	code, body := putBindings(t, "gpt-4o",
		`{"bindings":[{"channel_id":`+idStr+`,"weight":5,"enabled":true,`+upstreamJSON+`}]}`)
	if code != http.StatusOK {
		t.Fatalf("存量绑定应豁免渠道状态校验，实际 %d：%s", code, body)
	}
	var row model.ModelBinding
	if err := gdb.Where("model_id = ? AND channel_id = ?", "gpt-4o", chID).First(&row).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if row.Weight != 5 {
		t.Fatalf("权重应更新为 5，实际 %d", row.Weight)
	}
}

// TestBindingEmbeddingModelOnOpenAIChannel 覆盖「模态 + 端点」双闸都通过的正路径。
func TestBindingEmbeddingModelOnOpenAIChannel(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{
		ID: "text-embedding-3", Vendor: "openai", Modality: "embedding", Enabled: true,
		Endpoints: model.MustJSON([]string{"embeddings"}),
	})
	chID := mustCreateChannel(t, gdb, "通用渠道", "openai", 1)
	body := `{"bindings":[{"channel_id":` + strconv.FormatUint(chID, 10) + `,"weight":1,` + upstreamJSON + `}]}`
	if code, resp := putBindings(t, "text-embedding-3", body); code != http.StatusOK {
		t.Fatalf("embedding 模型绑 openai 渠道应 200，实际 %d：%s", code, resp)
	}
}

// TestBindingUnknownEndpointNotBlocking 断言无法识别的端点写法不参与能力校验。
//
// 存量库里可能存在历史别名或运营备注（如 "openai-compat"）。硬拒会让「改个模型名」
// 都保存不了，代价远大于收益；真正不支持的端点会在运行期被模态闸门拦住。
func TestBindingUnknownEndpointNotBlocking(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{
		ID: "gpt-4o", Vendor: "openai", Modality: "chat", Enabled: true,
		Endpoints: model.MustJSON([]string{"openai-compat", "chat/completions"}),
	})
	chID := mustCreateChannel(t, gdb, "视频渠道", "autodl", 1)
	// chat/completions 本身就无法被 autodl 承接 → 仍应 400（证明已知端点仍在校验）。
	body := `{"bindings":[{"channel_id":` + strconv.FormatUint(chID, 10) + `,"weight":1,` + upstreamJSON + `}]}`
	if code, resp := putBindings(t, "gpt-4o", body); code != http.StatusBadRequest {
		t.Fatalf("已知端点不被承接应 400，实际 %d：%s", code, resp)
	}
}

// TestBindingRejectsEmptyUpstream 断言上游模型名必填。
//
// 为什么这是硬错误而不是告警：空 upstream_model 会让 relay 回落平台模型 ID、
// 多协议适配器再回落内置档案，而同步图像桥没有第三层兜底 —— 它会把 model:""
// 直接发给上游，得到 400 并把这记成渠道故障（一条健康渠道被熔断）。
// 这条规则此前只有 AI/建模型路径有，人工抽屉完全不校验。
func TestBindingRejectsEmptyUpstream(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{ID: "gpt-4o", Vendor: "openai", Modality: "chat", Enabled: true})
	chID := mustCreateChannel(t, gdb, "渠道A", "openai", 1)

	code, body := putBindings(t, "gpt-4o",
		fmt.Sprintf(`{"bindings":[{"channel_id":%d,"weight":1,"upstream_model":"  "}]}`, chID))
	if code != http.StatusBadRequest {
		t.Fatalf("空上游名应 400，实际 %d：%s", code, body)
	}
	if !strings.Contains(body, "上游模型名") {
		t.Fatalf("错误应点名 upstream_model，实际：%s", body)
	}
}

// TestBindingWarnsUnsupportedModel 断言「保存成功但给出告警」。
//
// 这是本次改造的核心闭环：多协议适配器（多米）在保存期就能判断某个模型 ID
// 能否被承接。不做硬拦截是因为绑定与渠道配置分属两个页面，合理的操作顺序可能相反；
// 但也不能静默 —— 否则运营只能等用户报障才知道模型用不了。
func TestBindingWarnsUnsupportedModel(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{ID: "zzz-9", Vendor: "duomi", Modality: "image", Enabled: true})
	chID := mustCreateChannel(t, gdb, "多米渠道", "duomiapi", 1)

	code, body := putBindings(t, "zzz-9",
		fmt.Sprintf(`{"bindings":[{"channel_id":%d,"weight":1,"upstream_model":"zzz-9"}]}`, chID))
	if code != http.StatusOK {
		t.Fatalf("预检失败不应阻断保存，实际 %d：%s", code, body)
	}
	if !strings.Contains(body, `"warnings":[{"channel_id"`) {
		t.Fatalf("未知模型应产出告警，实际：%s", body)
	}
	if !strings.Contains(body, "未知的多米模型") {
		t.Fatalf("告警应带适配器的原始原因，实际：%s", body)
	}
	// 数据确实落库（告警不是失败）。
	var n int64
	if err := gdb.Model(&model.ModelBinding{}).Where("model_id = ?", "zzz-9").Count(&n).Error; err != nil {
		t.Fatalf("统计绑定失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("告警不应阻断落库，实际 %d 条", n)
	}
}

// TestBindingPersistsPriority 断言 binding.priority 真的落库（P3-02）。
//
// 改造前 SaveModelBindings 把 Priority 硬编码成 1，选路也完全不读它，
// 运营在界面上填的优先级被静默丢弃。
func TestBindingPersistsPriority(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{ID: "gpt-4o", Vendor: "openai", Modality: "chat", Enabled: true})
	chID := mustCreateChannel(t, gdb, "渠道A", "openai", 1)
	idStr := strconv.FormatUint(chID, 10)

	code, body := putBindings(t, "gpt-4o",
		`{"bindings":[{"channel_id":`+idStr+`,"weight":1,"priority":7,`+upstreamJSON+`}]}`)
	if code != http.StatusOK {
		t.Fatalf("保存应 200，实际 %d：%s", code, body)
	}
	var row model.ModelBinding
	if err := gdb.Where("model_id = ? AND channel_id = ?", "gpt-4o", chID).First(&row).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if row.Priority != 7 {
		t.Fatalf("优先级应落库为 7，实际 %d", row.Priority)
	}

	// 未填（0）归一为 1，不能落成 0 —— 0 会让这条绑定永远排在最前。
	code, body = putBindings(t, "gpt-4o", `{"bindings":[{"channel_id":`+idStr+`,"weight":1,`+upstreamJSON+`}]}`)
	if code != http.StatusOK {
		t.Fatalf("保存应 200，实际 %d：%s", code, body)
	}
	// 必须用新变量：绑定是「硬删 + 重建」，行 ID 会变；复用 row 会带上旧主键做条件。
	var row2 model.ModelBinding
	if err := gdb.Where("model_id = ? AND channel_id = ?", "gpt-4o", chID).First(&row2).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if row2.Priority != 1 {
		t.Fatalf("未填优先级应归一为 1，实际 %d", row2.Priority)
	}
}

// TestBindingPriorityUpperBound 断言优先级有上限（否则单条绑定可以把其它层饿死）。
func TestBindingPriorityUpperBound(t *testing.T) {
	gdb := withBindingDB(t)
	mustCreateModel(t, gdb, model.Model{ID: "gpt-4o", Vendor: "openai", Modality: "chat", Enabled: true})
	chID := mustCreateChannel(t, gdb, "渠道A", "openai", 1)
	body := fmt.Sprintf(`{"bindings":[{"channel_id":%d,"weight":1,"priority":%d,%s}]}`, chID, 1001, upstreamJSON)
	code, resp := putBindings(t, "gpt-4o", body)
	if code != http.StatusBadRequest {
		t.Fatalf("超上限优先级应 400，实际 %d：%s", code, resp)
	}
	var env struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(resp), &env)
	if !strings.Contains(env.Message, "优先级") {
		t.Fatalf("错误文案应提到优先级，实际：%s", resp)
	}
}

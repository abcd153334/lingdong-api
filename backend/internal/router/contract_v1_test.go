package router_test

// 本文件是 /v1 协议面的**路由级契约测试**（G1）。
//
// 与其它用例的分工：service 层测的是"算得对不对"，这里测的是"发出去长什么样"——
// 状态码、顶层键集、错误体形状、SSE 帧。这些是接入方唯一能看见的东西，
// 而它们此前**一条断言都没有**：改一个状态码、少一个键，全量测试仍然全绿，
// 只有真实客户端会崩（而且往往崩在别人的生产里）。
//
// 每个用例都写清"改错了谁会疼"，不写"验证某某功能"。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/router"
	"lingdong-api/internal/service/apikey"
)

const contractV1Token = "sk-contract-v1-test-token"

// contractV1Env 起一套 /v1 用的最小环境：用户 + 一把真实哈希的 KEY + 任务表。
//
// 为什么用真 TokenAuth 而不是伪造 context：错误体形状（401 的 type/message、
// 缺 KEY 时的 code）全部由中间件写出，绕过它等于没测。
func contractV1Env(t *testing.T) (*gin.Engine, *model.APIKey, *gorm.DB) {
	t.Helper()
	gdb := authFlowDB(t)
	authFlowRedis(t)
	if err := gdb.AutoMigrate(&model.Task{}, &model.File{},
		&model.Model{}, &model.Channel{}, &model.ModelBinding{}, &model.Usage{},
		&model.Vendor{}); err != nil {
		t.Fatalf("迁移 /v1 所需表失败: %v", err)
	}
	u := model.User{Username: "contract-v1", Status: 1}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	k := model.APIKey{
		UserID: u.ID, Kind: model.KeyKindUser,
		KeyCipher: "x", KeyHash: apikey.Hash(contractV1Token),
		Name: "contract", Status: 1,
	}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}
	gin.SetMode(gin.TestMode)
	return router.New(), &k, gdb
}

// doV1 发一个 /v1 请求（可选带 KEY）。
func doV1(t *testing.T, r *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doAPI(t, r, method, path, token, body)
}

// v1Error 取出 /v1 错误体里的 error 对象。
//
// 直接解到 map：字段形状本身是被测对象，用结构体解会把"少一个键"变成零值通过。
func v1Error(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误体不是 JSON: %s", rec.Body.String())
	}
	if env.Error == nil {
		t.Fatalf("响应里没有 error 对象: %s", rec.Body.String())
	}
	return env.Error
}

// TestV1MissingKeyShape 守 401 的**形状**。
//
// 接入方（以及 Compat 客户端）按 `error.type` 分流：`new_api_error` 才被认成
// "平台侧鉴权失败"。写成 `authentication_error` 时，兼容客户端会走它自己的
// "重新登录"分支（它们以为对面是 OpenAI），而这一步在这里根本不存在。
// message 里的 `(request id: …)` 是用户报障时唯一能贴给客服的定位信息。
func TestV1MissingKeyShape(t *testing.T) {
	r, _, _ := contractV1Env(t)
	rec := doV1(t, r, http.MethodGet, "/v1/models", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺 KEY 必须 401，实际 %d %s", rec.Code, rec.Body.String())
	}
	e := v1Error(t, rec)
	if e["type"] != "new_api_error" {
		t.Fatalf("401 的 error.type 必须是 new_api_error，实际 %v", e["type"])
	}
	if e["code"] != "invalid_api_key" {
		t.Fatalf("401 的 error.code 必须是 invalid_api_key，实际 %v", e["code"])
	}
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "request id:") {
		t.Fatalf("401 的 message 必须带 request id（用户报障的唯一线索），实际 %q", msg)
	}
}

// TestV1ModelsListShape 守 `/v1/models` 的信封。
//
// 顶层 `success:true` 与 `object:"list"` 缺一个，OpenAI SDK 的 models.list()
// 就会解析不到分页（它按 object 判类型），而客户端只会显示"暂无模型"。
func TestV1ModelsListShape(t *testing.T) {
	r, _, _ := contractV1Env(t)
	rec := doV1(t, r, http.MethodGet, "/v1/models", contractV1Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("不是 JSON: %s", rec.Body.String())
	}
	if body["success"] != true {
		t.Fatalf("缺顶层 success:true（兼容协议的列表信封），实际 %v", body["success"])
	}
	if body["object"] != "list" {
		t.Fatalf("object 必须是 list，实际 %v", body["object"])
	}
	if _, ok := body["data"].([]any); !ok {
		t.Fatalf("data 必须是数组（空也要是 []），实际 %T", body["data"])
	}
}

// TestV1ModelRetrieveMissIs200 守「查不到模型给 200 + error」这条**反直觉**的契约。
//
// 这是兼容协议自己的缺陷（SDK 会把它解成零值对象而不是抛错），但既有客户端的
// 降级分支是按它写的：给 404 会让它们认定"接口版本不对"并去改写 base_url。
// 我们跟随它，但文档里必须写明。
func TestV1ModelRetrieveMissIs200(t *testing.T) {
	r, _, _ := contractV1Env(t)
	rec := doV1(t, r, http.MethodGet, "/v1/models/definitely-not-a-model", contractV1Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("未命中必须 200（兼容协议行为），实际 %d %s", rec.Code, rec.Body.String())
	}
	e := v1Error(t, rec)
	if e["code"] != "model_not_found" {
		t.Fatalf("code 必须是 model_not_found，实际 %v", e["code"])
	}
}

// TestV1UnknownPathIs404 守未知路径的兜底。
//
// 501 会让 SDK 认定"服务端不支持该能力"而放弃重试/降级；真实原因通常只是
// 路径拼错（把别家的端点贴过来了）。文案里必须带方法与路径 —— 否则用户只能
// 靠 diff base_url 猜。`no-store` 是必需的：CDN 会按启发式把 404 缓存下来，
// 于是"端点刚上线"在一部分用户那里仍然是 404。
func TestV1UnknownPathIs404(t *testing.T) {
	r, _, _ := contractV1Env(t)
	rec := doV1(t, r, http.MethodGet, "/v1/definitely-not-a-route", contractV1Token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知路径必须 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	e := v1Error(t, rec)
	msg, _ := e["message"].(string)
	if !strings.HasPrefix(msg, "Invalid URL (GET /v1/definitely-not-a-route") {
		t.Fatalf("文案必须是 `Invalid URL (METHOD /path)`，实际 %q", msg)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("未知路径必须压 Cache-Control: no-store，实际 %q", cc)
	}
}

// TestV1WrongMethodIs405 守「路径存在、方法不对」。
//
// 不开 HandleMethodNotAllowed 时 gin 会把它折进 NoRoute —— 于是"对
// /v1/models 用了 POST"被答成"地址不存在"，排查方向直接错到 base_url 上。
func TestV1WrongMethodIs405(t *testing.T) {
	r, _, _ := contractV1Env(t)
	rec := doV1(t, r, http.MethodPost, "/v1/models", contractV1Token, map[string]any{})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("方法错必须 405，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestV1ArtifactFailureCodes 守产物读取的四类失败各给一个码。
//
// 折叠成同一个码时，客户端与运维都分不清该做什么：
//   - 序号不存在（404）→ 重新取清单；
//   - 任务还没产出（409）→ 继续轮询（30 秒后产物就出来了，410 会让它当场放弃）；
//   - 已过期（410）→ 别再试；
//   - 存储后端缺失（503）→ 报障（补配置就能读）。
func TestV1ArtifactFailureCodes(t *testing.T) {
	r, k, gdb := contractV1Env(t)
	mk := func(publicID string, count int) *model.Task {
		task := &model.Task{
			PublicID: publicID, UserID: k.UserID, KeyID: k.ID,
			Model: "m", Modality: "video", Status: "succeeded", ArtifactCount: count,
		}
		if err := gdb.Create(task).Error; err != nil {
			t.Fatalf("造任务失败: %v", err)
		}
		return task
	}
	// 清单长度 0、但 artifact_count=1：曾经存在过、现在读不到 ⇒ 410。
	mk("task_01J9HA7JPQ9A0Z6JZ3V8M9W6Q1", 1)
	// artifact_count=0：从未转存 ⇒ 回源失败后 409（继续轮询）。
	mk("task_01J9HA7JPQ9A0Z6JZ3V8M9W6Q2", 0)

	cases := []struct {
		name   string
		path   string
		method string
		want   int
		code   string
	}{
		{"序号越界", "/v1/tasks/task_01J9HA7JPQ9A0Z6JZ3V8M9W6Q1/artifacts/5/content", http.MethodGet, 404, "artifact_not_found"},
		{"曾存在已清理", "/v1/tasks/task_01J9HA7JPQ9A0Z6JZ3V8M9W6Q1/artifacts/0/content", http.MethodGet, 410, "file_gone"},
		{"从未产出", "/v1/tasks/task_01J9HA7JPQ9A0Z6JZ3V8M9W6Q2/artifacts/0/content", http.MethodGet, 409, "artifact_not_ready"},
	}
	for _, tc := range cases {
		rec := doV1(t, r, tc.method, tc.path, contractV1Token, nil)
		if rec.Code != tc.want {
			t.Fatalf("%s：期望 %d，实际 %d %s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
		if e := v1Error(t, rec); e["code"] != tc.code {
			t.Fatalf("%s：期望码 %s，实际 %v", tc.name, tc.code, e["code"])
		}
	}
}

// TestV1ArtifactHeadHasNoBody 守 HEAD 的轻路径。
//
// 产物是 512MB 级视频，`curl -I` / CDN 预热 / 播放器预检都是常态。
// 走 GET 的读取路径时，net/http 只抑制响应体、存储侧仍会把整个对象读一遍 ——
// 一次探活等于一次全量下载，还白烧出站流量。
func TestV1ArtifactHeadHasNoBody(t *testing.T) {
	r, k, gdb := contractV1Env(t)
	task := &model.Task{
		PublicID: "task_01J9HA7JPQ9A0Z6JZ3V8M9W6Q3", UserID: k.UserID, KeyID: k.ID,
		Model: "m", Modality: "video", Status: "succeeded", ArtifactCount: 1,
	}
	if err := gdb.Create(task).Error; err != nil {
		t.Fatalf("造任务失败: %v", err)
	}
	rec := doV1(t, r, http.MethodHead, "/v1/tasks/"+task.PublicID+"/artifacts/0/content", contractV1Token, nil)
	// 清单为空（这份产物已不可读）：HEAD 与 GET 必须给同一个码，且不带 body。
	if rec.Code != http.StatusGone {
		t.Fatalf("HEAD 应与 GET 同码（410），实际 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD 不许有响应体，实际 %q", rec.Body.String())
	}
}

// TestV1VideoObjectShape 守 OpenAI Video 的四态与整数进度。
//
// 与 `/v1/tasks/{id}` 同一行数据、不同协议：状态词是 queued/in_progress/
// completed/failed，进度是**整数**（给 "50%" 会被 SDK 的宽松解析读成 0）。
func TestV1VideoObjectShape(t *testing.T) {
	r, k, gdb := contractV1Env(t)
	task := &model.Task{
		PublicID: "task_01J9HA7JPQ9A0Z6JZ3V8M9W6Q4", UserID: k.UserID, KeyID: k.ID,
		Model: "kling-v2", Modality: "video", Status: "running", ArtifactCount: 0,
	}
	if err := gdb.Create(task).Error; err != nil {
		t.Fatalf("造任务失败: %v", err)
	}
	rec := doV1(t, r, http.MethodGet, "/v1/videos/"+task.PublicID, contractV1Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("不是 JSON: %s", rec.Body.String())
	}
	if body["object"] != "video" {
		t.Fatalf("object 必须是 video，实际 %v", body["object"])
	}
	if body["status"] != "in_progress" {
		t.Fatalf("running 在视频词表里是 in_progress，实际 %v", body["status"])
	}
	if p, ok := body["progress"].(float64); !ok || p != 50 {
		t.Fatalf("progress 必须是整数 50，实际 %T %v", body["progress"], body["progress"])
	}
}

// TestV1TaskNotFoundShape 守任务查不到的码与状态。
//
// 不使用 task_ 前缀的 id（例如老客户端按数字拼）必须是 404 not_found，
// 而不是 400 参数错：前者让客户端去核对任务号，后者会让它去改请求体。
func TestV1TaskNotFoundShape(t *testing.T) {
	r, _, _ := contractV1Env(t)
	rec := doV1(t, r, http.MethodGet, "/v1/tasks/1024", contractV1Token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应先判非法任务号并给 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	e := v1Error(t, rec)
	if e["code"] != "not_found" {
		t.Fatalf("code 必须是 not_found，实际 %v", e["code"])
	}
}

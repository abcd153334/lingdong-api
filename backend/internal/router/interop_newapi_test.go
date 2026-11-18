package router_test

// 本文件是「new-api 存量应用能否无缝迁移」的**真机模拟测试**。
//
// 做法：起一个真实的假上游（httptest）+ 真实的 router（httptest.NewServer，
// 走真 TCP 与真 HTTP 报文），用**客户端实际会发的报文**逐个打我们这个网关，
// 记录状态码、Content-Type、顶层键、SSE 帧。结论由这些实测数据得出，
// 不靠读代码推断。
//
// 为什么必须真起服务：/v1 的形状有一半由中间件决定（鉴权信封、格式判定、
// 维护闸、限流），单测 handler 会把这半层全部绕过去。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
	"lingdong-api/internal/router"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/pricing"

	_ "lingdong-api/internal/relay/adapters"
)

const interopKey = "sk-interop-0123456789abcdef"

// interopRow 是每一格实测结果。
type interopRow struct {
	Name        string   `json:"name"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Status      int      `json:"status"`
	ContentType string   `json:"content_type"`
	Keys        []string `json:"keys"`
	Note        string   `json:"note"`
}

// fakeUpstream 是最小 OpenAI/Claude 兼容上游：只要网关能打通，回什么不重要，
// 但**形状要真**（客户端的解析分支按真形状写）。
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	sse := func(w http.ResponseWriter, frames ...string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, f := range frames {
			_, _ = io.WriteString(w, f+"\n\n")
			fl.Flush()
		}
	}
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "gpt-4o-mini", "object": "model", "created": 1700000000, "owned_by": "openai"},
		}})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, _ := body["stream"].(bool); s {
			sse(w,
				`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
				`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}]}`,
				`data: [DONE]`)
			return
		}
		writeJSON(w, map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1700000000, "model": "gpt-4o-mini",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "你好"},
				"finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 9, "completion_tokens": 4, "total_tokens": 13},
		})
	})
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "cmpl-1", "object": "text_completion", "choices": []any{
			map[string]any{"index": 0, "text": "hello", "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "model": "text-embedding-3-small",
			"data":  []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}}},
			"usage": map[string]any{"prompt_tokens": 4, "total_tokens": 4}})
	})
	mux.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"created": 1700000000,
			"data": []any{map[string]any{"url": "https://cdn.example.com/a.png", "revised_prompt": "a cat"}}})
	})
	mux.HandleFunc("/v1/audio/speech", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3fakeaudio"))
	})
	mux.HandleFunc("/v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"results": []any{map[string]any{"index": 0, "relevance_score": 0.9}}})
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, _ := body["stream"].(bool); s {
			sse(w,
				`event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress"}}`,
				`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"你好"}`,
				`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`)
			return
		}
		writeJSON(w, map[string]any{"id": "resp_1", "object": "response", "status": "completed",
			"model": "gpt-4o-mini", "output": []any{}})
	})
	// compact：Responses 家族的上下文压缩。响应形状与 /v1/responses 相同，
	// usage 用 Responses 的字段名（input_tokens 已含缓存），正好覆盖网关的语义归一。
	mux.HandleFunc("/v1/responses/compact", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"id": "resp_compact_1", "object": "response.compaction", "status": "completed",
			"model":  "gpt-4o-mini",
			"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "…"}}}},
			"usage": map[string]any{
				"input_tokens": 120, "output_tokens": 30, "total_tokens": 150,
				"input_tokens_details":  map[string]any{"cached_tokens": 40},
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			},
		})
	})
	mux.HandleFunc("/v1/video/generations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "upstream-task-1", "task_id": "upstream-task-1",
			"status": "queued", "model": "sora-1"})
	})
	mux.HandleFunc("/v1/video/generations/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "upstream-task-1", "status": "running"})
	})
	mux.HandleFunc("/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"text": "你好"})
	})
	mux.HandleFunc("/v1/images/edits", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"created": 1700000000,
			"data": []any{map[string]any{"url": "https://cdn.example.com/b.png"}}})
	})
	mux.HandleFunc("/v1/moderations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "modr-1", "model": "omni-moderation",
			"results": []any{map[string]any{"flagged": false}}})
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "msg_1", "type": "message", "role": "assistant",
			"model": "gpt-4o-mini", "content": []any{map[string]any{"type": "text", "text": "你好"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 5, "output_tokens": 3}})
	})
	return httptest.NewServer(mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// interopEnv 造一套能真正跑通同步调用的环境，并返回已监听端口的网关地址。
func interopEnv(t *testing.T) (string, *http.Client) {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过真机互通测试")
	}
	const schema = "ld_test_interop"
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	_ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE").Error
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), silent)
	if err != nil {
		t.Fatalf("连接 schema 失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		pricing.InvalidatePricingCache()
		if sqlDB, e := gdb.DB(); e == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE").Error
		if sqlDB, e := admin.DB(); e == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	if err := db.AutoMigrate(gdb); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 假上游在 127.0.0.1：默认 SSRF 策略会拒绝私网/回环地址（这是对的，
	// 生产环境不该允许）。测试里显式打开，否则所有同步调用都会 503，
	// 而 503 的原因与"互通性"无关。
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")

	up := fakeUpstream(t)
	t.Cleanup(up.Close)

	u := model.User{Username: "interop", Status: 1, Balance: 100_000_000, Group: "default"}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, Kind: model.KeyKindUser, KeyCipher: "x",
		KeyHash: apikey.Hash(interopKey), Name: "interop", Status: 1}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}
	keys, err := channelkeys.Encrypt([]string{"sk-upstream-test"})
	if err != nil {
		t.Fatalf("加密渠道密钥失败: %v", err)
	}
	ch := model.Channel{Name: "interop-upstream", AdapterID: "openai", BaseURL: up.URL,
		Keys: keys, MultiKeyMode: "random", Groups: model.MustJSON([]string{"default"}),
		Weight: 1, Priority: 1, Status: 1, AutoBan: true}
	if err := gdb.Select("*").Create(&ch).Error; err != nil {
		t.Fatalf("造渠道失败: %v", err)
	}
	in, out, cache, call := int64(150), int64(600), int64(30), int64(2000)
	models := []model.Model{
		{ID: "gpt-4o-mini", DisplayName: "GPT-4o mini", Vendor: "openai", Modality: "chat",
			BillingMode: model.BillingModeTokens, Enabled: true,
			Prices: model.Prices{Input: &in, Output: &out, Cache: &cache}},
		{ID: "text-embedding-3-small", DisplayName: "Embedding", Vendor: "openai", Modality: "embedding",
			BillingMode: model.BillingModeTokens, Enabled: true,
			Prices: model.Prices{Input: &in}},
		{ID: "dall-e-3", DisplayName: "DALL·E 3", Vendor: "openai", Modality: "image",
			BillingMode: model.BillingModeCall, Enabled: true,
			Prices: model.Prices{Call: &call}},
		{ID: "tts-1", DisplayName: "TTS", Vendor: "openai", Modality: "audio",
			BillingMode: model.BillingModeCall, Enabled: true,
			Prices: model.Prices{Call: &call}},
		{ID: "bge-rerank", DisplayName: "Rerank", Vendor: "openai", Modality: "rerank",
			BillingMode: model.BillingModeCall, Enabled: true,
			Prices: model.Prices{Call: &call}},
		{ID: "omni-moderation", DisplayName: "Moderation", Vendor: "openai", Modality: "moderation",
			BillingMode: model.BillingModeCall, Enabled: true,
			Prices: model.Prices{Call: &call}},
		{ID: "sora-1", DisplayName: "Sora", Vendor: "openai", Modality: "video",
			BillingMode: model.BillingModeSpec, Enabled: true,
			SpecPrices: model.SpecPrices{Kind: "per_video",
				SecondPrice: map[string]int64{"720p": 5000}, MaxSeconds: int64Ptr(5),
				ResolutionFixed: "720p"}},
	}
	for i := range models {
		if err := gdb.Select("*").Create(&models[i]).Error; err != nil {
			t.Fatalf("造模型 %s 失败: %v", models[i].ID, err)
		}
		b := model.ModelBinding{ModelID: models[i].ID, ChannelID: ch.ID, UpstreamModel: models[i].ID,
			Weight: 1, Priority: 1, Enabled: true}
		if err := gdb.Select("*").Create(&b).Error; err != nil {
			t.Fatalf("造绑定失败: %v", err)
		}
	}
	pricing.InvalidatePricingCache()

	gin.SetMode(gin.TestMode)
	srv := httptest.NewServer(router.New())
	t.Cleanup(srv.Close)
	return srv.URL, &http.Client{Timeout: 20 * time.Second}
}

// doInterop 发请求并抓取形状（不是一个断言一个 t.Fatalf，而是记录后统一出表）。
func doInterop(t *testing.T, base string, c *http.Client, name, method, path string,
	headers map[string]string, body any) interopRow {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		if raw, ok := body.([]byte); ok {
			rdr = bytes.NewReader(raw) // multipart 等已编码好的原身体
		} else {
			raw, _ := json.Marshal(body)
			rdr = bytes.NewReader(raw)
		}
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return interopRow{Name: name, Method: method, Path: path, Status: 0, Note: "请求失败: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	row := interopRow{Name: name, Method: method, Path: path, Status: resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type")}
	if strings.HasPrefix(row.ContentType, "text/event-stream") {
		row.Keys = sseFrameKinds(raw)
		row.Note = fmt.Sprintf("%d 字节", len(raw))
		return row
	}
	if strings.HasPrefix(row.ContentType, "audio/") || strings.HasPrefix(row.ContentType, "image/") {
		row.Note = fmt.Sprintf("%d 字节二进制", len(raw))
		return row
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		row.Keys = sortedKeys(obj)
	}
	row.Note = truncate(strings.TrimSpace(string(raw)), 160)
	return row
}

// sseFrameKinds 汇总流里出现过的事件名与 data 首帧（判定是否 [DONE] 收尾）。
func sseFrameKinds(raw []byte) []string {
	var kinds []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "event:"):
			k := "event:" + strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			if !seen[k] {
				seen[k] = true
				kinds = append(kinds, k)
			}
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if d == "[DONE]" {
				if !seen["[DONE]"] {
					seen["[DONE]"] = true
					kinds = append(kinds, "[DONE]")
				}
			} else if !seen["data(JSON)"] {
				seen["data(JSON)"] = true
				kinds = append(kinds, "data(JSON)")
			}
		}
	}
	return kinds
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestNewAPIInterop 是本轮结论的**唯一来源**：跑真实请求，写实测表。
func TestNewAPIInterop(t *testing.T) {
	base, c := interopEnv(t)
	auth := map[string]string{"Authorization": "Bearer " + interopKey}
	jsonCT := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + interopKey}

	cases := []struct {
		model  string
		method string
		path   string
		hdrs   map[string]string
		body   any
	}{
		{"01 OpenAI SDK · 模型列表", http.MethodGet, "/v1/models", auth, nil},
		{"02 模型列表 · ?key= 鉴权（Gemini 习惯）", http.MethodGet, "/v1/models?key=" + interopKey, nil, nil},
		{"03 模型列表 · x-goog-api-key", http.MethodGet, "/v1/models", map[string]string{"x-goog-api-key": interopKey}, nil},
		{"04 OpenAI SDK · 单模型查询", http.MethodGet, "/v1/models/gpt-4o-mini", auth, nil},
		{"05 单模型查询 · 不存在的模型", http.MethodGet, "/v1/models/no-such-model", auth, nil},
		{"06 对话补全（非流式）", http.MethodPost, "/v1/chat/completions", jsonCT,
			map[string]any{"model": "gpt-4o-mini", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"07 对话补全（流式 SSE）", http.MethodPost, "/v1/chat/completions", jsonCT,
			map[string]any{"model": "gpt-4o-mini", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"08 旧版补全 /v1/completions", http.MethodPost, "/v1/completions", jsonCT,
			map[string]any{"model": "gpt-4o-mini", "prompt": "hi"}},
		{"09 向量 /v1/embeddings", http.MethodPost, "/v1/embeddings", jsonCT,
			map[string]any{"model": "text-embedding-3-small", "input": "hi"}},
		{"10 出图 /v1/images/generations", http.MethodPost, "/v1/images/generations", jsonCT,
			map[string]any{"model": "dall-e-3", "prompt": "a cat", "n": 1, "size": "1024x1024"}},
		{"11 语音 /v1/audio/speech", http.MethodPost, "/v1/audio/speech", jsonCT,
			map[string]any{"model": "tts-1", "input": "hi", "voice": "alloy"}},
		{"12 重排 /v1/rerank", http.MethodPost, "/v1/rerank", jsonCT,
			map[string]any{"model": "bge-rerank", "query": "hi", "documents": []string{"a", "b"}}},
		{"13 Claude SDK · /v1/messages", http.MethodPost, "/v1/messages",
			map[string]string{"Content-Type": "application/json", "x-api-key": interopKey, "anthropic-version": "2023-06-01"},
			map[string]any{"model": "gpt-4o-mini", "max_tokens": 64,
				"messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"14 Claude · count_tokens", http.MethodPost, "/v1/messages/count_tokens",
			map[string]string{"Content-Type": "application/json", "x-api-key": interopKey, "anthropic-version": "2023-06-01"},
			map[string]any{"model": "gpt-4o-mini", "messages": []any{}}},
		{"15 Gemini · /v1beta/models", http.MethodGet, "/v1beta/models", map[string]string{"x-goog-api-key": interopKey}, nil},
		{"16 Gemini · generateContent", http.MethodPost, "/v1beta/models/gpt-4o-mini:generateContent",
			map[string]string{"Content-Type": "application/json", "x-goog-api-key": interopKey},
			map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]any{"text": "hi"}}}}}},
		{"17 Responses API", http.MethodPost, "/v1/responses", jsonCT,
			map[string]any{"model": "gpt-4o-mini", "input": "hi"}},
		{"18 Responses 查询", http.MethodGet, "/v1/responses/resp_1", auth, nil},
		{"19 图片编辑 /v1/edits", http.MethodPost, "/v1/edits", jsonCT,
			map[string]any{"model": "dall-e-3", "prompt": "x", "image": "https://a/b.png"}},
		{"20 Files 列表", http.MethodGet, "/v1/files", auth, nil},
		{"21 视频提交 /v1/video/generations", http.MethodPost, "/v1/video/generations", jsonCT,
			map[string]any{"model": "gpt-4o-mini", "prompt": "x"}},
		{"22 视频读面 /v1/videos/{id}", http.MethodGet, "/v1/videos/task_01J9HA7JPQ9A0Z6JZ3V8M9W6PZ", auth, nil},
		{"23 任务列表 /v1/tasks", http.MethodGet, "/v1/tasks", auth, nil},
		{"24 账户用量 /v1/dashboard/billing/subscription", http.MethodGet, "/v1/dashboard/billing/subscription", auth, nil},
		{"25 账户用量 /v1/dashboard/billing/usage", http.MethodGet, "/v1/dashboard/billing/usage", auth, nil},
		{"26 对话用量 /v1/usage", http.MethodGet, "/v1/usage", auth, nil},
		{"27 未知路径", http.MethodGet, "/v1/definitely-not-a-route", auth, nil},
		{"28 无凭证", http.MethodGet, "/v1/models", nil, nil},
		{"29 错误凭证", http.MethodGet, "/v1/models", map[string]string{"Authorization": "Bearer sk-wrong"}, nil},
		{"30 方法错（GET 打 POST 端点）", http.MethodGet, "/v1/chat/completions", auth, nil},
		{"31 Responses 流式", http.MethodPost, "/v1/responses", jsonCT,
			map[string]any{"model": "gpt-4o-mini", "input": "hi", "stream": true}},
		{"32 视频提交 /v1/video/generations", http.MethodPost, "/v1/video/generations", jsonCT,
			map[string]any{"model": "sora-1", "prompt": "a cat", "duration": 5, "size": "1280x720"}},
		{"33 Sora 提交 /v1/videos", http.MethodPost, "/v1/videos", jsonCT,
			map[string]any{"model": "sora-1", "prompt": "a cat", "duration": 5, "size": "1280x720"}},
		{"34 语音转写（multipart）", http.MethodPost, "/v1/audio/transcriptions",
			map[string]string{"Content-Type": multiType, "Authorization": "Bearer " + interopKey},
			multiBody("tts-1")},
		{"35 图片编辑（multipart）", http.MethodPost, "/v1/images/edits",
			map[string]string{"Content-Type": multiType, "Authorization": "Bearer " + interopKey},
			multiBody("dall-e-3")},
		{"36 内容审核 /v1/moderations", http.MethodPost, "/v1/moderations", jsonCT,
			map[string]any{"model": "omni-moderation", "input": "hi"}},
		{"37 DELETE 模型（new-api 501）", http.MethodDelete, "/v1/models/gpt-4o-mini", auth, nil},
		{"38 Midjourney /mj/submit/imagine", http.MethodPost, "/mj/submit/imagine", jsonCT,
			map[string]any{"prompt": "a cat"}},
		{"39 Alpha 搜索", http.MethodPost, "/v1/alpha/search", jsonCT,
			map[string]any{"model": "gpt-4o-mini", "query": "hi"}},
		{"40 Responses 压缩", http.MethodPost, "/v1/responses/compact", jsonCT,
			map[string]any{"model": "gpt-4o-mini", "input": "hi"}},
		// 遗留路径 + **OpenAI 形状**的请求体（老客户端的写法）；模型必须是 embedding 模态，
		// 否则会被模态闸门按"这个模型不能做向量化"正确拒绝（那是另一条用例的事）。
		{"41 Gemini 风格 embeddings", http.MethodPost, "/v1/engines/text-embedding-3-small/embeddings", jsonCT,
			map[string]any{"input": "hi"}},
		// Gemini 形状的向量化请求体：content.parts[].text → input。
		{"56 Gemini embedContent", http.MethodPost, "/v1beta/models/text-embedding-3-small:embedContent",
			map[string]string{"Content-Type": "application/json", "x-goog-api-key": interopKey},
			map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "hi"}}}}},
		{"47 计费 · 无 /v1 前缀", http.MethodGet, "/dashboard/billing/subscription", auth, nil},
		{"48 Realtime WebSocket", http.MethodGet, "/v1/realtime", auth, nil},
		{"42 Gemini countTokens", http.MethodPost, "/v1beta/models/gpt-4o-mini:countTokens",
			map[string]string{"Content-Type": "application/json", "x-goog-api-key": interopKey},
			map[string]any{"contents": []any{}}},
		// ── 同一 URL 按鉴权头换形状（三类 SDK 会打同一个 /v1/models）──
		// Anthropic SDK 的 client.models.list() 带这两个头，按 created_at + first_id 解析。
		{"57 模型列表（Anthropic 头）", http.MethodGet, "/v1/models",
			map[string]string{"x-api-key": interopKey, "anthropic-version": "2023-06-01"}, nil},
		// google-genai 在 base_url 填了 /v1 时带 x-goog-api-key，按 models[] 解析。
		{"58 模型列表（Gemini 头）", http.MethodGet, "/v1/models",
			map[string]string{"x-goog-api-key": interopKey}, nil},
		// /v1/models/*path 的 Gemini 通配：base_url 填成 .../v1 的客户端会落到这里。
		{"59 /v1 下的 Gemini 通配", http.MethodPost, "/v1/models/gpt-4o-mini:generateContent", jsonCT,
			map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]any{"text": "hi"}}}}}},
		// 兼容层即使带 ?key= 也必须是 OpenAI 形状（它的存在前提就是 OpenAI SDK 只改 base_url）。
		{"60 兼容层带 ?key=", http.MethodGet, "/v1beta/openai/models?key=" + interopKey, nil, nil},
		// Gemini 侧的新增用例：流式必须带 alt=sse、未知动作要回 Gemini 形状的 404、
		// 兼容层 /v1beta/openai/* 必须保持 OpenAI 形状。
		{"49 Gemini 流式（alt=sse）", http.MethodPost, "/v1beta/models/gpt-4o-mini:streamGenerateContent?alt=sse",
			map[string]string{"Content-Type": "application/json", "x-goog-api-key": interopKey},
			map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]any{"text": "hi"}}}}}},
		{"50 Gemini 流式（缺 alt）", http.MethodPost, "/v1beta/models/gpt-4o-mini:streamGenerateContent",
			map[string]string{"Content-Type": "application/json", "x-goog-api-key": interopKey},
			map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]any{"text": "hi"}}}}}},
		{"51 Gemini 未知动作", http.MethodPost, "/v1beta/models/gpt-4o-mini:noSuchAction",
			map[string]string{"Content-Type": "application/json", "x-goog-api-key": interopKey}, map[string]any{}},
		{"52 Gemini 未知路径", http.MethodGet, "/v1beta/definitely-not-a-route",
			map[string]string{"x-goog-api-key": interopKey}, nil},
		{"53 Gemini 无凭证", http.MethodGet, "/v1beta/models", nil, nil},
		{"54 Gemini 兼容层模型列表", http.MethodGet, "/v1beta/openai/models",
			map[string]string{"Authorization": "Bearer " + interopKey}, nil},
		{"55 Gemini 兼容层 404 形状", http.MethodGet, "/v1beta/openai/nope",
			map[string]string{"Authorization": "Bearer " + interopKey}, nil},
	}

	rows := make([]interopRow, 0, len(cases))
	for _, tc := range cases {
		rows = append(rows, doInterop(t, base, c, tc.model, tc.method, tc.path, tc.hdrs, tc.body))
	}

	// 视频读面必须用**真实任务号**打：new-api 的 Sora 客户端是"提交→轮询→取内容"
	// 三步，随便编一个 id 只能测到 404 分支。
	if id := submitVideo(t, base, c, auth); id != "" {
		rows = append(rows,
			doInterop(t, base, c, "43 轮询 /v1/videos/{id}", http.MethodGet, "/v1/videos/"+id, auth, nil),
			doInterop(t, base, c, "44 轮询 /v1/video/generations/{id}", http.MethodGet, "/v1/video/generations/"+id, auth, nil),
			doInterop(t, base, c, "45 任务 /v1/tasks/{id}", http.MethodGet, "/v1/tasks/"+id, auth, nil),
			doInterop(t, base, c, "46 产物 /v1/tasks/{id}/artifacts", http.MethodGet, "/v1/tasks/"+id+"/artifacts", auth, nil),
		)
	}

	// 写一份可读的实测表（供人工复核；测试本身只负责产出事实，不做判定）。
	out := "| # | 场景 | 请求 | 状态 | Content-Type | 顶层键 / 帧 | 摘要 |\n|---|---|---|---|---|---|---|\n"
	for _, r := range rows {
		out += fmt.Sprintf("| %s | %s | %s | **%d** | %s | %s | %s |\n",
			strings.SplitN(r.Name, " ", 2)[0], strings.TrimSpace(strings.TrimPrefix(r.Name, strings.SplitN(r.Name, " ", 2)[0])),
			r.Method+" "+r.Path, r.Status, shortCT(r.ContentType),
			strings.Join(r.Keys, ", "), strings.ReplaceAll(r.Note, "|", "\\|"))
	}
	// 报告路径：显式给了 LD_INTEROP_REPORT 才写出去，否则写临时目录。
	//
	// 以前固定写 backend/outputs/，于是每跑一次测试就在工作区里长回一份 28MB 的产物目录，
	// 而且它和时间戳一起进 diff —— 让"交付物"和"测试副产物"混成一堆。
	path := os.Getenv("LD_INTEROP_REPORT")
	if path == "" {
		path = filepath.Join(t.TempDir(), "interop-newapi-actual.md")
	}
	if dir := filepath.Dir(path); dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(path, []byte("# new-api 存量应用互通实测（真实请求）\n\n"+out), 0o644); err != nil {
		t.Fatalf("写实测表失败: %v", err)
	}
	t.Logf("实测表: %s", path)
	for _, r := range rows {
		t.Logf("%-52s %s %-42s → %d %v", r.Name, r.Method, r.Path, r.Status, r.Keys)
	}

	// ── 断言（报告只是事实，不加断言等于没测）────────────────────────────────
	//
	// 只断言**稳定契约**：状态码 + 关键形状。摘要文本会随渠道/模型变化，不做断言。
	// 每条都对应一次真实修复，注释写明它守的是什么。
	byName := map[string]interopRow{}
	for _, r := range rows {
		byName[strings.SplitN(r.Name, " ", 2)[0]] = r
	}
	wantStatus := map[string]int{
		// /v1/edits 是 images/edits 的旧别名，历史上只发 JSON 体。
		"19": 200,
		// Responses 真流式：过去是一刀切 400 endpoint_unsupported。
		"31": 200,
		// Codex 上下文压缩：过去 404。
		"40": 200,
		// Gemini 协议面（过去整棵树 404）。
		"15": 200, // 模型列表
		"16": 200, // generateContent
		"41": 200, // /v1/engines 遗留向量化
		"49": 200, // streamGenerateContent?alt=sse
		"56": 200, // embedContent（Gemini 形状）
		"50": 400, // 缺 alt=sse 必须 400 且指路
		"51": 404, // 未知动作
		"52": 404, // 未知路径
		"53": 401, // 无凭证
		"54": 200, // 兼容层模型列表
		"55": 404, // 兼容层未知路径
		// countTokens 明确不提供：501 而不是伪造一个数字。
		"42": 501,
		// 同一 URL 两种形状都要 200（形状本身在下面逐条断言）。
		"57": 200,
		"58": 200,
		"59": 200,
		"60": 200,
	}
	for id, want := range wantStatus {
		r, ok := byName[id]
		if !ok {
			t.Fatalf("用例 %s 没有产出结果（用例表被改了？）", id)
		}
		if r.Status != want {
			t.Errorf("用例 %s（%s）状态 %d，期望 %d：%v", id, r.Name, r.Status, want, r.Keys)
		}
	}
	// ── /v1/models 的三形状 ───────────────────────────────────────────────
	//
	// 这三条是本轮对账新发现的真差异：同一个 URL，三类 SDK 期待三种形状。
	// 断言"形状"而不是"条数"：条数随目录变化，形状是契约。
	if r, ok := byName["57"]; ok && r.Status == 200 {
		// 顶层判据：Anthropic 是 {data, first_id, has_more, last_id} 且**没有** object；
		// OpenAI 是 {object, data, success}；Gemini 是 {models}。三者互斥，顶层就够区分。
		j := strings.Join(r.Keys, ",")
		if !strings.Contains(j, "first_id") || !strings.Contains(j, "last_id") || !strings.Contains(j, "has_more") {
			t.Errorf("Anthropic 头下的 /v1/models 不是 Anthropic 形状：%v", r.Keys)
		}
		if strings.Contains(j, "object") || strings.Contains(j, "models") {
			t.Errorf("Anthropic 头下不该出现 OpenAI/Gemini 的顶层键：%v", r.Keys)
		}
		// 元素层判据：created_at 必须是 RFC3339 字符串（给成 Unix 秒会让 SDK 时间解析抛错）。
		if !strings.Contains(r.Note, `"created_at":"`) {
			t.Errorf("Anthropic 形状缺 created_at（RFC3339 字符串）：%s", r.Note)
		}
		if strings.Contains(r.Note, `"created_at":2`) {
			t.Errorf("created_at 给成了数字，Anthropic 要求 RFC3339 字符串：%s", r.Note)
		}
	}
	// 02/03 是「Gemini 的鉴权习惯打在 /v1/models 上」，必须给 Gemini 形状。
	// 这两条以前是 OpenAI 形状（本轮改成按头分支），加断言把新口径钉住：
	// 没有断言的行为变更，下一次重构会悄悄退回去。
	for _, id := range []string{"02", "03", "58"} {
		r, ok := byName[id]
		if !ok {
			t.Fatalf("用例 %s 没有产出结果", id)
		}
		if r.Status == 200 && !strings.Contains(strings.Join(r.Keys, ","), "models") {
			t.Errorf("用例 %s（%s）应是 Gemini 形状（含 models）：%v", id, r.Name, r.Keys)
		}
	}
	if r, ok := byName["58"]; ok && r.Status == 200 {
		if !strings.Contains(strings.Join(r.Keys, ","), "models") {
			t.Errorf("x-goog-api-key 头下的 /v1/models 不是 Gemini 形状（缺 models）：%v", r.Keys)
		}
	}
	if r, ok := byName["59"]; ok && r.Status == 200 {
		if !strings.Contains(strings.Join(r.Keys, ","), "candidates") {
			t.Errorf("/v1/models/*path 的 Gemini 通配响应缺 candidates：%v", r.Keys)
		}
	}
	if r, ok := byName["60"]; ok && r.Status == 200 {
		j := strings.Join(r.Keys, ",")
		if !strings.Contains(j, "object") || strings.Contains(j, "first_id") {
			t.Errorf("/v1beta/openai/models 必须恒为 OpenAI 形状（带 ?key= 也不行）：%v", r.Keys)
		}
	}

	// Gemini 的错误体形状必须钉死：code 是整数、status 是枚举。
	// 回成 OpenAI 形状时 SDK 只会说 "unknown error"，用户连"是参数错还是额度不足"都分不出。
	geminiShaped := []string{"42", "51", "52", "53"}
	for _, id := range geminiShaped {
		r, ok := byName[id]
		if !ok {
			t.Fatalf("用例 %s 没有产出结果", id)
		}
		if !strings.Contains(r.Note, `"status":"`) {
			t.Errorf("用例 %s（%s）不是 Gemini 错误体形状：%s", id, r.Name, r.Note)
		}
	}
	// 兼容层必须保持 OpenAI 形状（它的前提就是"OpenAI SDK 只改 base_url"）。
	for _, id := range []string{"55"} {
		r, ok := byName[id]
		if !ok {
			t.Fatalf("用例 %s 没有产出结果", id)
		}
		if !strings.Contains(r.Note, `"type":"invalid_request_error"`) {
			t.Errorf("用例 %s（%s）应为 OpenAI 错误体（兼容层例外）：%s", id, r.Name, r.Note)
		}
	}
	// Gemini 的生成响应必须是 candidates 形状：只改 id/model 的话 SDK 解析不到内容，
	// 表现为"200 但拿不到数据"——最坏的一种修复。
	if r, ok := byName["16"]; ok && r.Status == 200 {
		if !strings.Contains(strings.Join(r.Keys, ","), "candidates") {
			t.Errorf("generateContent 响应缺少 candidates：%v", r.Keys)
		}
	}
	// SSE 的形状必须钉死：OpenAI 系的 responses 流**不发 [DONE]**，
	// 也不带 event: 行（那是 Anthropic 的形状）。发错了客户端解析器会直接报错。
	if r, ok := byName["31"]; ok && r.Status == 200 {
		if !strings.HasPrefix(r.ContentType, "text/event-stream") {
			t.Errorf("Responses 流式的 Content-Type 是 %q，期望 text/event-stream", r.ContentType)
		}
		hasData, hasDone, hasEvent := false, false, false
		for _, k := range r.Keys {
			switch k {
			case "data(JSON)":
				hasData = true
			case "[DONE]":
				hasDone = true
			default:
				if strings.HasPrefix(k, "event:") {
					hasEvent = true
				}
			}
		}
		if !hasData {
			t.Errorf("Responses 流式没有任何 JSON 数据帧：%v", r.Keys)
		}
		if hasDone {
			t.Errorf("Responses 流式不应以 [DONE] 收尾（那是 chat 的终止符）：%v", r.Keys)
		}
		if hasEvent {
			t.Errorf("Responses 流式不应带 event: 行（那是 Anthropic 的形状）：%v", r.Keys)
		}
	}
}

func shortCT(ct string) string {
	if ct == "" {
		return "—"
	}
	if i := strings.Index(ct, ";"); i > 0 {
		return ct[:i]
	}
	return ct
}

// int64Ptr 取 int64 指针（规格价目的上限字段是指针，零值表示"未设置"）。
func int64Ptr(v int64) *int64 { return &v }

const multiType = "multipart/form-data; boundary=----interop"

// multiBody 造一个最小 multipart（一个文本字段 + 一个假音频文件）。
func multiBody(model string) []byte {
	var b bytes.Buffer
	w := "--" + strings.TrimPrefix(multiType, "multipart/form-data; boundary=")
	b.WriteString(w + "\r\n")
	b.WriteString("Content-Disposition: form-data; name=\"model\"\r\n\r\n")
	b.WriteString(model + "\r\n")
	b.WriteString(w + "\r\n")
	b.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"a.mp3\"\r\n")
	b.WriteString("Content-Type: audio/mpeg\r\n\r\n")
	b.WriteString("ID3fake\r\n")
	b.WriteString(w + "--\r\n")
	return b.Bytes()
}

// submitVideo 提交一个视频任务并返回平台任务号（失败返回空串）。
func submitVideo(t *testing.T, base string, c *http.Client, auth map[string]string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"model": "sora-1", "prompt": "a cat", "duration": 5, "size": "1280x720"})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/video/generations", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth["Authorization"])
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var obj map[string]any
	body, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	if id, _ := obj["task_id"].(string); id != "" {
		return id
	}
	id, _ := obj["id"].(string)
	return id
}

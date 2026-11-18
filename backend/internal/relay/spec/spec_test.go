package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeJSON 是测试用的辅助：把 JSON 字面量解成 Spec 并跑校验。
func decodeJSON(t *testing.T, raw string) (*Spec, error) {
	t.Helper()
	s, err := Decode([]byte(raw))
	if err != nil {
		return nil, err
	}
	return s, s.Validate()
}

// TestSpecValidateAccepts 覆盖规格应当接受的六种典型形状。
//
// 这些用例的用途不是"证明校验能过"，而是**锁死表达力边界**：
// 一旦有人放宽限额或删掉某条分支，用例会立刻失败，而不是等到线上出现
// "配了但永远不生效"的渠道。
func TestSpecValidateAccepts(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			// 1. OpenAI 兼容对话：域名不同、鉴权不同，只要透传。
			name: "chat 透传（最小规格）",
			raw: `{
				"slug": "relay-a", "label": "中转 A",
				"endpoints": {
					"chat/completions": {"path": "/v1/chat/completions", "passthrough": true}
				}
			}`,
		},
		{
			// 2. 同步图像：白名单重建请求体 + 单产物取值。
			name: "同步图像（const/copy + result）",
			raw: `{
				"slug": "img-b", "label": "图像 B", "vendor": "openai",
				"endpoints": {
					"images/generations": {
						"path": "/v1/images/generations",
						"body": {
							"const": {"model": "gpt-image-2", "response_format": "url"},
							"copy": {"prompt": "$req.prompt", "size": "$req.size", "n": "$req.n"}
						},
						"extract": {"result": "$.data[0].url"}
					}
				}
			}`,
		},
		{
			// 3. 异步视频：提交 + 轮询 + 四态映射 + 数组映射（首尾帧）。
			name: "异步视频（submit + poll）",
			raw: `{
				"slug": "video-c", "label": "视频 C",
				"endpoints": {
					"video/generations": {
						"path": "/v1/videos",
						"body": {
							"const": {"model": "kling-v2"},
							"copy": {"prompt": "$req.prompt"},
							"arrays": {
								"image_list": {"from": "$req.image_list", "max": 2, "item": {"image_url": "$value"}}
							}
						},
						"extract": {"task_id": "$.data.task_id"},
						"async": {"poll": "video/generations@poll"}
					},
					"video/generations@poll": {
						"path": "/v1/videos/{task_id}",
						"extract": {"status": "$.data.status", "progress": "$.data.progress", "result_urls": "$.data.videos[*].url"},
						"status_map": {
							"queued": ["waiting"],
							"running": ["processing"],
							"succeeded": ["succeed"],
							"failed": ["failed"]
						}
					}
				}
			}`,
		},
		{
			// 4. 成功判据在响应体里（上游 200 也可能是业务失败）。
			name: "body_code 成功判据",
			raw: `{
				"slug": "audio-d", "label": "语音 D",
				"endpoints": {
					"audio/speech": {
						"path": "/api/tts",
						"auth": {"in": "query", "name": "token"},
						"query": {"format": "mp3"},
						"body": {"copy": {"text": "$req.input", "voice": "$req.voice"}},
						"extract": {"result": "$.data.audio_url"},
						"errors": {"success": "body_code", "code_path": "$.code", "ok_codes": ["0", "200"], "message_path": "$.msg"}
					}
				}
			}`,
		},
		{
			// 5. 多端点混合：对话 + 向量 + 重排（同一家上游同一种鉴权）。
			name: "多端点混合",
			raw: `{
				"slug": "multi-e", "label": "混合 E",
				"endpoints": {
					"chat/completions": {"path": "/v1/chat/completions", "passthrough": true},
					"embeddings": {
						"path": "/v1/embeddings",
						"body": {"const": {"model": "text-embed"}, "copy": {"input": "$req.input"}},
						"extract": {"result": "$.data[0].embedding"}
					},
					"rerank": {
						"path": "/v1/rerank",
						"body": {"copy": {"query": "$req.query", "documents": "$req.documents"}},
						"extract": {"result": "$.results[0].relevance_score"}
					}
				}
			}`,
		},
		{
			// 6. 图像异步版：正本端点就是 images/generations，带 async 声明即支持 async:true。
			// 没有单独的 .../async 键（那会让同一个能力有两个名字，前端与路由都要分叉）。
			name: "异步图像",
			raw: `{
				"slug": "img-async", "label": "图像异步",
				"endpoints": {
					"images/generations": {
						"path": "/v1/images/generations",
						"body": {"copy": {"prompt": "$req.prompt"}},
						"extract": {"result_urls": "$.data[*].url", "task_id": "$.task_id"},
						"async": {"poll": "images/generations@poll"}
					},
					"images/generations@poll": {
						"path": "/v1/images/tasks/{task_id}",
						"extract": {"status": "$.status", "result_urls": "$.images[*].url"},
						"status_map": {
							"queued": ["pending"],
							"running": ["running"],
							"succeeded": ["done"],
							"failed": ["error"]
						}
					}
				}
			}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := decodeJSON(t, c.raw); err != nil {
				t.Fatalf("应接受但被拒绝: %v", err)
			}
		})
	}
}

// TestSpecValidateRejects 覆盖规格必须拒绝的十四种形状。
//
// 每一条都对应一个真实会出问题的写法，错误都必须带**字段定位**：
// 只说"规格不合法"，运营面对 200 行 JSON 无从下手。
func TestSpecValidateRejects(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		field string // 期望错误里出现的字段前缀（空 = 只要报错即可）
	}{
		{
			name:  "slug 不合法",
			raw:   `{"slug": "Img_B", "label": "x", "endpoints": {"chat/completions": {"path": "/v1/chat/completions", "passthrough": true}}}`,
			field: "slug",
		},
		{
			name:  "label 为空",
			raw:   `{"slug": "abc", "label": "  ", "endpoints": {"chat/completions": {"path": "/v1/chat/completions", "passthrough": true}}}`,
			field: "label",
		},
		{
			name:  "没有端点",
			raw:   `{"slug": "abc", "label": "x", "endpoints": {}}`,
			field: "endpoints",
		},
		{
			name:  "端点不在允许清单",
			raw:   `{"slug": "abc", "label": "x", "endpoints": {"chat/flux": {"path": "/v1/flux", "passthrough": true}}}`,
			field: "endpoints.chat/flux",
		},
		{
			name:  "path 带协议",
			raw:   `{"slug": "abc", "label": "x", "endpoints": {"chat/completions": {"path": "https://evil.example/v1", "passthrough": true}}}`,
			field: "path",
		},
		{
			name:  "path 带 ..",
			raw:   `{"slug": "abc", "label": "x", "endpoints": {"chat/completions": {"path": "/v1/../admin", "passthrough": true}}}`,
			field: "path",
		},
		{
			name:  "鉴权头不在白名单",
			raw:   `{"slug": "abc", "label": "x", "endpoints": {"chat/completions": {"path": "/v1/chat/completions", "passthrough": true, "auth": {"name": "x-forwarded-for"}}}}`,
			field: "auth",
		},
		{
			name:  "const 写死计费维度",
			raw:   `{"slug": "abc", "label": "x", "endpoints": {"images/generations": {"path": "/v1/images/generations", "body": {"const": {"seconds": 10}}, "extract": {"result": "$.data[0].url"}}}}`,
			field: "const.seconds",
		},
		{
			name:  "透传与非透传混用 drop",
			raw:   `{"slug": "abc", "label": "x", "endpoints": {"images/generations": {"path": "/v1/images/generations", "body": {"drop": ["user"]}, "extract": {"result": "$.data[0].url"}}}}`,
			field: "body.drop",
		},
		{
			name:  "异步端点缺 async",
			raw:   `{"slug": "abc", "label": "x", "endpoints": {"video/generations": {"path": "/v1/videos", "body": {"const": {"model": "m"}}, "extract": {"task_id": "$.id"}}}}`,
			field: "async",
		},
		{
			name: "轮询端点未声明",
			raw: `{"slug": "abc", "label": "x", "endpoints": {"video/generations": {
				"path": "/v1/videos", "body": {"const": {"model": "m"}},
				"extract": {"task_id": "$.id"}, "async": {"poll": "video/generations@poll"}}}}`,
			field: "async.poll",
		},
		{
			name: "status_map 缺态",
			raw: `{"slug": "abc", "label": "x", "endpoints": {
				"video/generations": {"path": "/v1/videos", "body": {"const": {"model": "m"}},
					"extract": {"task_id": "$.id"}, "async": {"poll": "video/generations@poll"}},
				"video/generations@poll": {"path": "/v1/videos/{task_id}",
					"extract": {"status": "$.status"},
					"status_map": {"queued": ["wait"], "running": ["run"], "succeeded": ["ok"]}}}}`,
			field: "status_map",
		},
		{
			name: "同步端点没有结果取值",
			raw: `{"slug": "abc", "label": "x", "endpoints": {
				"images/generations": {"path": "/v1/images/generations", "body": {"const": {"model": "m"}}}}}`,
			field: "extract",
		},
		{
			name:  "multipart 端点不能由规格承载",
			raw:   `{"slug": "abc", "label": "x", "endpoints": {"images/edits": {"path": "/v1/images/edits", "extract": {"result": "$.data[0].url"}}}}`,
			field: "images/edits",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeJSON(t, c.raw)
			if err == nil {
				t.Fatalf("应拒绝但被接受")
			}
			if c.field == "" {
				return
			}
			field, msg := AsFieldError(err)
			// 字段定位形如 `endpoints.<key>.body.const.seconds`：断言**后缀**匹配，
			// 这样用例不必重复写端点键，同时仍能锁住"定位到具体字段"这条要求。
			if !strings.HasSuffix(field, c.field) {
				t.Fatalf("错误应定位到字段 %q，实际 field=%q msg=%q", c.field, field, msg)
			}
		})
	}
}

// TestSpecDecodeStrict 断言未知键被拒绝。
//
// 为什么必须严格：规格是手写/由 AI 生成的 JSON，拼错的键（`passthrouh`）
// 若被静默忽略，结果就是"配置写了但没生效"——上游照常收到未改写的请求。
func TestSpecDecodeStrict(t *testing.T) {
	_, err := Decode([]byte(`{"slug":"abc","label":"x","endpoints":{},"passthrouh":true}`))
	if err == nil {
		t.Fatal("未知键应被拒绝")
	}
	if !strings.Contains(err.Error(), "未知字段") && !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("错误信息应指出未知字段，实际: %v", err)
	}
}

// TestSpecDecodeLimits 断言超限被拒绝（空内容 / 超过 64KB）。
func TestSpecDecodeLimits(t *testing.T) {
	if _, err := Decode(nil); err == nil {
		t.Fatal("空规格应被拒绝")
	}
	big := `{"slug":"abc","label":"` + strings.Repeat("x", MaxSpecBytes) + `"}`
	if _, err := Decode([]byte(big)); err == nil {
		t.Fatal("超过字节上限的规格应被拒绝")
	}
}

// TestSpecCaps 断言能力矩阵由端点派生（不单独声明）。
func TestSpecCaps(t *testing.T) {
	raw := `{"slug":"abc","label":"x","endpoints":{
		"chat/completions":{"path":"/v1/chat/completions","passthrough":true},
		"images/generations":{"path":"/v1/images/generations","body":{"const":{"model":"m"}},"extract":{"result":"$.data[0].url"}}}}`
	s, err := decodeJSON(t, raw)
	if err != nil {
		t.Fatalf("应接受: %v", err)
	}
	got := strings.Join(s.Caps(), ",")
	if got != "image,text" {
		t.Fatalf("能力矩阵不符：got %q want %q", got, "image,text")
	}
}

// TestSpecPollKeyHelpers 断言 poll 键的拼装/还原一致（提交端点与轮询端点靠它配对）。
func TestSpecPollKeyHelpers(t *testing.T) {
	key := "video/generations"
	if PollKey(key) != key+"@poll" {
		t.Fatalf("PollKey 拼接错误: %q", PollKey(key))
	}
	if !IsPollKey(PollKey(key)) {
		t.Fatal("IsPollKey 应识别轮询键")
	}
	if BaseKey(PollKey(key)) != key {
		t.Fatalf("BaseKey 还原错误: %q", BaseKey(PollKey(key)))
	}
	if IsPollKey(key) {
		t.Fatal("提交键不应被判为轮询键")
	}
}

// TestSpecJSONRoundTrip 断言「校验通过 → 序列化 → 再解析」仍然等价。
//
// 落库前的规范化就是这条路径：如果 round-trip 会丢字段，
// 运营保存后刷新页面看到的就是"少了点什么"的规格。
func TestSpecJSONRoundTrip(t *testing.T) {
	raw := `{"slug":"abc","label":"x","endpoints":{
		"video/generations":{"path":"/v1/videos","body":{"const":{"model":"m"}},
			"extract":{"task_id":"$.id"},"async":{"poll":"video/generations@poll"}},
		"video/generations@poll":{"path":"/v1/videos/{task_id}","extract":{"status":"$.status"},
			"status_map":{"queued":["wait"],"running":["run"],"succeeded":["ok"],"failed":["err"]}}}}`
	s, err := decodeJSON(t, raw)
	if err != nil {
		t.Fatalf("应接受: %v", err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	s2, err := Decode(b)
	if err != nil {
		t.Fatalf("二次解析失败: %v", err)
	}
	if err := s2.Validate(); err != nil {
		t.Fatalf("二次校验失败: %v", err)
	}
	if len(s2.Endpoints) != len(s.Endpoints) {
		t.Fatalf("端点数量不一致: %d vs %d", len(s2.Endpoints), len(s.Endpoints))
	}
	if !s2.SupportsEndpointKey("video/generations@poll") {
		t.Fatal("轮询端点在 round-trip 后丢失")
	}
}

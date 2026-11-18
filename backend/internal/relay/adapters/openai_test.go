package adapters

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"lingdong-api/internal/relay"
)

// TestTestSuccess 校验 base_url 可达且鉴权通过时连通测试成功。
func TestTestSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	a := OpenAI{}
	if err := a.Test(context.Background(), &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-test"}); err != nil {
		t.Fatalf("连通测试应成功，实际: %v", err)
	}
}

// TestTestUnauthorized 校验上游 401 时连通测试失败并给出状态码。
func TestTestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := OpenAI{}
	err := a.Test(context.Background(), &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-bad"})
	if err == nil {
		t.Fatal("401 应判定为连通失败")
	}
}

// TestTestUnreachable 校验 base_url 不可达时的失败路径。
func TestTestUnreachable(t *testing.T) {
	a := OpenAI{}
	err := a.Test(context.Background(), &relay.ChannelConfig{BaseURL: "http://127.0.0.1:1", APIKey: "sk-x"})
	if err == nil {
		t.Fatal("不可达地址应判定为连通失败")
	}
}

// TestTestMissingConfig 校验缺 URL/密钥时的快速失败。
func TestTestMissingConfig(t *testing.T) {
	a := OpenAI{}
	if err := a.Test(context.Background(), &relay.ChannelConfig{APIKey: "k"}); err == nil {
		t.Fatal("缺 BaseURL 应报错")
	}
	if err := a.Test(context.Background(), &relay.ChannelConfig{BaseURL: "http://x"}); err == nil {
		t.Fatal("缺密钥应报错")
	}
}

// TestChatPassthrough 校验对话请求透传：路径/鉴权头/模型别名替换。
func TestChatPassthrough(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer srv.Close()

	a := OpenAI{}
	resp, err := a.Chat(context.Background(), &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-1"}, &relay.RelayRequest{
		Model: "upstream-gpt",
		Body:  json.RawMessage(`{"model":"local-alias","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("透传失败: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("路径不符: %s", gotPath)
	}
	if gotAuth != "Bearer sk-1" {
		t.Fatalf("鉴权头不符: %s", gotAuth)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	var model string
	_ = json.Unmarshal(sent["model"], &model)
	if model != "upstream-gpt" {
		t.Fatalf("模型别名未替换为上游模型: %s", model)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("响应状态码不符: %d", resp.StatusCode)
	}
	if len(resp.Body) == 0 {
		t.Fatal("响应体不应为空")
	}
}

// TestEmbeddingsPath 校验向量请求打向 /v1/embeddings。
func TestEmbeddingsPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	a := OpenAI{}
	if _, err := a.Embeddings(context.Background(), &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "k"}, &relay.RelayRequest{
		Body: json.RawMessage(`{"model":"m","input":"x"}`),
	}); err != nil {
		t.Fatalf("向量透传失败: %v", err)
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("路径不符: %s", gotPath)
	}
}

// TestEmptyBody 校验空请求体快速失败。
func TestEmptyBody(t *testing.T) {
	a := OpenAI{}
	if _, err := a.Chat(context.Background(), &relay.ChannelConfig{BaseURL: "http://x"}, &relay.RelayRequest{}); err == nil {
		t.Fatal("空请求体应报错")
	}
}

// TestCaps 校验能力矩阵声明。
func TestCaps(t *testing.T) {
	caps := OpenAI{}.Caps()
	if len(caps) == 0 {
		t.Fatal("能力矩阵不应为空")
	}
}

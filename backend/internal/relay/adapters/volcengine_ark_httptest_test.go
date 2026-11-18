package adapters

// 本文件是火山方舟适配器的**调用级**测试：真的起一个假 Ark 上游，走完整的
// 「构造请求 → 出站 → 解析响应 / 翻译错误」链路。
//
// 为什么必须有这一层：此前的用例全是纯函数构造（arkBuildSpecBody / arkReplaceModel），
// 能证明"某个函数对某份输入产出某份字节"，但证明不了「SubmitTask 真的把这份字节
// 发到了哪个 URL、带了什么鉴权头、拿什么状态码做什么归因」。这两件事之间隔着
// client 构造、BuildRawURL、SetAuthHeader、arkDo、arkSubmitError 五层。
//
// 一条真实踩过的坑正是这样漏掉的：TestArkReplaceModelKeepsOtherValues 直接调
// arkReplaceModel，跳过了 task 层的 normalizeSubmitBody（map[string]any 往返），
// 而那里已经丢了大整数精度 —— 单测全绿，线上 seed 已被改写。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// arkTestChannel 起一个假 Ark 上游并返回指向它的渠道配置。
//
// 必须打开 LD_UPSTREAM_ALLOW_PRIVATE：netguard 默认拒绝环回地址，
// 不开的话所有请求都在拨号阶段被拒（ErrBlocked），表现成"上游不可达"，
// 与真实的连接失败无法区分，测试会误以为代码有问题。
func arkTestChannel(t *testing.T, h http.HandlerFunc) *relay.ChannelConfig {
	t.Helper()
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &relay.ChannelConfig{
		ID: 1, Name: "fake-ark", BaseURL: srv.URL, APIKey: "test-key",
		Timeout: 5 * time.Second,
	}
}

// ① arkDo 是视频链路唯一出站口：路径拼接、鉴权头、响应体都必须是它负责。
func TestArkDoForwardsPathAndAuth(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cgt-1"}`))
	})

	resp, err := arkDo(context.Background(), cfg, http.MethodPost, arkPathTaskSubmit, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("arkDo 失败：%v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.Status)
	}
	if gotPath != arkPathTaskSubmit {
		t.Fatalf("路径 = %q，期望 %q", gotPath, arkPathTaskSubmit)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("方法 = %q，期望 POST", gotMethod)
	}
	// Bearer 头是唯一鉴权方式：写错一个字符，所有请求都是 401，
	// 而 401 会走到「冷却密钥」分支，表现为"密钥莫名其妙被冷却"。
	if !strings.HasPrefix(gotAuth, "Bearer ") || !strings.HasSuffix(gotAuth, "test-key") {
		t.Fatalf("Authorization = %q，期望 Bearer 前缀 + 密钥", gotAuth)
	}
}

// ② arkDo 只负责搬运：非 2xx 原样返回状态码与体，由 arkSubmitError/arkError 翻译。
//
// 这条边界必须守住：如果 arkDo 自己把 4xx 转成 error，提交与轮询两条路会各拿
// 到一份"已经被翻译过"的错误，再翻译一次就变成重复包装（IsAmbiguousSubmit
// 认不出 HTTPStatusError，5xx 会被当成可换渠道 → 建出两个上游任务）。
func TestArkDoReturnsStatusWithoutTranslating(t *testing.T) {
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"InternalServiceError","message":"boom"}}`))
	})

	resp, err := arkDo(context.Background(), cfg, http.MethodGet, arkPathTaskList, nil)
	if err != nil {
		t.Fatalf("arkDo 不应翻译错误，却返回 err=%v", err)
	}
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "InternalServiceError") {
		t.Fatalf("响应体未原样带回：%s", resp.Body)
	}
}

// ③ 提交成功：2xx + 合法 id → 返回任务号，并回写子协议标记。
func TestArkSubmitTaskCreates(t *testing.T) {
	var gotBody map[string]any
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"cgt-20261028-abc"}`))
	})

	req := &relay.TaskSubmitRequest{
		UpstreamModel: "doubao-seedance-2-0-260128",
		// 原生体（content[] 非空）→ 走 arkReplaceModel，只换 model。
		Body: []byte(`{"model":"alias","content":[{"type":"text","text":"a cat"}],"duration":5}`),
	}
	id, err := VolcengineARK{}.SubmitTask(context.Background(), cfg, req)
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if id != "cgt-20261028-abc" {
		t.Fatalf("任务号 = %q", id)
	}
	// model 必须换成渠道绑定里的 upstream_model（别名/区域模型 id 全靠这一步）。
	if gotBody["model"] != "doubao-seedance-2-0-260128" {
		t.Fatalf("出站 model = %v，期望被替换为 upstream_model", gotBody["model"])
	}
	// 原生通道的标记必须回写，否则重试会把它当规范体重新拼一遍。
	if req.SubKind != arkSubKindNative {
		t.Fatalf("SubKind = %q，期望 %q", req.SubKind, arkSubKindNative)
	}
}

// ④ 2xx 但没有任务号 = 契约违约，必须让上层停止 failover。
func TestArkSubmitTaskMissingIDIsAmbiguous(t *testing.T) {
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	req := &relay.TaskSubmitRequest{
		UpstreamModel: "m",
		Body:          []byte(`{"model":"m","content":[{"type":"text","text":"x"}]}`),
	}
	_, err := VolcengineARK{}.SubmitTask(context.Background(), cfg, req)
	if err == nil {
		t.Fatal("缺少任务号必须报错")
	}
	var amb *relay.AmbiguousSubmitError
	if !errors.As(err, &amb) {
		t.Fatalf("错误类型 = %T，期望 *relay.AmbiguousSubmitError（换渠道会建出第二个上游任务）", err)
	}
}

// ⑤ 400 = 调用方参数错：必须是 CallerParamError，且带上游原话。
func TestArkSubmitTask400IsCallerError(t *testing.T) {
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"InvalidParameter","message":"duration must be in [2,30]"}}`))
	})

	req := &relay.TaskSubmitRequest{
		UpstreamModel: "m",
		Body:          []byte(`{"model":"m","content":[{"type":"text","text":"x"}],"duration":99}`),
	}
	_, err := VolcengineARK{}.SubmitTask(context.Background(), cfg, req)
	if !errors.Is(err, relay.ErrCallerParam) {
		t.Fatalf("错误 = %v，期望 CallerParamError（换渠道不会变好，必须直接 400）", err)
	}
	if !strings.Contains(err.Error(), "duration must be in") {
		t.Fatalf("错误文案未带上游原话：%v", err)
	}
}

// ⑥ 401 = 密钥问题：必须带状态码（B5 靠 StatusOf 判定 auth 分支），
// 而不是普通 error —— 普通 error 会被归因成"上游故障"并熔断整条渠道。
func TestArkSubmitTask401CarriesStatus(t *testing.T) {
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"AuthenticationError","message":"invalid key"}}`))
	})

	req := &relay.TaskSubmitRequest{
		UpstreamModel: "m",
		Body:          []byte(`{"model":"m","content":[{"type":"text","text":"x"}]}`),
	}
	_, err := VolcengineARK{}.SubmitTask(context.Background(), cfg, req)
	if err == nil {
		t.Fatal("401 必须报错")
	}
	if got := relay.StatusOf(err); got != http.StatusUnauthorized {
		t.Fatalf("StatusOf(err) = %d，期望 401（否则上层无法区分密钥坏与上游坏）", got)
	}
}

// ⑦ 轮询成功：产物顺序、时长、状态全部要落到 TaskStatus 上。
func TestArkPollSucceededReadsDurationAndURLs(t *testing.T) {
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/contents/generations/tasks/") {
			t.Errorf("轮询路径 = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"id":"cgt-1","status":"succeeded",
			"content":{"video_url":"https://cdn.example.com/v.mp4","last_frame_url":"","file_url":""},
			"usage":{"completion_tokens":411300,"total_tokens":411300},
			"duration":11,"frames":0,"framespersecond":24
		}`))
	})

	st, err := VolcengineARK{}.PollTaskDetailed(context.Background(), cfg,
		&relay.TaskPollRequest{UpstreamTaskID: "cgt-1"})
	if err != nil {
		t.Fatalf("轮询失败：%v", err)
	}
	if st.Status != taskstate.Succeeded {
		t.Fatalf("状态 = %q，期望 succeeded", st.Status)
	}
	if len(st.ResultURLs) != 1 || st.ResultURLs[0] != "https://cdn.example.com/v.mp4" {
		t.Fatalf("产物 = %v，期望单条主视频", st.ResultURLs)
	}
	if st.Usage.OutputSeconds != 11 {
		t.Fatalf("时长 = %v，期望 11（结算按它计费）", st.Usage.OutputSeconds)
	}
	// usage 原文必须逐字节留住（B7-3）：Ark 现在报的是 token 口径，
	// 平台既不用它计费也解析不出秒数，但「上游说了什么」只有原文能回答。
	if !strings.Contains(string(st.Usage.Raw), `"total_tokens":411300`) {
		t.Fatalf("usage 原文未保留：%q", string(st.Usage.Raw))
	}
}

// ⑧ 进行中 / 任务不存在：都必须继续轮询，不能判失败。
//
// 404 尤其容易写错：判失败会立即退款并丢弃可能已生成的产物；判渠道故障会熔断
// 一条其实健康的渠道。
func TestArkPollRunningAndNotFoundStayRunning(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"运行中", http.StatusOK, `{"status":"running"}`},
		{"任务不存在", http.StatusNotFound, `{"error":{"code":"NotFound","message":"no such task"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			})
			st, err := VolcengineARK{}.PollTaskDetailed(context.Background(), cfg,
				&relay.TaskPollRequest{UpstreamTaskID: "cgt-1"})
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if st.Status != taskstate.Running {
				t.Fatalf("状态 = %q，期望 running", st.Status)
			}
		})
	}
}

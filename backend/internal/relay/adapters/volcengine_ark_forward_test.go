package adapters

// 本文件是火山方舟**图片同步**链路的调用级测试（此前完全空缺）。
//
// 为什么必须有这一层（一条真实事故）：
//   - 旧实现用 `json.Marshal(res.ImageGenerationResponse)` 把 SDK typed 响应「重序列化」成
//     出站报文。传进 interface 的值拷贝不可寻址 ⇒ SDK 的指针接收者 MarshalJSON 不生效，
//     encoding/json 退回逐字段反射；而 `OptImageError` 这类字段的 MarshalJSON 是**值接收者**
//     且零值一个字节都不写 ⇒ 拿到空字节 ⇒ `unexpected end of JSON input`。
//   - 结果：**上游已经出图并计费（HTTP 2xx），平台把整份响应丢掉**，返回普通 error →
//     网关判渠道故障并换候选重投（最多 4 次真实出站），调用方只看到 503。
//   - 当时的全部测试只覆盖「请求构造」（volcengine_ark_golden_test.go），没有一处真的调用
//     `VolcengineARK{}.Forward`，所以这行从上线起就是坏的，却全绿通过。
//
// 因此这里的断言核心只有一句：**2xx 的响应体必须逐字节等于上游原文**。
// 值正确但键序被重排 = 我们又在二次序列化 = 这条链路会再次丢字段。

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"

	"lingdong-api/internal/relay"
)

const arkForwardUpstream = "doubao-seedream-5-0-flash-260915"

// arkForwardReq 是网关交给适配器的请求（Model 已被换成上游模型名）。
func arkForwardReq(body string) *relay.RelayRequest {
	return &relay.RelayRequest{Model: arkForwardUpstream, ModelID: "doubao-seedream-5-0-flash", Body: []byte(body)}
}

// arkSuccessBody 刻意同时具备三个「旧实现必错」的特征：
//  1. **没有顶层 `error`** —— 这正是旧实现崩溃的触发条件（OptImageError 未 Set）；
//  2. 带 `usage` —— 证明上游真的出图并报账（也让旧实现不先在 OptUsage 上炸）；
//  3. 含 schema 外字段 `request_id`，且键序不是字典序 —— 旧实现的 typed 往返会丢字段+重排。
const arkSuccessBody = `{"created":1767225600,"model":"` + arkForwardUpstream + `",` +
	`"data":[{"url":"https://ark.example.com/img/1.png","size":"1024x1024","output_format":"png"}],` +
	`"usage":{"generated_images":1,"output_tokens":4096,"total_tokens":4100},` +
	`"request_id":"req-1"}`

// TestArkImageForwardReturnsUpstreamBytesVerbatim 是本次事故的回归钉。
//
// 旧实现（json.Marshal typed）在这条用例上会报
// `响应序列化失败：json: error calling MarshalJSON for type images.OptImageError: unexpected end of JSON input`。
func TestArkImageForwardReturnsUpstreamBytesVerbatim(t *testing.T) {
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(arkSuccessBody))
	})

	resp, err := VolcengineARK{}.Forward(context.Background(), cfg, arkForwardReq(`{"model":"m","prompt":"a"}`), relay.PathImagesGenerations)
	if err != nil {
		t.Fatalf("上游 2xx 时 Forward 必须成功，实际报错：%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	// 逐字节比较：值相同但键序不同也判失败（键序变了就说明我们又解析再序列化了一次）。
	if string(resp.Body) != arkSuccessBody {
		t.Fatalf("响应体必须原样透传（不得二次序列化）\n上游原文: %s\n平台返回: %s", arkSuccessBody, resp.Body)
	}
	if resp.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q，期望 application/json", resp.ContentType)
	}
}

// TestArkImageForwardRewritesModelAndPath：出站路径、鉴权、模型替换三件事只能发生一次。
func TestArkImageForwardRewritesModelAndPath(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var gotBody map[string]json.RawMessage
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(arkSuccessBody))
	})

	_, err := VolcengineARK{}.Forward(context.Background(), cfg,
		arkForwardReq(`{"model":"doubao-seedream-5-0-flash","prompt":"a","size":"1024x1024"}`),
		relay.PathImagesGenerations)
	if err != nil {
		t.Fatalf("Forward 失败：%v", err)
	}
	if gotPath != arkPathImages {
		t.Fatalf("出站路径 = %q，期望 %q（平台 canonical path 不能出网）", gotPath, arkPathImages)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("方法 = %q，期望 POST", gotMethod)
	}
	if gotAuth == "" {
		t.Fatal("出站请求缺鉴权头")
	}
	if string(gotBody["model"]) != `"`+arkForwardUpstream+`"` {
		t.Fatalf("model = %s，期望上游模型名 %q", gotBody["model"], arkForwardUpstream)
	}
}

// TestArkImageForwardKeepsStreamFalse：同步端点绝不能因为客户端写了 stream=true 就变成流式。
//
// 旧实现靠 SDK 的 GenerateImages 内部 `request.SetStream(false)`；改用原始出站后这步必须自己补，
// 否则上游回 SSE、而网关按 JSON 交付，客户端会拿到一坨无法解析的帧。
func TestArkImageForwardKeepsStreamFalse(t *testing.T) {
	var gotBody map[string]json.RawMessage
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(arkSuccessBody))
	})

	_, err := VolcengineARK{}.Forward(context.Background(), cfg,
		arkForwardReq(`{"model":"m","prompt":"a","stream":true}`), relay.PathImagesGenerations)
	if err != nil {
		t.Fatalf("Forward 失败：%v", err)
	}
	if string(gotBody["stream"]) != "false" {
		t.Fatalf("同步出站必须带 stream=false，实际 %s", gotBody["stream"])
	}
}

// TestArkImageForward400IsCallerParam：上游 400 是**调用方参数错**，必须原话回 400 且不重试。
//
// 文案取上一轮真实事故的原文（参考图回取超时）：它被误判成普通 error 时，
// 用户看到的是 503「服务暂时不可用」，运维在两边的日志里都找不到原因。
func TestArkImageForward400IsCallerParam(t *testing.T) {
	const upstreamMsg = "The parameter image specified in the request are not valid: " +
		"Timeout while downloading url=https://lapi.example.com/f/01M6BS2GPNK4WP7MTNV6RN1V19/25d3f399.png"
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "InvalidParameter", "message": upstreamMsg, "type": "BadRequest"},
		})
	})

	_, err := VolcengineARK{}.Forward(context.Background(), cfg,
		arkForwardReq(`{"model":"m","prompt":"a"}`), relay.PathImagesGenerations)
	if err == nil {
		t.Fatal("上游 400 必须返回错误")
	}
	var ce *relay.CallerParamError
	if !errors.As(err, &ce) {
		t.Fatalf("上游 400 必须翻译成 CallerParamError（否则会换渠道重投、并记渠道故障），实际 %T：%v", err, err)
	}
	if ce.Msg != upstreamMsg+"（InvalidParameter）" {
		t.Fatalf("必须原样保留上游原话并附上 code（运营靠它定位），实际 %q", ce.Msg)
	}
}

// TestArkImageOutboundErrorClassifies 钉死三分类：超时≠断开≠上游拒绝。
//
// 三条分支的代价完全不同：
//   - 超时 → 504 + 停止 failover（上游可能已出图计费，重投=付两次钱）；
//   - 客户端断开 → 499 + 全额释放（不能记渠道故障）；
//   - 其它传输错误 → 归因上游、允许换候选。
//
// 混成一条的后果就是把「我们没等到」记成「上游坏了」，渠道被熔断而真正的原因是慢。
func TestArkImageOutboundErrorClassifies(t *testing.T) {
	// 客户端断开：即使 err 看起来像超时，也必须原样返回（网关靠 errors.Is 判 499）。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	timeoutErr := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	if got := arkImageOutboundError(cancelled, timeoutErr); got != timeoutErr {
		t.Fatalf("客户端断开必须原样返回，实际 %T：%v", got, got)
	}

	var igt *relay.ImageGenTimeoutError
	if got := arkImageOutboundError(context.Background(), timeoutErr); !errors.As(got, &igt) {
		t.Fatalf("超时必须翻译成 ImageGenTimeoutError（否则会换渠道重复出图），实际 %T：%v", got, got)
	}
	plain := errors.New("connection reset by peer")
	if got := arkImageOutboundError(context.Background(), plain); got != plain {
		t.Fatalf("非超时错误必须原样返回，实际 %T：%v", got, got)
	}
}

// TestArkStatusErrorKeepsEmptyBodyOn5xx：5xx 空响应体必须保持空串。
//
// IsAmbiguousSubmit 用「5xx + 空响应体」判定「上游可能已建单」，一旦我们给它补上文案，
// 网关就会认为可以换渠道重投 —— 对计费接口就是第二次真实出站。
func TestArkStatusErrorKeepsEmptyBodyOn5xx(t *testing.T) {
	cfg := &relay.ChannelConfig{ID: 1, Name: "ark", BaseURL: "https://ark.example.com/api/v3", APIKey: "k"}
	err := arkStatusError(http.StatusBadGateway, nil, cfg)
	if st := relay.StatusOf(err); st != http.StatusBadGateway {
		t.Fatalf("StatusOf = %d，期望 502", st)
	}
	var he *relay.HTTPStatusError
	if !errors.As(err, &he) {
		t.Fatalf("5xx 必须是 HTTPStatusError（网关靠它判歧义与归因），实际 %T", err)
	}
	if he.Body != "" {
		t.Fatalf("5xx 空体的 Body 必须为空串（IsAmbiguousSubmit 的唯一判据），实际 %q", he.Body)
	}
	// 4xx 反过来：必须给人话，否则上层拼出「参数不合法：」这种没有信息量的句子。
	err = arkStatusError(http.StatusTooManyRequests, nil, cfg)
	if code := relay.StatusOf(err); code != http.StatusTooManyRequests {
		t.Fatalf("4xx 也要带状态码，实际 %d", code)
	}
	var he4 *relay.HTTPStatusError
	if !errors.As(err, &he4) || he4.Body == "" {
		t.Fatalf("4xx 空体必须补一句人话，实际 %q", err.Error())
	}
}

// TestArkImageForwardTooLargeIsAmbiguous：响应读不全时**绝不能**换渠道重投。
//
// 场景：`response_format=b64_json` + 4K 组图，单响应可达数十 MB。超过读体上限时
// 上游往往**已经出完图并计费**；落成普通 error 会被网关当成渠道故障，换一条渠道
// 再生成一遍 —— 平台付两次钱、用户只收到一份。
//
// 判据必须是 AmbiguousSubmitError（网关据此 502 + 全额释放 + 打标，且不 failover）。
func TestArkImageForwardTooLargeIsAmbiguous(t *testing.T) {
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = 'x'
	}
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 65 MiB > arkMaxUpstreamBody(64 MiB)，分块写，避免测试自己吃满内存。
		for i := 0; i < 65; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})

	_, err := VolcengineARK{}.Forward(context.Background(), cfg,
		arkForwardReq(`{"model":"m","prompt":"a","response_format":"b64_json"}`), relay.PathImagesGenerations)
	if err == nil {
		t.Fatal("超限响应必须报错")
	}
	if !errors.Is(err, relay.ErrUpstreamBodyTooLarge) {
		t.Fatalf("必须保留 ErrUpstreamBodyTooLarge 哨兵，实际 %v", err)
	}
	// 判据是类型而不是 IsAmbiguousSubmit：那个函数只按「错误形状」猜（5xx 空体/超时/EOF），
	// 不看这个类型；网关的短路分支认的是 errors.As(*AmbiguousSubmitError)。
	var amb *relay.AmbiguousSubmitError
	if !errors.As(err, &amb) {
		t.Fatalf("超限必须包成 AmbiguousSubmitError（否则网关会换渠道再出一遍图），实际 %T", err)
	}
}

// TestArkImageForward5xxKeepsStatus：5xx 必须带状态码（网关按状态码判渠道健康/冷却密钥）。
func TestArkImageForward5xxKeepsStatus(t *testing.T) {
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"InternalServiceError","message":"boom"}}`))
	})

	_, err := VolcengineARK{}.Forward(context.Background(), cfg,
		arkForwardReq(`{"model":"m","prompt":"a"}`), relay.PathImagesGenerations)
	if err == nil {
		t.Fatal("上游 500 必须返回错误")
	}
	if st := relay.StatusOf(err); st != http.StatusInternalServerError {
		t.Fatalf("StatusOf = %d，期望 500（普通 error 会让网关看不到状态码）", st)
	}
}

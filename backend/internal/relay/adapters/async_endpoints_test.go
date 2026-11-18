package adapters

import (
	"context"
	"errors"
	"testing"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// 本组用例守「异步能力按端点声明」这一件事，三面：
//  1. 声明必须落在平台词表内（否则保存期校验与运行期过滤用的不是同一个集合）；
//  2. 每家到底承接哪几个端点（矩阵，改动时立刻可见）；
//  3. 提交闸门：越界端点零出站（否则表现是上游 502，没人会去查配置）。

// TestAsyncEndpointsInVocabulary 断言所有内置适配器声明的端点都在 apidoc 词表内。
//
// 词表唯一源是 apidoc.TaskEndpointKeys()（Phase=submit && CapabilityChecked）。
// 这条用例的价值：新增一个异步端点时，适配器可以先加声明，但只要忘了把
// CapabilityChecked 置 true，这里立刻红 —— 而不是等到运营保存模型时
// 报「没有任何启用绑定能承接」（那时完全看不出是词表漏登记）。
func TestAsyncEndpointsInVocabulary(t *testing.T) {
	vocab := map[string]bool{}
	for _, k := range apidoc.TaskEndpointKeys() {
		vocab[k] = true
	}
	if len(vocab) == 0 {
		t.Fatal("平台异步端点词表为空：TaskEndpointKeys 派生条件被改坏了")
	}

	for _, name := range []string{"openai", "volcengine-ark", "autodl", "metaso", "duomiapi"} {
		a, ok := relay.GetAdapter(name)
		if !ok {
			t.Fatalf("适配器 %s 未注册", name)
		}
		s, ok := a.(relay.AsyncEndpointSupplier)
		if !ok {
			t.Fatalf("内置适配器 %s 必须显式声明 AsyncEndpoints（不能吃第三方默认值）", name)
		}
		eps := s.AsyncEndpoints()
		if len(eps) == 0 {
			t.Fatalf("适配器 %s 声明了空端点集，等于声明「不做异步」", name)
		}
		for _, e := range eps {
			if !vocab[apidoc.NormalizeEndpointKey(e)] {
				t.Fatalf("适配器 %s 声明了词表外的端点 %q（词表：%v）", name, e, apidoc.TaskEndpointKeys())
			}
		}
	}
}

// TestTaskEndpointMatrix 是能力矩阵本身：改任何一家的声明都会在这里出现 diff。
func TestTaskEndpointMatrix(t *testing.T) {
	const (
		video  = "video/generations"
		regen  = "video/regenerations"
		imgA   = "images/generations"
		music  = "audio/music/generations"
		lyrics = "audio/lyrics"
	)
	cases := []struct {
		adapter string
		key     string
		want    bool
		why     string
	}{
		{"openai", video, true, "OpenAI 兼容渠道有视频生成"},
		{"openai", imgA, false, "SubmitTask 固定打视频端点，图像（同路径 async）会 502"},
		{"openai", music, false, "没有音乐上游契约"},
		{"volcengine-ark", video, true, ""},
		{"volcengine-ark", music, false, ""},
		{"autodl", video, true, ""},
		{"autodl", imgA, false, ""},
		{"metaso", video, true, ""},
		{"metaso", regen, true, "唯一有再生成语义的上游"},
		{"metaso", music, false, ""},
		{"duomiapi", video, true, ""},
		{"duomiapi", imgA, true, "nano / gpt-image（同一个路径，async:true 才转任务）"},
		{"duomiapi", music, true, "SUNO"},
		{"duomiapi", lyrics, true, "SUNO 歌词"},
		{"duomiapi", regen, false, "多米没有再生成"},
	}
	for _, c := range cases {
		if got := relay.TaskEndpointSupported(c.adapter, c.key); got != c.want {
			t.Errorf("%s × %s = %v，期望 %v %s", c.adapter, c.key, got, c.want, c.why)
		}
	}

	// 未注册适配器一律 false：否则一个拼错的 adapter_id 会被当成"能做异步"。
	if relay.TaskEndpointSupported("no-such-adapter", video) {
		t.Fatal("未注册适配器不得被判定为可承接")
	}
	// 空 key 只要求「能做异步」：重试路径没有入站端点。
	if !relay.TaskEndpointSupported("openai", "") {
		t.Fatal("空 key 应退化为「能做异步」判定")
	}
	if relay.TaskEndpointSupported("no-such-adapter", "") {
		t.Fatal("未注册适配器空 key 也必须 false")
	}
}

// countingTask 只用来证明「闸门拒绝时一次都没出站」。
type countingTask struct{ calls int }

func (c *countingTask) SubmitTask(context.Context, *relay.ChannelConfig, *relay.TaskSubmitRequest) (string, error) {
	c.calls++
	return "upstream-1", nil
}

func (c *countingTask) PollTask(context.Context, *relay.ChannelConfig, string) (*relay.TaskStatus, error) {
	return &relay.TaskStatus{Status: taskstate.Running}, nil
}

// TestSubmitTaskOfRejectsForeignEndpoint 守提交管线上的能力闸门。
//
// 为什么必须在提交前拦：越界端点打到上游会得到 401/404/400，被归类成"上游故障"
// 并记进渠道健康 —— 一条配错的绑定足以把一条好渠道熔断，而真正要改的是绑定。
func TestSubmitTaskOfRejectsForeignEndpoint(t *testing.T) {
	cfg := &relay.ChannelConfig{ID: 1, Name: "c", BaseURL: "https://example.com"}
	ta := &countingTask{}

	_, err := relay.SubmitTaskOf(context.Background(), "openai", ta, cfg,
		&relay.TaskSubmitRequest{EndpointKey: "audio/lyrics"})
	if err == nil {
		t.Fatal("越界端点必须报错")
	}
	var ce *relay.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("应为 ConfigError（平台配置问题），实际 %T：%v", err, err)
	}
	if ta.calls != 0 {
		t.Fatalf("闸门拒绝时不得出站，实际调用 %d 次", ta.calls)
	}

	// 命中端点：正常透传，且不吞掉适配器的返回值。
	id, err := relay.SubmitTaskOf(context.Background(), "openai", ta, cfg,
		&relay.TaskSubmitRequest{EndpointKey: "/v1/video/generations"})
	if err != nil {
		t.Fatalf("带前导斜杠的写法也要认得（NormalizeEndpointKey）：%v", err)
	}
	if id != "upstream-1" || ta.calls != 1 {
		t.Fatalf("应透传一次提交，实际 id=%q calls=%d", id, ta.calls)
	}

	// 空 EndpointKey（重试路径）：只要求能异步，直接放行。
	ta2 := &countingTask{}
	if _, err := relay.SubmitTaskOf(context.Background(), "openai", ta2, cfg, &relay.TaskSubmitRequest{}); err != nil {
		t.Fatalf("空端点应放行：%v", err)
	}
	if ta2.calls != 1 {
		t.Fatal("空端点应真的调一次适配器")
	}
}

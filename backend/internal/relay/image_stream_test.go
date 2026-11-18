package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// 本文件覆盖「图片流式」的两个纯函数：
//   - imageStreamBillUsage：部分交付时到底收几张的钱；
//   - rewriteImageFrameURL：帧里的 url 改写后其它键必须还在。
//
// 为什么单独测这两处：它们都在**计费与对外响应**的路径上，出错不会报错，
// 只会安静地多收 / 少收，或者让前端拿到一个打不开的地址。

// ① 张数结算：多退少不补，且永远至少 1 张（0 张 = 白送一次生成）。
func TestImageStreamBillUsage(t *testing.T) {
	spec := billing.Usage{ImageSpec: "2K", ImageQuality: "high"}
	cases := []struct {
		name       string
		res        *ImageStreamResult
		wantN      int64
		wantSource string
		wantAnom   string
	}{
		{
			name: "生成数与交付数一致", res: &ImageStreamResult{Delivered: 4, GeneratedImages: 4},
			wantN: 4, wantSource: "upstream", wantAnom: "",
		},
		{
			name: "部分交付只收交付的那几张", res: &ImageStreamResult{Delivered: 2, GeneratedImages: 4},
			wantN: 2, wantSource: "upstream", wantAnom: model.AnomalyStreamPartialDelivered,
		},
		{
			name: "上游没报张数时按交付数结算", res: &ImageStreamResult{Delivered: 3},
			wantN: 3, wantSource: "estimated", wantAnom: "",
		},
		{
			name: "一帧都没交付也要按 1 张收（否则等于免费）", res: &ImageStreamResult{},
			wantN: 1, wantSource: "estimated", wantAnom: "",
		},
		{
			name: "适配器没返回结果时的兜底", res: nil,
			wantN: 1, wantSource: "estimated", wantAnom: "",
		},
	}
	for _, c := range cases {
		usage, source, anom := imageStreamBillUsage(spec, c.res, 0)
		if usage.ImageN != c.wantN {
			t.Fatalf("%s：张数应为 %d，实际 %d", c.name, c.wantN, usage.ImageN)
		}
		if source != c.wantSource {
			t.Fatalf("%s：计费来源应为 %q，实际 %q", c.name, c.wantSource, source)
		}
		if anom != c.wantAnom {
			t.Fatalf("%s：异常标记应为 %q，实际 %q", c.name, c.wantAnom, anom)
		}
		// 档位与质量必须从请求快照继承：丢了它们，2K 的图会按 1K 收。
		if usage.ImageSpec != spec.ImageSpec || usage.ImageQuality != spec.ImageQuality {
			t.Fatalf("%s：档位/质量没有继承请求快照：%+v", c.name, usage)
		}
	}
}

// ② 帧 URL 改写：只换 url 的值，同帧其它键（前端靠它排序与展示）必须保留。
func TestRewriteImageFrameURLKeepsOtherKeys(t *testing.T) {
	// 签名依赖主密钥：不设的话 Link 会返回 false，测试变成「什么都没发生也通过」。
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	conf.Load()

	frame := []byte(`{"type":"image_generation.result","image_index":2,"size":"2048x2048","url":"https://cdn.example.com/a.png"}`)
	out, ok := rewriteImageFrameURL(frame, "https://api.platform.test")
	if !ok {
		t.Fatal("含 url 的帧应该被改写")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("改写后不是合法 JSON：%v", err)
	}
	if m["image_index"] != float64(2) || m["size"] != "2048x2048" || m["type"] != "image_generation.result" {
		t.Fatalf("改写丢了其它键：%v", m)
	}
	u, _ := m["url"].(string)
	if u == "" || u == "https://cdn.example.com/a.png" {
		t.Fatalf("url 没有被改写：%v", m["url"])
	}
	if !strings.Contains(u, "api.platform.test") {
		t.Fatalf("改写后的地址应指向平台自身：%v", u)
	}
	if strings.Contains(u, "cdn.example.com") {
		t.Fatalf("改写后仍残留上游域名（泄漏提供方）：%v", u)
	}
}

// ③ 不该改写的帧必须原样透传（ok=false），调用方据此走零拷贝路径。
func TestRewriteImageFrameURLLeavesOtherFramesAlone(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		base  string
	}{
		{"没有 url 键（usage 帧）", `{"type":"image_generation.completed","usage":{"total_tokens":9}}`, "https://api.platform.test"},
		{"url 不是字符串", `{"url":123}`, "https://api.platform.test"},
		{"空 url", `{"url":""}`, "https://api.platform.test"},
		{"帧不是对象", `"ping"`, "https://api.platform.test"},
		{"外部基址推不出来", `{"url":"https://cdn.example.com/a.png"}`, ""},
	}
	for _, c := range cases {
		out, ok := rewriteImageFrameURL([]byte(c.frame), c.base)
		if ok {
			t.Fatalf("%s：不该改写，实际改成了 %s", c.name, out)
		}
		if string(out) != c.frame {
			t.Fatalf("%s：不该改写时字节必须原样，实际 %s", c.name, out)
		}
	}
}

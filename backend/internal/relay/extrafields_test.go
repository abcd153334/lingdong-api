package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

// rawText 取出补入值的原始 JSON 文本（补入的是 json.RawMessage）。
func rawText(v any) string {
	switch x := v.(type) {
	case json.RawMessage:
		return string(x)
	case string:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return ""
}

// ForwardUnmapped 是「未声明字段透传」的唯一实现，这几条断言就是它的全部语义。
func TestForwardUnmapped(t *testing.T) {
	t.Run("未知键原样补入", func(t *testing.T) {
		dst := map[string]any{"model": "upstream-m", "prompt": "p"}
		ForwardUnmapped(dst, []byte(`{"model":"client-m","prompt":"p","output_format":"png","seed":7}`))
		// 补进去的是原始 JSON 字节（RawMessage），不是解码后的 any：
		// 只有这样才能保住大整数与小数写法。
		if got := rawText(dst["output_format"]); got != `"png"` {
			t.Fatalf("output_format 未透传：%v", dst)
		}
		if got, ok := dst["seed"].(json.RawMessage); !ok || string(got) != "7" {
			t.Fatalf("seed 未以原始字节补入：%#v", dst["seed"])
		}
	})

	t.Run("映射写过的键不被覆盖", func(t *testing.T) {
		dst := map[string]any{"model": "upstream-m"}
		ForwardUnmapped(dst, []byte(`{"model":"client-m","prompt":"p"}`))
		if dst["model"] != "upstream-m" {
			t.Fatalf("model 被客户端覆盖了：%v", dst["model"])
		}
		if got := rawText(dst["prompt"]); got != `"p"` {
			t.Fatalf("未映射的 prompt 应透传：%v", dst)
		}
	})

	t.Run("localOnly 生效", func(t *testing.T) {
		dst := map[string]any{"model": "m"}
		ForwardUnmapped(dst, []byte(`{"response_format":"b64_json","seconds":8,"quality":"high"}`),
			"response_format", "seconds")
		for _, k := range []string{"response_format", "seconds"} {
			if _, ok := dst[k]; ok {
				t.Fatalf("%s 是本地下发键，不该出现在上游报文：%v", k, dst)
			}
		}
		if got := rawText(dst["quality"]); got != `"high"` {
			t.Fatalf("quality 应透传：%v", dst)
		}
	})

	t.Run("null 跳过", func(t *testing.T) {
		// 转发 null 会被多数上游判非法，而 JSON 语义上它就是「没设置」。
		dst := map[string]any{"model": "m"}
		ForwardUnmapped(dst, []byte(`{"mask":null,"background":""}`))
		if _, ok := dst["mask"]; ok {
			t.Fatalf("null 不应透传：%v", dst)
		}
		if got := rawText(dst["background"]); got != `""` {
			t.Fatalf("空串是用户显式写的值，应透传：%v", dst)
		}
	})

	t.Run("大整数逐字节保留", func(t *testing.T) {
		dst := map[string]any{"model": "m"}
		ForwardUnmapped(dst, []byte(`{"seed":1234567890123}`))
		if got := string(dst["seed"].(json.RawMessage)); got != "1234567890123" {
			t.Fatalf("大整数被改写：%s", got)
		}
	})

	t.Run("非对象静默返回", func(t *testing.T) {
		dst := map[string]any{"model": "m"}
		for _, raw := range []string{"", "null", `[]`, `"x"`, `{bad`} {
			ForwardUnmapped(dst, []byte(raw))
		}
		if len(dst) != 1 {
			t.Fatalf("非对象输入不该补键：%v", dst)
		}
	})
}

func TestStripTopLevelKey(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"prompt":"p"}`)
	out := StripTopLevelKey(body, "stream")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("输出不是合法 JSON：%s", out)
	}
	if _, ok := m["stream"]; ok {
		t.Fatalf("stream 未删除：%s", out)
	}
	if string(m["prompt"]) != `"p"` {
		t.Fatalf("其余字段被改动：%s", out)
	}

	// 键不存在时逐字节原样返回（不能为了删一个不存在的键改动报文）。
	keep := []byte(`{"model":"m","n":1.50}`)
	if got := string(StripTopLevelKey(keep, "stream")); got != string(keep) {
		t.Fatalf("无该键时不该重排报文：%s", got)
	}
}

// TestImageOutboundBodyStripsStream 是「图片响应形态由平台决定」的回归：
// 非流式候选绝不能把客户端的 stream:true 发给上游。
func TestImageOutboundBodyStripsStream(t *testing.T) {
	body := []byte(`{"model":"m","prompt":"p","stream":true}`)

	got := string(imageOutboundBody(body, false))
	if strings.Contains(got, `"stream"`) {
		t.Fatalf("非流式候选必须剥掉 stream：%s", got)
	}
	if !strings.Contains(got, `"prompt":"p"`) {
		t.Fatalf("其余字段不能动：%s", got)
	}

	// 流式候选（Ark 自己会覆写 stream）保持原样，由适配器决定取值。
	if string(imageOutboundBody(body, true)) != string(body) {
		t.Fatalf("流式候选不该被改写：%s", imageOutboundBody(body, true))
	}
}

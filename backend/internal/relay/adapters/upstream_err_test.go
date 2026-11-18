package adapters

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// 本文件守「上游错误文案」的唯一提取实现：识别顺序与三态形状。
//
// 改造前四份实现各支持不同字段（多米读 message、可灵读 message、metaso 读
// message/code、火山读 error.message），同一种故障在不同渠道显示不同详细度，
// 甚至只剩「上游返回业务错误」。

// TestUpstreamErrEnvelopeTextOrder 固定识别顺序：message > msg > error.* > data > base_resp。
func TestUpstreamErrEnvelopeTextOrder(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		want string
	}{
		{"message 优先", `{"message":"顶层 message","msg":"顶层 msg"}`, "顶层 message"},
		{"无 message 用 msg", `{"msg":"顶层 msg","error":{"message":"嵌套"}}`, "顶层 msg"},
		{"error 字符串", `{"error":"字符串错误"}`, "字符串错误"},
		{"error 对象", `{"error":{"message":"嵌套错误","code":"E1"}}`, "嵌套错误"},
		{"error 数组拼接", `{"error":[{"message":"字段A必填"},{"message":"字段B非法"}]}`, "字段A必填; 字段B非法"},
		{"data.message", `{"data":{"message":"数据层原因"}}`, "数据层原因"},
		{"base_resp", `{"base_resp":{"status_code":1001,"status_msg":"内容审核未通过"}}`, "内容审核未通过"},
		{"全都取不到", `{"code":400,"data":{}}`, ""},
		{"非 JSON", `<html>502 Bad Gateway</html>`, ""},
	}
	for _, c := range cases {
		if got := envelopeTextOf([]byte(c.buf)); got != c.want {
			t.Fatalf("%s：got %q want %q", c.name, got, c.want)
		}
	}
}

// TestErrTextFromValueShapes 三态（字符串/对象/数组）+ 空值边界。
func TestErrTextFromValueShapes(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"plain"`, "plain"},
		{`{"message":"m"}`, "m"},
		{`{"msg":"s"}`, "s"},
		// code 只在没有 message/msg 时兜底，且必须是可读字符串：
		// 数字回 "400" 对用户毫无意义。
		{`{"code":"invalid_api_key"}`, "invalid_api_key"},
		{`{"code":400}`, ""},
		{`["a","b"]`, "a; b"},
		{`[]`, ""},
		{`{}`, ""},
		{`null`, ""},
		{``, ""},
	}
	for _, c := range cases {
		if got := errTextFromValue(json.RawMessage(c.raw)); got != c.want {
			t.Fatalf("%s：got %q want %q", c.raw, got, c.want)
		}
	}
}

// TestMetasoEnvelopeReadsTopLevelMsg metaso 的顶层 msg 也要能取到（此前只读 message/error/code）。
func TestMetasoEnvelopeReadsTopLevelMsg(t *testing.T) {
	code, msg, ok := metasoErrorEnvelope([]byte(`{"type":"error","msg":"余额不足"}`))
	if !ok {
		t.Fatal("type=error 应识别为错误信封")
	}
	if code != 0 || msg != "余额不足" {
		t.Fatalf("got code=%d msg=%q", code, msg)
	}
}

// TestArkErrTextStillAppendsCode 火山的 messages 后面要带 code（运营靠它定位）。
func TestArkErrTextStillAppendsCode(t *testing.T) {
	got := arkErrText([]byte(`{"error":{"code":"InvalidParameter","message":"size 非法"}}`))
	if got != "size 非法（InvalidParameter）" {
		t.Fatalf("got %q", got)
	}
	// 取不到 message 时退化为原文片段，绝不返回空串。
	if got := arkErrText([]byte(`plain text`)); got != "plain text" {
		t.Fatalf("无信封时应回原文，实际 %q", got)
	}
}

// TestSnippetNeverSplitsUTF8 按字节截断会把中文劈成非法序列（前端直接乱码）。
func TestSnippetNeverSplitsUTF8(t *testing.T) {
	// 199 个 ASCII + 1 个汉字：按字节切正好切断汉字，按字符切则整段保留。
	body := strings.Repeat("a", 199) + "错"
	got := snippet([]byte(body))
	if !utf8.ValidString(got) {
		t.Fatalf("截断产生了非法 UTF-8：%q", got)
	}
	if n := len([]rune(got)); n != 200 {
		t.Fatalf("应按字符截到 200，实际 %d", n)
	}
}

// TestSnippetRedactsKeys 适配器贴的原文里常有 Authorization 回显。
func TestSnippetRedactsKeys(t *testing.T) {
	body := []byte(`{"error":"bad key","request":"Authorization: Bearer sk-live-abcdefghijkl"}`)
	got := snippet(body)
	if strings.Contains(got, "sk-live-abcdefghijkl") {
		t.Fatalf("密钥未脱敏：%q", got)
	}
	if !strings.Contains(got, "<redacted-key>") {
		t.Fatalf("应写入占位符，实际 %q", got)
	}
}

// TestUpstreamDetailFallsBack 取不到原因时必须明说「上游没给」，不能留空。
func TestUpstreamDetailFallsBack(t *testing.T) {
	if got := upstreamDetail([]byte(`{"code":500,"data":{}}`), 500); got != "上游未给出原因（HTTP 500）" {
		t.Fatalf("got %q", got)
	}
	// 非 JSON（网关错误页）只能贴原文片段。
	if got := upstreamDetail([]byte(`Bad Gateway`), 502); got != "Bad Gateway" {
		t.Fatalf("got %q", got)
	}
	// 有信封取信封那一句。
	if got := upstreamDetail([]byte(`{"code":400,"msg":"bad size"}`), 400); got != "bad size" {
		t.Fatalf("got %q", got)
	}
}

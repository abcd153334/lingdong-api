package model

import "testing"

// TestMediaKindPrefersContentOverModality 锁死「类型按内容判，不按端点模态」。
//
// 这条规则对应一类线上投诉："详情里的视频预览打不开"。真实数据是
// video 端点返回的封面/缩略图（.jpg），按模态写成 video 就会用 <video> 渲染一张图。
func TestMediaKindPrefersContentOverModality(t *testing.T) {
	cases := []struct {
		name     string
		mime     string
		url      string
		fallback string
		want     string
	}{
		{"MIME 优先", "image/png", "https://x.test/a.mp4", "video", "image"},
		{"无 MIME 看扩展名", "", "https://x.test/a.mp4", "image", "video"},
		{"都没有才用模态", "", "https://x.test/download/1", "audio", "audio"},
		{"什么都没有", "", "", "", "file"},
		{"带参数的 MIME", "image/jpeg; charset=binary", "", "", "image"},
		{"签名链接里的假后缀不算", "", "https://x.test/dl/1?name=a.png&sig=x", "audio", "audio"},
		{"SVG 不内联", "image/svg+xml", "https://x.test/a.svg", "image", "file"},
		{"JSON 按文本", "application/json", "", "", "text"},
		{"PDF 不内联", "application/pdf", "", "", "file"},
		{"data URL 不猜扩展名", "", "data:image/png;base64,AAAA", "image", "image"},
	}
	for _, c := range cases {
		if got := MediaKind(c.mime, c.url, c.fallback); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestMIMETypeOfURLIgnoresQuery 只看路径：上游签名链接常在 query 里带文件名的假后缀。
func TestMIMETypeOfURLIgnoresQuery(t *testing.T) {
	if got := MIMETypeOfURL("https://cdn.test/v/1?filename=poster.jpg"); got != "" {
		t.Fatalf("query 里的后缀不该被采信，实际 %q", got)
	}
	if got := MIMETypeOfURL("https://cdn.test/a.PNG"); got != "image/png" {
		t.Fatalf("扩展名应大小写不敏感，实际 %q", got)
	}
	if got := MIMETypeOfURL("/files/a.mp4?x=1"); got != "video/mp4" {
		t.Fatalf("相对路径应能取到扩展名，实际 %q", got)
	}
}

// TestEffectiveContentType 处理"上游只给 octet-stream"的常态。
//
// 不补的话，产物在门户会显示成「文件」、<video> 也打不开（blob 的 type 决定解码器）。
func TestEffectiveContentType(t *testing.T) {
	f := &File{Filename: "task-9-0.png", ContentType: "application/octet-stream"}
	if got := f.EffectiveContentType(); got != "image/png" {
		t.Fatalf("octet-stream 应按文件名补成 image/png，实际 %q", got)
	}
	f = &File{ContentType: "video/mp4"}
	if got := f.EffectiveContentType(); got != "video/mp4" {
		t.Fatalf("已知类型不应被改写，实际 %q", got)
	}
	f = &File{Filename: "artifact.bin"}
	if got := f.EffectiveContentType(); got != "" {
		t.Fatalf("认不出时应保持空，实际 %q", got)
	}
}

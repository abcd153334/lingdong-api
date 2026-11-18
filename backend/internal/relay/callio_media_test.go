package relay

import "testing"

// TestMediaFromBodyShapes 覆盖各渠道真实出现过的响应形状。
//
// 这些用例的存在理由：线上"很多模型没有结果可看"的根因就是
// mediaFromBody 只认 OpenAI 的 data[].url 一种形状，其余全部静默返回空。
func TestMediaFromBodyShapes(t *testing.T) {
	cases := []struct {
		name   string
		kind   endpointKind
		body   string
		want   int
		url    string
		kindOf string
	}{
		{
			name: "OpenAI data[].url", kind: kindImagesGenerations,
			body: `{"data":[{"url":"https://cdn.test/a.png"},{"url":"https://cdn.test/b.png"}]}`,
			want: 2, url: "https://cdn.test/a.png", kindOf: "image",
		},
		{
			name: "多米 output 字符串数组", kind: kindImagesGenerations,
			body: `{"output":["https://cdn.test/o1.png"]}`,
			want: 1, url: "https://cdn.test/o1.png", kindOf: "image",
		},
		{
			name: "images 数组", kind: kindImagesGenerations,
			body: `{"images":["https://cdn.test/i1.png"]}`,
			want: 1, url: "https://cdn.test/i1.png", kindOf: "image",
		},
		{
			name: "result.data 嵌套", kind: kindImagesGenerations,
			body: `{"result":{"data":[{"url":"https://cdn.test/r1.png"}]}}`,
			want: 1, url: "https://cdn.test/r1.png", kindOf: "image",
		},
		{
			name: "data[].image_url", kind: kindImagesGenerations,
			body: `{"data":[{"image_url":"https://cdn.test/x.png","mime_type":"image/webp"}]}`,
			want: 1, url: "https://cdn.test/x.png", kindOf: "image",
		},
		{
			name: "顶层单链接", kind: kindImagesGenerations,
			body: `{"url":"https://cdn.test/top.png"}`,
			want: 1, url: "https://cdn.test/top.png", kindOf: "image",
		},
		{
			name: "视频端点 videos 数组",
			kind: kindImagesGenerations,
			body: `{"videos":[{"video_url":"https://cdn.test/v.mp4"}]}`,
			want: 1, url: "https://cdn.test/v.mp4", kindOf: "video",
		},
		{
			// 类型按**内容**判，不按端点：video 端点回一张 jpg 封面时，
			// 按模态写成 video 会让前端用 <video> 渲染一张图（黑屏 + 控制条）。
			name: "视频端点回图片封面",
			kind: kindImagesGenerations,
			body: `{"data":[{"url":"https://cdn.test/cover.jpg"}]}`,
			want: 1, url: "https://cdn.test/cover.jpg", kindOf: "image",
		},
		{
			// 上游只给链接、不给 mime_type 是常态：MIME 由扩展名补齐，
			// 否则详情页只剩一个「文件」标签。
			name: "无 mime_type 时按扩展名补齐",
			kind: kindImagesGenerations,
			body: `{"output":["https://cdn.test/no-mime.webp"]}`,
			want: 1, url: "https://cdn.test/no-mime.webp", kindOf: "image",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mediaFromBody(c.kind, []byte(c.body))
			if len(got) != c.want {
				t.Fatalf("应提取 %d 项，实际 %d 项：%+v", c.want, len(got), got)
			}
			if got[0].URL != c.url {
				t.Fatalf("URL 应为 %q，实际 %q", c.url, got[0].URL)
			}
			if got[0].Kind != c.kindOf {
				t.Fatalf("Kind 应为 %q，实际 %q", c.kindOf, got[0].Kind)
			}
			// 同步响应不是任务产物：artifact_index 必须是 -1，
			// 否则前端会拿 (task_id, 0) 去请求产物端点。
			if got[0].ArtifactIndex != -1 {
				t.Fatalf("同步媒体 ArtifactIndex 应为 -1，实际 %d", got[0].ArtifactIndex)
			}
		})
	}
}

// TestMediaFromBodyB64 内联数据只记大小，不留内容（零转存红线）。
func TestMediaFromBodyB64(t *testing.T) {
	got := mediaFromBody(kindImagesGenerations, []byte(
		`{"data":[{"b64_json":"aGVsbG8gd29ybGQ=","mime_type":"image/png"}]}`))
	if len(got) != 1 {
		t.Fatalf("应 1 项，实际 %d", len(got))
	}
	if got[0].URL != "" {
		t.Fatalf("内联数据不得有 URL，实际 %q", got[0].URL)
	}
	if got[0].Bytes <= 0 {
		t.Fatalf("应记录解码后大小，实际 %d", got[0].Bytes)
	}
	if got[0].Note == "" {
		t.Fatal("必须说明内容未留存")
	}
}

// TestMediaFromBodyIgnoresChat 非媒体端点不提取（避免把 {"url":"docs"} 误判成媒体）。
func TestMediaFromBodyIgnoresChat(t *testing.T) {
	if got := mediaFromBody(kindChat, []byte(`{"data":[{"url":"https://cdn.test/a.png"}]}`)); len(got) != 0 {
		t.Fatalf("chat 端点不应提取媒体，实际 %+v", got)
	}
}

// TestMediaFromBodyCaps 最多 20 项（media 是 jsonb 列，不能无限涨）。
func TestMediaFromBodyCaps(t *testing.T) {
	body := `{"output":[`
	for i := 0; i < 30; i++ {
		if i > 0 {
			body += ","
		}
		body += `"https://cdn.test/` + string(rune('a'+i)) + `.png"`
	}
	body += `]}`
	if got := mediaFromBody(kindImagesGenerations, []byte(body)); len(got) != maxMediaItems {
		t.Fatalf("应截到 %d 项，实际 %d", maxMediaItems, len(got))
	}
}

// TestMediaFromBodyRelative 相对路径不拼 base_url，但必须留说明。
func TestMediaFromBodyRelative(t *testing.T) {
	got := mediaFromBody(kindImagesGenerations, []byte(`{"output":["/files/a.png"]}`))
	if len(got) != 1 || got[0].URL != "/files/a.png" {
		t.Fatalf("应原样保留相对路径，实际 %+v", got)
	}
	if got[0].Note == "" {
		t.Fatal("相对路径必须注明需补全域名")
	}
}

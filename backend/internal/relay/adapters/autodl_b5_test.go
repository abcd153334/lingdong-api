package adapters

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lingdong-api/internal/relay"
)

// TestPickAutoDLArtifacts 锁死多产物保留规则（P1-28）。
//
// 改造前只挑一条视频：一次返回「视频 + 封面图 + 音轨」的任务会把后两者直接丢掉，
// 而上游是按多产物计费的 —— 钱付了、东西没交付。
func TestPickAutoDLArtifacts(t *testing.T) {
	cases := []struct {
		name    string
		results []autodlResult
		want    []string
	}{
		{
			name:    "空结果",
			results: nil,
			want:    nil,
		},
		{
			name:    "只有视频",
			results: []autodlResult{{URL: "https://cdn/v.mp4", Type: "video", FileType: "mp4"}},
			want:    []string{"https://cdn/v.mp4"},
		},
		{
			name: "封面在前、视频在后 → 视频必须排第一",
			results: []autodlResult{
				{URL: "https://cdn/cover.png", Type: "image", FileType: "png"},
				{URL: "https://cdn/v.mp4", Type: "video", FileType: "mp4"},
			},
			want: []string{"https://cdn/v.mp4", "https://cdn/cover.png"},
		},
		{
			name: "多产物全部保留（视频优先，其余按上游顺序）",
			results: []autodlResult{
				{URL: "https://cdn/a.png", Type: "image"},
				{URL: "https://cdn/v1.mp4", Type: "video"},
				{URL: "https://cdn/m.mp3", Type: "audio"},
				{URL: "https://cdn/v2.mp4", Type: "video"},
			},
			want: []string{"https://cdn/v1.mp4", "https://cdn/v2.mp4", "https://cdn/a.png", "https://cdn/m.mp3"},
		},
		{
			name: "type 缺失时按 file_type 判视频",
			results: []autodlResult{
				{URL: "https://cdn/x", FileType: "MP4"},
				{URL: "https://cdn/y", FileType: "png"},
			},
			want: []string{"https://cdn/x", "https://cdn/y"},
		},
		{
			name: "空 URL 被剔除",
			results: []autodlResult{
				{URL: "   ", Type: "video"},
				{URL: "https://cdn/v.mp4", Type: "video"},
			},
			want: []string{"https://cdn/v.mp4"},
		},
		{
			name: "只有非视频产物时也保留（避免整单被误判失败）",
			results: []autodlResult{
				{URL: "https://cdn/i.png", Type: "image", FileType: "png"},
			},
			want: []string{"https://cdn/i.png"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pickAutoDLArtifacts(c.results)
			if len(got) != len(c.want) {
				t.Fatalf("产物数量 %d，期望 %d：%v", len(got), len(c.want), got)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("第 %d 个产物 %q，期望 %q（全量 %v）", i, got[i], c.want[i], got)
				}
			}
		})
	}
}

// TestAutoDLMultiArtifactPoll 验证查询链路真的把多产物写进 TaskStatus。
func TestAutoDLMultiArtifactPoll(t *testing.T) {
	srv, _, _ := newAutoDLServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"Success","msg":"","data":{"status":"completed","task_id":"t1","results":[
			{"url":"https://cdn/cover.png","type":"image","file_type":"png","output_type":"output"},
			{"url":"https://cdn/v.mp4","type":"video","file_type":"mp4","output_type":"output"}]}}`))
	})
	st, err := (AutoDL{}).PollTask(context.Background(), autoDLCfg(srv.URL, nil), "t1")
	if err != nil {
		t.Fatalf("轮询失败：%v", err)
	}
	if st.Status != "succeeded" {
		t.Fatalf("状态应为 succeeded，实际 %s", st.Status)
	}
	if len(st.ResultURLs) != 2 {
		t.Fatalf("应保留 2 个产物，实际 %v", st.ResultURLs)
	}
	if st.ResultURL != "https://cdn/v.mp4" {
		t.Fatalf("主产物必须是视频，实际 %q", st.ResultURL)
	}
}

// TestAutoDL404HintsBaseURL 验证 404 的排障文案给出可执行检查项（P2-05）。
func TestAutoDL404HintsBaseURL(t *testing.T) {
	srv, _, _ := newAutoDLServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"msg":"not found"}`))
	})
	_, err := submitAutoDL(t, srv.URL, `{"prompt":"p","seconds":5,"resolution":"480p"}`)
	if err == nil {
		t.Fatal("404 必须报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "/api") {
		t.Fatalf("文案应提示 Base URL 可能多写了 /api：%v", err)
	}
	// 必须保留状态码：异步链路据此判定「上游明确拒绝」，不做歧义重投。
	if relay.StatusOf(err) != http.StatusNotFound {
		t.Fatalf("应保留 404 状态码，实际 %d", relay.StatusOf(err))
	}
}

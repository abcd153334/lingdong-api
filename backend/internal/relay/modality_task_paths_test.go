package relay

import "testing"

// TestTaskEndpointPathsAreProbeable 锁死「异步端点清单自洽」。
//
// 为什么需要这条：TaskEndpointPaths 一旦与 router 的真实路由不一致，
// 后果是**静默**的 —— 声明了错路径的模型会被 ProbeEndpointPath 判成「未知」，
// 从而跳过保存期的渠道能力校验，直到第一次调用才 404。
// 真实事故：清单里长期写着 music/generations，而真实路由是 audio/music/generations，
// 于是音乐模型的端点声明从来没有被校验过。
//
// 这里不能直接 import router（controller → relay，会成环），
// 所以校验的是「清单自身可被探针识别」，路由一致性由下面这份注释锚定：
//
//	/v1/video/generations         → controller.VideoCreate
//	/v1/video/regenerations       → controller.VideoRegenerate
//	/v1/images/generations        → controller.ImagesCreate（同步，带 async:true 转任务）
//	/v1/audio/music/generations   → controller.MusicCreate
//	/v1/audio/lyrics              → controller.LyricsCreate
func TestTaskEndpointPathsAreProbeable(t *testing.T) {
	want := map[string]bool{
		"video/generations":       true,
		"video/regenerations":     true,
		"images/generations":      true,
		"audio/music/generations": true,
		"audio/lyrics":            true,
	}
	if len(TaskEndpointPaths) != len(want) {
		t.Fatalf("异步端点数量变了（%d），请同步核对 router.go 的路由与 capabilityOf",
			len(TaskEndpointPaths))
	}
	seen := make(map[string]bool, len(TaskEndpointPaths))
	for _, p := range TaskEndpointPaths {
		if seen[p] {
			t.Fatalf("异步端点清单有重复项：%s", p)
		}
		seen[p] = true
		if !want[p] {
			t.Fatalf("未知的异步端点 %q：router 里没有对应路由", p)
		}
		pr := ProbeEndpointPath(p)
		if !pr.Known {
			t.Fatalf("%q 应被识别为已知端点", p)
		}
		// 图像是唯一「同路径可同步可异步」的端点：探针先按同步表命中，
		// 因此 IsTask=false 是**正确**的 —— 保存期该走同步能力校验（渠道能出图即可承接），
		// 提交期是否允许任务模式由 AsyncEndpoints 声明决定。
		if p != "images/generations" && !pr.IsTask {
			t.Fatalf("%q 应被识别为异步端点（IsTask=%v）", p, pr.IsTask)
		}
	}
	for p := range want {
		if !seen[p] {
			t.Fatalf("缺少异步端点 %q", p)
		}
	}
}

// TestMusicModalityProbeUsesRealPath 确认 music 模态的探针用真实路径。
//
// 用错路径的后果：music 模型的绑定校验永远判「不支持」，运营无法保存绑定。
func TestMusicModalityProbeUsesRealPath(t *testing.T) {
	probes := ModalityProbes("music")
	if len(probes) != 1 {
		t.Fatalf("music 应有且仅有一个探针，实际 %d", len(probes))
	}
	if probes[0].Path != "audio/music/generations" || !probes[0].Known || !probes[0].IsTask {
		t.Fatalf("music 探针应为已知异步端点 audio/music/generations，实际 %+v", probes[0])
	}
}

// TestImageModalityProbeIncludesSyncEndpoint 确认 image 探针覆盖同步端点。
//
// 多米系图像只有异步上游，但平台已经提供同步桥接；探针必须包含
// images/generations，否则「只声明同步端点」的图像模型会被判成无渠道可承接。
func TestImageModalityProbeIncludesSyncEndpoint(t *testing.T) {
	var hasSync bool
	for _, pr := range ModalityProbes("image") {
		if pr.Path == "images/generations" && pr.Known {
			hasSync = true
		}
	}
	if !hasSync {
		t.Fatalf("image 探针应包含 images/generations，实际 %+v", ModalityProbes("image"))
	}
}

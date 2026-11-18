package taskshell

import (
	"encoding/json"
	"testing"
	"time"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/taskstate"
)

// sunoTask 造一条任务行（只填投影会读的字段）。
func sunoTask(status string) *model.Task {
	at := time.Unix(1794818458, 0)
	return &model.Task{
		PublicID:  "task_01M7H9Z3QK8R2V5X7C4B6N1P0D",
		Status:    status,
		CreatedAt: at,
		UpdatedAt: at,
	}
}

func TestSunoOfMusicClipsFromArtifacts(t *testing.T) {
	tk := sunoTask(taskstate.Succeeded)
	tk.UpdatedAt = time.Unix(1794818572, 0)

	got := SunoOf(SunoBuildInput{
		Task: tk, Action: SunoActionMusic,
		Title: "还在路上", Tags: "民谣,温暖", Prompt: "轻快的民谣", Version: "chirp-hawk",
		Artifacts: []model.Artifact{
			{Index: 0, Kind: "audio"},
			{Index: 1, Kind: "audio"},
			// 文本产物不属于音乐任务的交付项：混进来会让客户端把歌词当音频下载。
			{Index: 2, Kind: "text"},
		},
		ContentURL: func(idx int) string { return "/content/" + string(rune('0'+idx)) },
	})

	if got.Status != "SUCCESS" {
		t.Fatalf("状态词应为 SUCCESS（对外词表），实际 %q", got.Status)
	}
	if got.FailReason != "" {
		t.Fatalf("成功任务不该给失败原因，实际 %q", got.FailReason)
	}
	if got.FinishTime != 1794818572 {
		t.Fatalf("终态要给出完成时间，实际 %d", got.FinishTime)
	}
	clips, ok := got.Data.([]SunoClip)
	if !ok {
		t.Fatalf("music 成功时内层 data 应是 clip 数组，实际 %T", got.Data)
	}
	if len(clips) != 2 {
		t.Fatalf("只应投影音频产物（2 条），实际 %d 条", len(clips))
	}
	if clips[1].ID != tk.PublicID+":1" {
		t.Fatalf("clip id 应按产物序号拼接，实际 %q", clips[1].ID)
	}
	if clips[1].AudioURL != "/content/1" {
		t.Fatalf("audio_url 应来自 ContentURL，实际 %q", clips[1].AudioURL)
	}
	// 封面与视频版平台不转存：键必须在，值必须空。
	if clips[0].ImageURL != "" || clips[0].VideoURL != "" || clips[0].ImageLargeURL != "" {
		t.Fatalf("封面/视频版不能有值：%+v", clips[0])
	}
	if clips[0].Metadata.Duration != nil {
		t.Fatalf("duration 平台不采集，必须是 null")
	}
}

func TestSunoOfLyricsUsesResultText(t *testing.T) {
	tk := sunoTask(taskstate.Succeeded)
	tk.ResultText = "[Verse]\n打卡机响过十二点"

	got := SunoOf(SunoBuildInput{
		Task: tk, Action: SunoActionLyrics, Title: "深夜加班",
		// 请求里的歌词只是兜底：正文以结果为准。
		Lyrics: "[Verse]\n旧歌词",
	})
	ly, ok := got.Data.(SunoLyrics)
	if !ok {
		t.Fatalf("lyrics 成功时内层 data 应是单个对象，实际 %T", got.Data)
	}
	if ly.Text != tk.ResultText {
		t.Fatalf("歌词正文应取 result_text，实际 %q", ly.Text)
	}
	if ly.Title != "深夜加班" {
		t.Fatalf("标题应透传请求侧，实际 %q", ly.Title)
	}
}

// 未完成时内层 data 必须是 null：给空数组会被读成「生成完了但一首都没有」。
func TestSunoOfNonTerminalHasNullData(t *testing.T) {
	for _, st := range []string{taskstate.Queued, taskstate.Running} {
		got := SunoOf(SunoBuildInput{
			Task: sunoTask(st), Action: SunoActionMusic,
			Artifacts: []model.Artifact{{Index: 0, Kind: "audio"}},
			ContentURL: func(int) string {
				t.Fatalf("%s 的任务不该构造下载地址", st)
				return ""
			},
		})
		if got.Data != nil {
			t.Fatalf("%s 的内层 data 必须是 null，实际 %#v", st, got.Data)
		}
		if got.FinishTime != 0 {
			t.Fatalf("%s 不是终态，完成时间必须是 0，实际 %d", st, got.FinishTime)
		}
	}
}

// 失败的任务也要给 null，且带上分类文案（客户端据此决定重试还是退款）。
func TestSunoOfFailedCarriesReason(t *testing.T) {
	got := SunoOf(SunoBuildInput{Task: sunoTask(taskstate.Expired), Action: SunoActionMusic})
	if got.Status != "FAILURE" {
		t.Fatalf("超时要折进 FAILURE，实际 %q", got.Status)
	}
	if got.Data != nil {
		t.Fatalf("失败任务的内层 data 必须是 null，实际 %#v", got.Data)
	}
	if got.FailReason == "" {
		t.Fatalf("失败任务必须给原因")
	}
}

// 信封三个键必须始终出现：客户端读 metadata.duration 不会先判 metadata 是否存在。
func TestSunoClipJSONKeepsNullKeys(t *testing.T) {
	raw, err := json.Marshal(SunoClip{})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	for _, k := range []string{"audio_url", "image_url", "image_large_url", "video_url", "text", "metadata"} {
		if _, has := m[k]; !has {
			t.Fatalf("clip 缺少键 %q（new-api 的客户端直接读它，不做存在性判断）", k)
		}
	}
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata 必须是对象，实际 %T", m["metadata"])
	}
	if v, has := meta["duration"]; !has || v != nil {
		t.Fatalf("metadata.duration 必须是显式 null，实际 %v（存在=%v）", v, has)
	}
}

package task

import (
	"context"
	"reflect"
	"testing"
	"time"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// 本文件覆盖 B3 的产物清单与上游用量合并（纯函数，不依赖 DB / 网络）。
//
// 这两个函数是成功路径上「唯一会改钱」的地方：清单决定收几张图的钱，
// 用量合并决定收几秒视频的钱。错一位就是系统性偏差，所以全部按表驱动锁死。

// TestMergeArtifactManifest 覆盖「按 URL 复用、新增追加、文本固定最后、序号稠密」。
func TestMergeArtifactManifest(t *testing.T) {
	t.Run("首次生成", func(t *testing.T) {
		got := mergeArtifactManifest(nil, []string{"u1", "u2"}, "")
		want := []artifactEntry{{Index: 0, URL: "u1"}, {Index: 1, URL: "u2"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v want %+v", got, want)
		}
	})

	t.Run("重试复用已转存条目", func(t *testing.T) {
		// 上一轮 u1 已转存（f=11），u2 失败（f=0）。这一轮两个都在。
		existing := []artifactEntry{{Index: 0, URL: "u1", FileID: 11}, {Index: 1, URL: "u2"}}
		got := mergeArtifactManifest(existing, []string{"u1", "u2"}, "")
		want := []artifactEntry{{Index: 0, URL: "u1", FileID: 11}, {Index: 1, URL: "u2"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("已转存条目必须复用 file_id（否则会重复下载）：got %+v want %+v", got, want)
		}
	})

	t.Run("上游顺序变化不影响已转存映射", func(t *testing.T) {
		// 关键：不能按下标匹配。上游把 u2 排到前面时，u2 仍应带上它的 f=22。
		existing := []artifactEntry{{Index: 0, URL: "u1", FileID: 11}, {Index: 1, URL: "u2", FileID: 22}}
		got := mergeArtifactManifest(existing, []string{"u2", "u1"}, "")
		if got[0].FileID != 22 || got[1].FileID != 11 {
			t.Fatalf("必须按 URL 复用而非按下标：got %+v", got)
		}
		if got[0].Index != 0 || got[1].Index != 1 {
			t.Fatalf("序号应重新稠密编号：got %+v", got)
		}
	})

	t.Run("新增产物追加到末尾", func(t *testing.T) {
		existing := []artifactEntry{{Index: 0, URL: "u1", FileID: 11}}
		got := mergeArtifactManifest(existing, []string{"u1", "u3"}, "")
		want := []artifactEntry{{Index: 0, URL: "u1", FileID: 11}, {Index: 1, URL: "u3"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v want %+v", got, want)
		}
	})

	t.Run("文本产物固定在最后", func(t *testing.T) {
		existing := []artifactEntry{
			{Index: 0, URL: "u1", FileID: 11},
			{Index: 1, Text: true, FileID: 12},
		}
		got := mergeArtifactManifest(existing, []string{"u1", "u2"}, "歌词内容")
		if len(got) != 3 {
			t.Fatalf("应有 3 条，实际 %+v", got)
		}
		last := got[2]
		if !last.Text || last.Index != 2 || last.FileID != 12 {
			t.Fatalf("文本产物应保留在末尾并复用 file_id，实际 %+v", last)
		}
		if got[0].URL != "u1" || got[1].URL != "u2" {
			t.Fatalf("链接产物顺序不符：%+v", got)
		}
	})

	t.Run("上游不再返回文本时不保留文本条目", func(t *testing.T) {
		existing := []artifactEntry{{Index: 0, URL: "u1", FileID: 11}, {Index: 1, Text: true, FileID: 12}}
		got := mergeArtifactManifest(existing, []string{"u1"}, "")
		if len(got) != 1 || got[0].URL != "u1" {
			t.Fatalf("本轮没有文本产物就不该保留，实际 %+v", got)
		}
	})

	t.Run("空产物", func(t *testing.T) {
		if got := mergeArtifactManifest(nil, nil, ""); len(got) != 0 {
			t.Fatalf("应返回空清单，实际 %+v", got)
		}
	})
}

// TestManifestStats 覆盖「主产物 = 第一个成功的、count = 成功数」。
func TestManifestStats(t *testing.T) {
	cases := []struct {
		name   string
		list   []artifactEntry
		mainID uint64
		count  int
	}{
		{"全成功", []artifactEntry{{FileID: 11}, {FileID: 12}}, 11, 2},
		{"首个失败", []artifactEntry{{FileID: 0}, {FileID: 12}}, 12, 1},
		{"全失败", []artifactEntry{{FileID: 0}, {FileID: 0}}, 0, 0},
		{"空清单", nil, 0, 0},
		{"文本也计入", []artifactEntry{{URL: "u1", FileID: 11}, {Text: true, FileID: 12}}, 11, 2},
	}
	for _, c := range cases {
		mainID, count := manifestStats(c.list)
		if mainID != c.mainID || count != c.count {
			t.Fatalf("%s：got (%d,%d) want (%d,%d)", c.name, mainID, count, c.mainID, c.count)
		}
	}
}

// TestParseArtifactManifest 覆盖脏数据容错（坏 JSON 不能让成功任务永远结算不了）。
func TestParseArtifactManifest(t *testing.T) {
	if got := parseArtifactManifest(nil); len(got) != 0 {
		t.Fatalf("nil 应返回空，实际 %+v", got)
	}
	if got := parseArtifactManifest(model.JSON(`not-json`)); len(got) != 0 {
		t.Fatalf("坏 JSON 应返回空，实际 %+v", got)
	}
	got := parseArtifactManifest(model.JSON(`[{"i":0,"u":"u1","f":11},{"i":1,"t":true,"f":12}]`))
	if len(got) != 2 || got[0].URL != "u1" || got[0].FileID != 11 || !got[1].Text {
		t.Fatalf("解析结果不符：%+v", got)
	}
}

// TestMergeUpstreamUsage 覆盖视频秒数的两种口径（显式 / 智能时长）与三态开关。
func TestMergeUpstreamUsage(t *testing.T) {
	base := billing.Usage{VideoSeconds: 5, RefVideoSeconds: 3, ImageN: 2}
	p := params{Seconds: 5, RefVideoSeconds: 3}

	t.Run("上游更短时保留请求值", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		got, anom := mergeUpstreamUsage(base, relay.TaskUsage{OutputSeconds: 3}, p)
		if got.VideoSeconds != 5 {
			t.Fatalf("上游更短应按请求值收（不少收），实际 %d", got.VideoSeconds)
		}
		if !hasFlag(anom, "usage_upstream_lower") {
			t.Fatalf("应标记 usage_upstream_lower，实际 %v", anom)
		}
	})

	t.Run("上游更长时按上游收", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		got, anom := mergeUpstreamUsage(base, relay.TaskUsage{OutputSeconds: 9}, p)
		if got.VideoSeconds != 9 {
			t.Fatalf("上游更长应按上游收，实际 %d", got.VideoSeconds)
		}
		if !hasFlag(anom, "usage_upstream_higher") {
			t.Fatalf("应标记 usage_upstream_higher，实际 %v", anom)
		}
	})

	t.Run("小数秒四舍五入而非向上取整", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		// 5.2 秒按 5 计。Ceil 会算成 6（多收 15%），而用户能拿视频时长举证；
		// 计费粒度是秒，四舍五入才与观感一致。Ark 的 duration 是整数，本改动
		// 只影响 frames/24 换算出的小数（如 121 帧 = 5.04 秒）。
		got, _ := mergeUpstreamUsage(base, relay.TaskUsage{OutputSeconds: 5.2}, p)
		if got.VideoSeconds != 5 {
			t.Fatalf("5.2 秒应四舍五入为 5，实际 %d", got.VideoSeconds)
		}
	})

	t.Run("帧数换算 121/24 不得算成 6 秒", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		got, _ := mergeUpstreamUsage(base, relay.TaskUsage{OutputSeconds: 121.0 / 24}, p)
		if got.VideoSeconds != 5 {
			t.Fatalf("121 帧（5.04 秒）应按 5 计，实际 %d", got.VideoSeconds)
		}
	})

	t.Run("帧数换算 133/24 按 6 秒", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		got, _ := mergeUpstreamUsage(base, relay.TaskUsage{OutputSeconds: 133.0 / 24}, p)
		if got.VideoSeconds != 6 {
			t.Fatalf("133 帧（5.54 秒）应按 6 计，实际 %d", got.VideoSeconds)
		}
	})

	t.Run("智能时长以上游为准（不再按预算收）", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		// 预算 30 秒是冻结上限，不是应收。用户选「智能时长」本意是让模型决定，
		// 按预算收等于「选了就被收最贵」。
		auto := params{Seconds: 30, SecondsAuto: true}
		got, anom := mergeUpstreamUsage(billing.Usage{VideoSeconds: 30}, relay.TaskUsage{OutputSeconds: 5}, auto)
		if got.VideoSeconds != 5 {
			t.Fatalf("智能时长应按上游 5 秒收，实际 %d", got.VideoSeconds)
		}
		if !hasFlag(anom, "usage_upstream_lower") {
			t.Fatalf("应标记 usage_upstream_lower，实际 %v", anom)
		}
	})

	t.Run("智能时长上游更长时按上游收", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		auto := params{Seconds: 30, SecondsAuto: true}
		got, _ := mergeUpstreamUsage(billing.Usage{VideoSeconds: 30}, relay.TaskUsage{OutputSeconds: 40}, auto)
		if got.VideoSeconds != 40 {
			t.Fatalf("应按上游 40 秒收，实际 %d", got.VideoSeconds)
		}
	})

	t.Run("智能时长但上游没报时长必须留痕", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		auto := params{Seconds: 30, SecondsAuto: true}
		got, anom := mergeUpstreamUsage(billing.Usage{VideoSeconds: 30}, relay.TaskUsage{}, auto)
		if got.VideoSeconds != 30 {
			t.Fatalf("无上游时长时只能回落预算 30，实际 %d", got.VideoSeconds)
		}
		if !hasFlag(anom, "usage_upstream_missing") {
			t.Fatalf("应标记 usage_upstream_missing（否则永远按上限收没人发现），实际 %v", anom)
		}
	})

	t.Run("上游谎报超大秒数被 clamp", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		got, _ := mergeUpstreamUsage(base, relay.TaskUsage{OutputSeconds: 86400}, p)
		if got.VideoSeconds != billing.MaxVideoSeconds {
			t.Fatalf("应 clamp 到 %d，实际 %d", billing.MaxVideoSeconds, got.VideoSeconds)
		}
	})

	t.Run("参考视频取 max", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		got, anom := mergeUpstreamUsage(base, relay.TaskUsage{InputSeconds: 8}, p)
		if got.RefVideoSeconds != 8 {
			t.Fatalf("参考视频应取 max(上游,请求)=8，实际 %d", got.RefVideoSeconds)
		}
		if !hasFlag(anom, "usage_upstream_input_higher") {
			t.Fatalf("应标记 usage_upstream_input_higher，实际 %v", anom)
		}
	})

	t.Run("图片张数只做对照不改结算", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		got, anom := mergeUpstreamUsage(base, relay.TaskUsage{ImageCount: 4}, p)
		if got.ImageN != 2 {
			t.Fatalf("图片张数不应被上游值改写（结算以成功转存数为准），实际 %d", got.ImageN)
		}
		if !hasFlag(anom, "upstream_image_count_mismatch") {
			t.Fatalf("应标记 upstream_image_count_mismatch，实际 %v", anom)
		}
	})

	t.Run("shadow 模式不改结算", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "shadow")
		got, anom := mergeUpstreamUsage(base, relay.TaskUsage{OutputSeconds: 9}, p)
		if got.VideoSeconds != 5 {
			t.Fatalf("shadow 模式不得改结算用量，实际 %d", got.VideoSeconds)
		}
		if !hasFlag(anom, "usage_upstream_higher") {
			t.Fatalf("shadow 模式仍应记录 anomaly，实际 %v", anom)
		}
	})

	t.Run("关闭模式完全忽略上游", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "0")
		got, anom := mergeUpstreamUsage(base, relay.TaskUsage{OutputSeconds: 9}, p)
		if got.VideoSeconds != 5 {
			t.Fatalf("关闭模式应完全忽略上游，实际 %d", got.VideoSeconds)
		}
		if len(anom) != 0 {
			t.Fatalf("关闭模式不应写 anomaly，实际 %v", anom)
		}
	})

	t.Run("上游零值不触发任何改写", func(t *testing.T) {
		t.Setenv("LD_TASK_USAGE_SETTLE", "1")
		got, anom := mergeUpstreamUsage(base, relay.TaskUsage{}, p)
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("零值用量不应改写：got %+v", got)
		}
		if len(anom) != 0 {
			t.Fatalf("零值不应产生 anomaly，实际 %v", anom)
		}
	})
}

// TestApplyArtifactActuals 覆盖「按成功转存数修正图像张数」。
func TestApplyArtifactActuals(t *testing.T) {
	p := params{ImageN: 4}
	cases := []struct {
		name    string
		usage   billing.Usage
		okCount int
		want    int64
	}{
		{"部分成功按实际", billing.Usage{ImageN: 4}, 2, 2},
		{"全部成功保持原值", billing.Usage{ImageN: 4}, 4, 4},
		{"成功数更多时不放大", billing.Usage{ImageN: 2}, 5, 2},
		{"非图像任务不受影响", billing.Usage{ImageN: 0}, 1, 0},
		{"okCount=0 保持原值（全失败另有闸门）", billing.Usage{ImageN: 4}, 0, 4},
	}
	for _, c := range cases {
		got := applyArtifactActuals(c.usage, c.okCount, p, billing.Snapshot{})
		if got.ImageN != c.want {
			t.Fatalf("%s：ImageN got %d want %d", c.name, got.ImageN, c.want)
		}
	}
}

// TestResultURLAtFromManifest 覆盖清单取链（零出站）与越界/文本条目语义。
func TestResultURLAtFromManifest(t *testing.T) {
	task := &model.Task{ArtifactManifest: model.JSON(
		`[{"i":0,"u":"https://a.example/1.png","f":11},{"i":1,"t":true,"f":12}]`)}

	if u, ok := ResultURLAtFromManifest(task, 0); !ok || u != "https://a.example/1.png" {
		t.Fatalf("第 0 个应取到链接，实际 (%q,%v)", u, ok)
	}
	// 文本条目没有链接：确定没有（ok=true），不要再去问上游。
	if u, ok := ResultURLAtFromManifest(task, 1); !ok || u != "" {
		t.Fatalf("文本条目应返回空且确定，实际 (%q,%v)", u, ok)
	}
	// 越界：同样确定没有。
	if u, ok := ResultURLAtFromManifest(task, 9); !ok || u != "" {
		t.Fatalf("越界应返回空且确定，实际 (%q,%v)", u, ok)
	}
	// 无清单（B3 之前的老任务）：不确定，调用方需回退实时查询。
	if _, ok := ResultURLAtFromManifest(&model.Task{}, 0); ok {
		t.Fatal("无清单应返回 ok=false 以便回退")
	}
	// 非 http(s) 链接一律不给（防 file:// 之类）。
	bad := &model.Task{ArtifactManifest: model.JSON(`[{"i":0,"u":"file:///etc/passwd"}]`)}
	if u, ok := ResultURLAtFromManifest(bad, 0); !ok || u != "" {
		t.Fatalf("非 http(s) 链接应被拒，实际 (%q,%v)", u, ok)
	}
}

// TestTaskUsageSettleMode 覆盖开关解析（默认 1）。
func TestTaskUsageSettleMode(t *testing.T) {
	cases := map[string]string{
		"":         "1",
		"1":        "1",
		"true":     "1",
		"shadow":   "shadow",
		" SHADOW ": "shadow",
		"0":        "0",
		"off":      "0",
		"false":    "0",
		"no":       "0",
		"disable":  "1", // 拼错 → 保持默认（参与结算）
	}
	for env, want := range cases {
		t.Setenv("LD_TASK_USAGE_SETTLE", env)
		if got := taskUsageSettleMode(); got != want {
			t.Fatalf("LD_TASK_USAGE_SETTLE=%q → %q, want %q", env, got, want)
		}
	}
}

// hasFlag 判断 anomaly 标记集合里是否含指定项。
func hasFlag(list []string, want string) bool {
	for _, f := range list {
		if f == want {
			return true
		}
	}
	return false
}

// TestRetryResetFields 锁死「重试必须重置哪些列」。
//
// 这不是形式化断言：少清 artifact_manifest 会让上一轮产物冒充本轮结果，
// 少清 channel_id/upstream_task_id 会让轮询去查上一轮的上游任务 ——
// 两种都不会报错，只会静默算错账/查错单。
func TestRetryResetFields(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	m := retryResetFields("image", deadline)

	expectNil := []string{"artifact_manifest", "result_file_id", "upstream_usage"}
	for _, k := range expectNil {
		v, ok := m[k]
		if !ok {
			t.Fatalf("必须显式清空 %s（缺失等于保留旧值）", k)
		}
		if v != nil {
			t.Fatalf("%s 应为 nil，实际 %v", k, v)
		}
	}
	if v, ok := m["artifact_count"]; !ok || v.(int) != 0 {
		t.Fatalf("artifact_count 应重置为 0，实际 %v（ok=%v）", v, ok)
	}
	for _, k := range []string{"channel_id", "platform", "upstream_task_id", "subtask_kind", "error_msg"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("必须重置 %s", k)
		}
	}
	if m["kind"] != "image" {
		t.Fatalf("kind 必须写回原模态（否则图像任务重试后变成 video），实际 %v", m["kind"])
	}
	if m["deadline_at"] != deadline {
		t.Fatalf("deadline_at 不符：%v", m["deadline_at"])
	}
	// 防御性：绝不能顺手把 usage_id 清掉（清了就永久丢账务会话）。
	if _, ok := m["usage_id"]; ok {
		t.Fatal("usage_id 不得在重试重置里被清空")
	}
}

// TestArtifactReadableOrder 覆盖「第 idx 个可读产物」的索引语义（需 DB）。
//
// 关键场景：内部序号有空洞（0 失败、2 成功）。按序号精确匹配会让 /contents/0 返回 410，
// 而 artifact_count=1，客户端逐个取时第一个就失败。现在序号直接索引
// readableArtifacts 的切片，清单与取数共用同一份，结构上不可能错位。
func TestArtifactReadableOrder(t *testing.T) {
	gdb := withTestDB(t)
	if err := gdb.AutoMigrate(&model.File{}); err != nil {
		t.Fatalf("迁移 files 表失败: %v", err)
	}
	ctx := context.Background()

	rows := []model.File{
		{TaskID: 42, ArtifactIndex: 0, UserID: 1, Filename: "a.png", SizeBytes: 10, StorageBackend: "none", StorageKey: "k0", ContentType: "image/png"},
		{TaskID: 42, ArtifactIndex: 2, UserID: 1, Filename: "c.png", SizeBytes: 30, StorageBackend: "none", StorageKey: "k2", ContentType: "image/png"},
		{TaskID: 42, ArtifactIndex: 5, UserID: 1, Filename: "f.png", SizeBytes: 50, StorageBackend: "none", StorageKey: "k5", ContentType: "image/png"},
	}
	for i := range rows {
		if err := gdb.Create(&rows[i]).Error; err != nil {
			t.Fatalf("造 files 行失败: %v", err)
		}
	}
	// 模拟「内部序号 0 其实没有落库」（失败条目）——删掉第 0 行。
	if err := gdb.Unscoped().Where("task_id = ? AND artifact_index = ?", 42, 0).Delete(&model.File{}).Error; err != nil {
		t.Fatalf("删 files 行失败: %v", err)
	}

	readable, err := readableArtifacts(ctx, 42)
	if err != nil {
		t.Fatalf("读可读产物失败: %v", err)
	}
	if len(readable) != 2 || readable[0].ArtifactIndex != 2 || readable[1].ArtifactIndex != 5 {
		t.Fatalf("可读产物应是内部序号 2,5（按序），实际 %+v", readable)
	}

	// ArtifactsOf 下发的 index 必须是稠密 0..n-1（与 /contents/{i} 一致）。
	list, err := ArtifactsOf(ctx, 42)
	if err != nil {
		t.Fatalf("ArtifactsOf 失败: %v", err)
	}
	if len(list) != 2 || list[0].Index != 0 || list[1].Index != 1 {
		t.Fatalf("对外序号应稠密重编号为 0,1，实际 %+v", list)
	}
	if list[0].Bytes != 30 || list[1].Bytes != 50 {
		t.Fatalf("字节数应与行一致，实际 %+v", list)
	}
}

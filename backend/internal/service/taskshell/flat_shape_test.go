package taskshell_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/taskshell"
)

// TestTaskFlatMatchesDocFields 是「结构体 ↔ 文档字段表」的唯一守卫。
//
// 为什么放在本包而不是 apidoc：apidoc 不能 import service（`relay → logs → task →
// relay` 的环已经存在），反向依赖会把文档层锁死在服务层之上。
//
// 比的是**顶层** json tag；嵌套的 artifacts[] 由下面 TestArtifactShapeMatchesDocFields
// 用同一套判据守。少一个键的表现是文档里写了一列永远不存在的字段，多一个键的
// 表现是客户端拿到的字段没人解释 —— 两种都不报错。
func TestTaskFlatMatchesDocFields(t *testing.T) {
	tags := jsonTagsOf(reflect.TypeOf(taskshell.TaskFlat{}))
	want := make([]string, 0, len(apidoc.TaskFlatFields()))
	for _, f := range apidoc.TaskFlatFields() {
		if strings.ContainsAny(f.Name, ".[") {
			continue
		}
		want = append(want, f.Name)
	}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("结构体与文档字段表不一致\n struct=%v\n   docs=%v", tags, want)
	}
}

// TestTaskFlatAliasesAreConsistent 守两个「同值别名键」。
//
// new-api 自己的两个壳用了不同的键名（id/task_id、finished_at/finish_time），
// 我们同时下发避免任一类客户端取空。别名一旦不同值，客户端对同一个任务
// 会算出两种结论 —— 这比缺字段更难查。
func TestTaskFlatAliasesAreConsistent(t *testing.T) {
	done := &model.Task{
		PublicID: "task_01J9HA7JPQ9A0Z6JZ3V8M9W6PZ", Model: "m",
		Status: "succeeded",
	}
	f := taskshell.FlatOf(done)
	if f.ID == "" || f.ID != f.TaskID {
		t.Fatalf("id 与 task_id 必须同值且非空: %+v", f)
	}
	if f.FinishedAt != f.FinishTime {
		t.Fatalf("finished_at 与 finish_time 必须同值: %+v", f)
	}
	if f.Platform != "lingdong" {
		t.Fatalf("platform 必须是中性标识，实际 %q", f.Platform)
	}
	// 未进入终态时两个时间键都是 0：给「现在」会让每次查询的完成时间都在变。
	running := taskshell.FlatOf(&model.Task{PublicID: "task_x", Status: "running"})
	if running.FinishedAt != 0 || running.FinishTime != 0 {
		t.Fatalf("非终态不应有完成时间: %+v", running)
	}
	if running.Status != "IN_PROGRESS" || running.Progress != "50%" {
		t.Fatalf("running 的状态词/进度不符: %+v", running)
	}
}

// TestTaskFlatFailureNeverLooksSuccessful 守业务底线：失败与超时都不能渲染成 SUCCESS。
func TestTaskFlatFailureNeverLooksSuccessful(t *testing.T) {
	for _, st := range []string{"failed", "expired", "canceled"} {
		f := taskshell.FlatOf(&model.Task{PublicID: "task_x", Status: st})
		if f.Status != "FAILURE" {
			t.Fatalf("%s 应编码为 FAILURE，实际 %q", st, f.Status)
		}
		if f.FailReason == "" {
			t.Fatalf("%s 必须有分类文案", st)
		}
	}
}

// jsonTagsOf 按**声明顺序**取 json 标签（omitempty 去掉）。
func jsonTagsOf(rt reflect.Type) []string {
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		out = append(out, name)
	}
	return out
}

// 保证别名键在 JSON 里真的存在（结构体同值但被 omitempty 吃掉也算违约）。
func TestTaskFlatJSONHasBothTimeKeys(t *testing.T) {
	raw, err := json.Marshal(taskshell.TaskFlat{ID: "a", TaskID: "a", FinishedAt: 1, FinishTime: 1})
	if err != nil {
		t.Fatalf("%v", err)
	}
	for _, k := range []string{`"finished_at":1`, `"finish_time":1`} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("JSON 缺少 %s：%s", k, raw)
		}
	}
}

// TestArtifactShapeMatchesDocFields 是「产物清单结构体 ↔ 文档字段表」的守卫。
//
// 这条曾经不存在（原注释把它推给"artifacts 的用例"，而那个用例并不存在），
// 于是把一个真实的谎言留在文档里：`mime_type` 带 omitempty，上游没给 MIME 时
// 整个键消失，而字段表写的是「MIME 类型」——没有"可能缺"这一说。
// 现在结构体去掉 omitempty、字段表注明可能为空串，键集合由本用例逐字对齐。
func TestArtifactShapeMatchesDocFields(t *testing.T) {
	got := make([]string, 0, 8)
	for _, n := range jsonTagsOf(reflect.TypeOf(taskshell.ArtifactList{})) {
		got = append(got, n)
		if n == "artifacts" {
			for _, a := range jsonTagsOf(reflect.TypeOf(taskshell.Artifact{})) {
				got = append(got, "artifacts[]."+a)
			}
		}
	}
	want := make([]string, 0, len(apidoc.TaskArtifactFields()))
	for _, f := range apidoc.TaskArtifactFields() {
		want = append(want, f.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("产物清单与文档字段表不一致（顺序也算）\n struct=%v\n   docs=%v", got, want)
	}
}

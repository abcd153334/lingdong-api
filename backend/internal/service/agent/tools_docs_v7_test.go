package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/docdocs"
)

// 文档工具面的门禁：**工具清单、参数形状、作用域**三者不能各说各话。
//
// 这一层的错误都不会报错，只会让模型"照着做却总是失败"：
//   - 声明了工具但没有实现 → 模型调它拿到 unknown_tool，然后反复换措辞重试；
//   - 参数 schema 漏了一列 → 模型写进去被拒，以为是自己格式不对；
//   - 工具清单里混进 publish → AI 就能把草稿推到线上，而这是明确不允许的。

func TestDocToolRegistryConsistent(t *testing.T) {
	specs := map[string]bool{}
	for _, s := range ToolSpecs() {
		specs[s.Function.Name] = true
	}
	for _, name := range docToolsV7 {
		if !specs[name] {
			t.Errorf("%s 有实现但没声明（模型永远调不到）", name)
		}
		if _, ok := toolImpls[name]; !ok {
			t.Errorf("%s 声明了但没有实现（模型会拿到 unknown_tool 并反复重试）", name)
		}
		if strings.Contains(name, "publish") || strings.Contains(name, "delete") {
			t.Errorf("%s 不该存在：发布与删除是人的动作", name)
		}
	}
	// 反向：声明里带 doc_ 前缀的必须都在清单里（漏登记 = 少一条防呆）。
	for name := range specs {
		if !strings.HasPrefix(name, "doc_") {
			continue
		}
		found := false
		for _, n := range docToolsV7 {
			if n == name {
				found = true
			}
		}
		// docs_read 是"读上传的上游文档"，不属于文档站工具面。
		if !found && name != "docs_read" {
			t.Errorf("%s 已声明但没登记进 docToolsV7（清单与声明会分叉）", name)
		}
	}
	// v6 的三个工具必须已经消失：留着它们，模型会先去改已经不存在的文档组。
	for _, dead := range []string{"doc_list", "doc_get", "doc_write"} {
		if specs[dead] {
			t.Errorf("%s 是 v6 工具，已随文档站重构删除", dead)
		}
	}
}

// readOnlyDocFields 是「投影层派生、不可写」的字段：它们出现在读回的结构体里，
// 但不该出现在 AI 的入参 schema 上（给了就是假选项，模型会以为能改）。
var readOnlyDocFields = map[string]bool{
	"only_models": true,
}

// TestDocToolSchemasMatchStructs 用反射比对参数属性名与结构体 json tag。
//
// 手写 schema 的好处是能决定"哪些必填"，代价是可能漏一列 —— 这个用例就是那条保险：
// 结构体加了列而 schema 没加，模型永远不会传它，而那一列在页面上永远是空。
func TestDocToolSchemasMatchStructs(t *testing.T) {
	cases := []struct {
		name string
		got  map[string]any
		typ  reflect.Type
	}{
		{"field", docFieldSchema(), reflect.TypeOf(docdocs.FieldView{})},
		{"response", docResponseSchema(), reflect.TypeOf(docdocs.AdminResponse{})},
		{"variant", docResponseSchema()["properties"].(map[string]any)["variants"].(map[string]any)["items"].(map[string]any), reflect.TypeOf(docdocs.AdminVariant{})},
		{"samples", docSamplesSchema("x"), reflect.TypeOf(docdocs.SamplesView{})},
		{"preset", docPresetsSchema()["items"].(map[string]any), reflect.TypeOf(docdocs.AdminPreset{})},
		{"error", docErrorsSchema()["items"].(map[string]any), reflect.TypeOf(docdocs.AdminError{})},
	}
	for _, c := range cases {
		props, _ := c.got["properties"].(map[string]any)
		got := map[string]bool{}
		for k := range props {
			got[k] = true
		}
		want := map[string]bool{}
		for i := 0; i < c.typ.NumField(); i++ {
			tag := c.typ.Field(i).Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name == "" || name == "-" || readOnlyDocFields[name] {
				continue
			}
			want[name] = true
		}
		for k := range want {
			if !got[k] {
				t.Errorf("%s schema 缺参数 %q（结构体有这一列，模型却传不进来）", c.name, k)
			}
		}
		for k := range got {
			if !want[k] {
				t.Errorf("%s schema 多了参数 %q（结构体里没有，传了会被 strictUnmarshal 拒）", c.name, k)
			}
		}
	}
}

// TestDocToolsRespectScope 守「从别的入口进来不能顺手改文档」。
//
// 这是入口语义而不是权限：管理员本来能改文档，但"帮我建个适配器"的会话
// 不该把对外文档一起改掉。
func TestDocToolsRespectScope(t *testing.T) {
	env := &toolEnv{AdminID: 1, Scope: []string{EntityAdapterSpec}}
	res, err := toolDocNodeCreate(env, json.RawMessage(`{"key":"x","kind":"family","title":"t","section":"images"}`))
	if err != nil {
		t.Fatalf("工具不该返回 Go error: %v", err)
	}
	m, _ := res.(map[string]any)
	if m["ok"] != false || m["code"] != "out_of_scope" {
		t.Fatalf("scope 不含 doc 时必须拒绝写文档，实际 %#v", m)
	}
	// 读工具**不查 scope**：只读不改变任何东西，拦它只会让模型连现状都看不到。
	// （这条不在这里调 toolDocTree：纯单测没有 db.DB，读工具会直接查库。）
	// 空 scope = 不限制（管理台 /admin/agent 入口）。
	if !(&toolEnv{}).scopeAllows(EntityDoc) {
		t.Fatal("空 scope 必须等于不限制")
	}
}

// TestDocToolsWriteDraftOnly 走一遍真实写链路：建节点 → 建接口（带 async 但缺 202）
// → doc_check 报阻断 → 补 202 → 归零，且**状态始终是草稿**。
func TestDocToolsWriteDraftOnly(t *testing.T) {
	withAgentDB(t)
	env := &toolEnv{AdminID: 1}

	res, err := toolDocNodeCreate(env, json.RawMessage(
		`{"key":"kling-videos","kind":"general","title":"KLING 视频","section":"videos"}`))
	if err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	if m := res.(map[string]any); m["ok"] != true {
		t.Fatalf("建节点应成功，实际 %#v", m)
	}

	tr, ok := apidoc.TransportOfDocID("images/generations")
	if !ok {
		t.Fatal("契约里必须有 images/generations")
	}
	// 故意缺 202 变体：这是图片/视频文档最常见的漏项。
	body := `{"node":"kling-videos","key":"gen","name":"生成图片","transport_doc_id":"images/generations","method":"` +
		tr.Method + `","path":"` + tr.Path + `","fields":[{"name":"prompt","type":"string","in":"body","required":"required"},` +
		`{"name":"async","type":"boolean","in":"body","required":"optional"}]}`
	res, err = toolDocEndpointCreate(env, json.RawMessage(body))
	if err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	m := res.(map[string]any)
	if m["ok"] != true {
		t.Fatalf("建接口应成功，实际 %#v", m)
	}
	if m["status"] != model.DocDraft {
		t.Fatalf("AI 建的接口必须是草稿，实际 %v", m["status"])
	}
	if m["blocking"].(int) == 0 {
		t.Fatalf("缺 202 变体时必须报阻断，实际 %#v", m["problems"])
	}

	// 补 202 变体 + 成功变体。
	fix := `{"node":"kling-videos","key":"gen","response":{"kind":"json","variants":[` +
		`{"status":200,"label":"成功","returns":[{"name":"url","type":"string","in":"body"}]},` +
		`{"status":202,"label":"已受理","returns":[{"name":"task_id","type":"string","in":"body"}]}]}}`
	res, err = toolDocEndpointUpdate(env, json.RawMessage(fix))
	if err != nil {
		t.Fatalf("改接口失败: %v", err)
	}
	m = res.(map[string]any)
	if m["ok"] != true {
		t.Fatalf("改接口应成功，实际 %#v", m)
	}
	if m["blocking"].(int) != 0 {
		t.Fatalf("补齐 202 后不该再有阻断项，实际 %#v", m["problems"])
	}
	if m["status"] != model.DocDraft {
		t.Fatalf("改完仍是草稿：AI 没有任何发布路径，实际 %v", m["status"])
	}

	// 版本冲突必须说清"重新读再改"，而不是一句失败。
	res, err = toolDocEndpointUpdate(env, json.RawMessage(`{"node":"kling-videos","key":"gen","version":1,"name":"改个名"}`))
	if err != nil {
		t.Fatalf("改接口失败: %v", err)
	}
	if m := res.(map[string]any); m["ok"] != false || m["code"] != "version_conflict" {
		t.Fatalf("旧 version 必须返回 version_conflict，实际 %#v", m)
	}
}

// TestDocFieldsWriteKeepsOtherContent 守"只改字段不动别的"。
func TestDocFieldsWriteKeepsOtherContent(t *testing.T) {
	withAgentDB(t)
	env := &toolEnv{AdminID: 1}
	if _, err := toolDocNodeCreate(env, json.RawMessage(
		`{"key":"gen-node","kind":"general","title":"通用","section":"general"}`)); err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	create := `{"node":"gen-node","key":"ep","name":"接口","fields":[{"name":"a","type":"string","in":"body"}],` +
		`"samples":{"curl":"curl {{base_url}}/v1/x -d '{\"a\":\"1\"}'"}}`
	if res, _ := toolDocEndpointCreate(env, json.RawMessage(create)); res.(map[string]any)["ok"] != true {
		t.Fatalf("建接口失败: %#v", res)
	}

	cur, err := docdocs.LoadAdminEndpoint("gen-node", "ep")
	if err != nil {
		t.Fatalf("读回接口失败: %v", err)
	}
	res, err := toolDocFieldsWrite(env, json.RawMessage(fmt.Sprintf(
		`{"node":"gen-node","key":"ep","version":%d,"fields":[{"name":"b","type":"integer","in":"query"}]}`, cur.Version)))
	if err != nil {
		t.Fatalf("写字段失败: %v", err)
	}
	if m := res.(map[string]any); m["ok"] != true {
		t.Fatalf("写字段应成功，实际 %#v", m)
	}
	ep, err := docdocs.LoadAdminEndpoint("gen-node", "ep")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(ep.Fields) != 1 || ep.Fields[0].Name != "b" {
		t.Fatalf("字段应被整体替换，实际 %+v", ep.Fields)
	}
	if ep.Samples == nil || !strings.Contains(ep.Samples.Curl, "{{base_url}}") {
		t.Fatalf("只改字段不应动示例代码，实际 %+v", ep.Samples)
	}
}

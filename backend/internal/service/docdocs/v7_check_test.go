package docdocs

import (
	"testing"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
)

// 发布闸门的判据测试（纯内存，不依赖数据库）。
//
// 这些用例守的是「文档不能承诺平台跑不通的东西」——闸门松一条，
// 用户就会照着文档拿到 400/404，而站点上没有任何提示。

func epWith(docID string) *EndpointBundle {
	var id *string
	if docID != "" {
		v := docID
		id = &v
	}
	tr, _ := apidoc.TransportOfDocID(docID)
	return &EndpointBundle{
		Node: model.DocNode{Kind: model.DocNodeFamily, Status: model.DocPublished},
		EP: model.DocEndpoint{
			Key: "e", Name: "e", TransportDocID: id,
			Method: tr.Method, Path: tr.Path, Status: model.DocPublished,
		},
	}
}

func hasCode(ps []Problem, code string) bool {
	for _, p := range ps {
		if p.Code == code {
			return true
		}
	}
	return false
}

func TestUnboundCannotPublish(t *testing.T) {
	ps := CheckEndpoint(epWith(""))
	if !hasCode(ps, "E_UNBOUND") {
		t.Fatalf("未绑定端点的文档必须被拦下（只能存草稿），实际 %+v", ps)
	}
	for _, p := range ps {
		if p.Code == "E_UNBOUND" && !p.Blocking {
			t.Fatal("E_UNBOUND 必须是阻断级（告警拦不住发布）")
		}
	}
}

func TestMethodDriftBlocked(t *testing.T) {
	b := epWith("images/generations")
	b.EP.Method = "GET"
	ps := CheckEndpoint(b)
	if !hasCode(ps, "E_METHOD_DRIFT") {
		t.Fatalf("方法漂移必须阻断，实际 %+v", ps)
	}
}

// 图片「没有异步」的根因之一：端点支持 async，文档却没有 async 字段 /
// 「受理后返回任务对象」的响应变体。两条都必须由闸门兜住。
func TestAsyncNeedsFieldAndTaskVariant(t *testing.T) {
	b := epWith("images/generations")
	b.Variants = []model.DocResponseVariant{{StatusCode: 200, Label: "成功"}}
	ps := CheckEndpoint(b)
	if !hasCode(ps, "E_ASYNC_FIELD_MISSING") || !hasCode(ps, "E_ASYNC_VARIANT_MISSING") {
		t.Fatalf("异步端点缺字段/缺任务对象变体都应被拦下，实际 %+v", ps)
	}

	// 补齐后不再报这两条（其余规则另算）。
	b.Fields = []model.DocEndpointField{{Name: "async", In: "body", Required: "optional"}}
	b.Variants = append(b.Variants, model.DocResponseVariant{
		StatusCode: 200, Label: "已受理",
		Example: model.MustJSON([]map[string]any{{"k": "task_id", "v": "task_x"}}),
	})
	ps = CheckEndpoint(b)
	if hasCode(ps, "E_ASYNC_FIELD_MISSING") || hasCode(ps, "E_ASYNC_VARIANT_MISSING") {
		t.Fatalf("补齐 async 字段与任务对象变体后不该再报，实际 %+v", ps)
	}
}

// Seedream 的**唯一一篇**必须有 async 与任务对象变体：用户按这篇调异步会拿到
// 一个任务号，页面上不写就等于让人对着一份无解释的响应体猜。
//
// 这篇曾经叫 `images/generations@jimeng`（与主篇同文的第二页），已合并成一篇。
// TestSeedSeedreamHasNoAsync 是**反向**门禁：Seedream 的协议没有异步形态。
//
// 这篇曾经按端点契约（`/v1/images/generations` 在多米有异步）写了 async 字段与
// 「已受理」变体，而方舟只有同步 —— 按文档发 async:true 会得到一个"没有渠道能承接"
// 的 502。族级声明（FamilyDoc.AsyncSubmit）改了之后，这条断言跟着反过来：
// 页面只要有 async 或任务对象，就是文档在替上游承诺它没有的能力。
func TestSeedSeedreamHasNoAsync(t *testing.T) {
	var found *apidoc.SeedEndpoint
	for _, f := range apidoc.FamilyDocs() {
		if f.Key != apidoc.FamilyImagesSeedream {
			continue
		}
		for _, s := range apidoc.SeedFamilyEndpoints(f) {
			if s.DocID == "images/generations" {
				it := s
				found = &it
			}
		}
	}
	if found == nil {
		t.Fatal("种子里缺少 Seedream 的生成图片文档")
	}
	hasAsync := false
	for _, f := range found.Doc.Fields {
		if f.Name == "async" {
			hasAsync = true
		}
	}
	if hasAsync {
		t.Fatal("Seedream 页不该有 async 字段：方舟的 images/generations 只有同步形态")
	}
	hasTask := false
	for _, v := range found.Doc.Response.Variants {
		for _, kv := range v.Example {
			if kv.K == "task_id" {
				hasTask = true
			}
		}
	}
	if hasTask {
		t.Fatal("Seedream 页不该有「受理后返回任务对象」的变体：上游没有任务号")
	}
}

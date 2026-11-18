package docdocs

import (
	"encoding/json"
	"fmt"
	"strings"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 校验层：**能不能发布**的唯一判据（管理台、发布、AI、启动体检共用）。
//
// Problem.Field 必须是可定位路径（能滚到对应输入框、能让 AI 读懂改哪里）。
// 只说「不能发布」而不说「哪一行错」的系统，运营第一反应是绕过它。

// Problem 是一条体检结论。
type Problem struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
	// Blocking=false 是告警（不阻断发布，但管理台挂红点、启动期打日志）。
	Blocking bool `json:"blocking"`
}

// CheckEndpoint 校验一篇接口文档。
func CheckEndpoint(b *EndpointBundle) []Problem {
	out := make([]Problem, 0, 8)

	// 1) 绑定：未绑定 = 只能草稿。这是「不许伪造能力」的闸门。
	if b.EP.TransportDocID == nil || strings.TrimSpace(*b.EP.TransportDocID) == "" {
		out = append(out, Problem{
			Code: "E_UNBOUND", Field: "transport_doc_id", Blocking: true,
			Message: "未绑定平台已登记的端点，只能存草稿；绑定后才能发布。",
		})
	} else if tr, ok := apidoc.TransportOfDocID(*b.EP.TransportDocID); !ok {
		out = append(out, Problem{
			Code: "E_UNKNOWN_TRANSPORT", Field: "transport_doc_id", Blocking: true,
			Message: fmt.Sprintf("契约里没有端点 %q（拼写或方言名写错）。", *b.EP.TransportDocID),
		})
	} else {
		// 2) 漂移：方法与路径必须与契约一致（否则文档在教用户调另一个接口）。
		if b.EP.Method != tr.Method {
			out = append(out, Problem{
				Code: "E_METHOD_DRIFT", Field: "method", Blocking: true,
				Message: fmt.Sprintf("契约要求 %s，文档写的是 %s。", tr.Method, b.EP.Method),
			})
		}
		if b.EP.Path != tr.Path {
			out = append(out, Problem{
				Code: "E_PATH_DRIFT", Field: "path", Blocking: true,
				Message: fmt.Sprintf("契约要求 %s，文档写的是 %s。", tr.Path, b.EP.Path),
			})
		}
		// 3) 异步端点必须同时有 async 字段与「任务对象」变体。
		//    这两条正是「图片文档看起来没有异步」的根因，必须由发布闸门兜住。
		//
		//    判据从「有 202 变体」改成「有 task_id 示例」：提交响应已统一成 200
		//    （与查询同一个状态码），再用状态码区分同步/异步就失去意义；而
		//    「示例里出现 task_id」恰好就是"这篇写了受理后的任务对象"这件事本身。
		if tr.AcceptsAsync && familyAllowsAsync(b.Node.Key, tr) {
			if !hasField(b, "async") {
				out = append(out, Problem{
					Code: "E_ASYNC_FIELD_MISSING", Field: "fields", Blocking: true,
					Message: "该端点支持 async:true，但请求字段里没有 async。",
				})
			}
			// 两种合法写法：老的「202 变体」与新的「200 + 任务对象示例」。
			// 提交响应统一成 200 之后，状态码本身不再区分同步/异步，但运营手里
			// 那些按 202 写的老页面不该被这条闸门判死。
			if !hasVariant(b, 202) && !hasTaskExample(b) {
				out = append(out, Problem{
					Code: "E_ASYNC_VARIANT_MISSING", Field: "response.variants", Blocking: true,
					Message: "该端点支持 async:true，但既没有 202 变体，也没有给出任务对象的响应变体（缺 task_id 示例）。",
				})
			}
		} else if tr.AcceptsAsync {
			// 反向：这一族的协议**不支持**异步，页面却写了 async 字段 / 任务对象。
			//
			// 这是「文档承诺、代码没有」的直接入口：Seedream 页曾经有 async 字段与
			// 「已受理」变体，照文档发 async:true 会走到一个没有任何渠道能承接的端点。
			// 只删内容不加这条闸门，下次改文档的人还会把它加回来。
			if hasField(b, "async") {
				out = append(out, Problem{
					Code: "E_ASYNC_UNEXPECTED", Field: "fields", Blocking: true,
					Message: "该族的协议不支持 async:true，请求字段里不能出现 async。",
				})
			}
			if hasVariant(b, 202) || hasTaskExample(b) {
				out = append(out, Problem{
					Code: "E_ASYNC_UNEXPECTED", Field: "response.variants", Blocking: true,
					Message: "该族的协议不支持 async:true，响应变体里不能出现任务对象（含 task_id 的示例）。",
				})
			}
		}
	}

	if !strings.HasPrefix(b.EP.Path, "/") || strings.ContainsAny(b.EP.Path, " \t") {
		out = append(out, Problem{
			Code: "E_PATH_INVALID", Field: "path", Blocking: true,
			Message: "路径必须以 / 开头且不含空白。",
		})
	}
	if !hasSuccessVariant(b) {
		out = append(out, Problem{
			Code: "E_NO_SUCCESS_VARIANT", Field: "response.variants", Blocking: true,
			Message: "至少要有 1 个 2xx 响应变体。",
		})
	}

	// 4) 字段表
	seen := map[string]bool{}
	for i, f := range b.Fields {
		if f.VariantID != nil {
			continue
		}
		path := fmt.Sprintf("fields[%d]", i)
		if strings.TrimSpace(f.Name) == "" {
			out = append(out, Problem{Code: "E_FIELD_NAME_EMPTY", Field: path + ".name", Blocking: true,
				Message: "字段名不能为空。"})
		}
		switch f.In {
		case "body", "query", "path", "form", "header":
		default:
			out = append(out, Problem{Code: "E_FIELD_IN_UNKNOWN", Field: path + ".in", Blocking: true,
				Message: fmt.Sprintf("未知位置 %q（只允许 body/query/path/form/header）。", f.In)})
		}
		if f.Required == "conditional" && strings.TrimSpace(f.RequiredNote) == "" {
			out = append(out, Problem{Code: "E_FIELD_REQUIRED_NOTE", Field: path + ".required_note", Blocking: true,
				Message: "条件必填必须写明条件。"})
		}
		if f.In == "path" && f.Required != "required" {
			out = append(out, Problem{Code: "E_FIELD_OPTIONAL_FORBIDDEN", Field: path + ".required", Blocking: true,
				Message: "路径参数必须标记为必填。"})
		}
		k := f.In + "|" + f.Name
		if f.Name != "" && seen[k] {
			out = append(out, Problem{Code: "E_FIELD_DUP", Field: path + ".name", Blocking: true,
				Message: fmt.Sprintf("字段 %s 在 %s 里重复。", f.Name, f.In)})
		}
		seen[k] = true
	}

	// 5) 请求字段被写丢：这个端点必须带 JSON 请求体，但库里一条字段都没有。
	//
	// 判据来自**传输契约**（POST + 非 multipart + 非二进制），不来自文档内容：
	// 过去这里读 `apidoc.DocOf(...).Fields`，等于让"后台文档里还剩几个字段"
	// 决定"这篇文档能不能发布"—— 内容与代码互为真相，删一个字段就会改变闸门。
	if dk, ok := documentKeyOf(b); ok && transportNeedsBody(dk.Kind) && requestFieldCount(b) == 0 {
		out = append(out, Problem{Code: "E_POST_NO_FIELDS", Field: "fields", Blocking: true,
			Message: "这个端点要求 JSON 请求体，但文档里一个请求字段都没有。"})
	}

	// 7) 用词：内部用词 / 上游名 / 第三方域名（见 v7_guard.go 的说明）。
	out = append(out, guardEndpoint(b)...)
	return out
}

// hasTaskExample 判断是否有响应变体的示例里出现 task_id（= 描述了"受理后返回任务对象"）。
func hasTaskExample(b *EndpointBundle) bool {
	for i := range b.Variants {
		ex := b.Variants[i].Example
		if len(ex) == 0 {
			continue
		}
		// 示例是**保序** KV 数组（[{k,v}]），不是对象：对象在 jsonb 里不保序，
		// 而文档示例的键序有语义价值（id/task_id 在最前）。
		var kv []struct {
			K string `json:"k"`
		}
		if json.Unmarshal([]byte(ex), &kv) != nil {
			continue
		}
		for _, it := range kv {
			if it.K == "task_id" {
				return true
			}
		}
	}
	return false
}

// transportNeedsBody 判断端点是否必须声明请求字段。
//
// 只看传输契约：POST 且不是 multipart、响应不是二进制。`DELETE /v1/files/{id}`
// 这类没有 body 的端点不会被误判（方法不是 POST）。
func transportNeedsBody(k apidoc.Kind) bool {
	tr, ok := apidoc.TransportOf(k)
	if !ok {
		return false
	}
	return tr.Method == "POST" && !tr.Multipart && !tr.Binary
}

// requestFieldCount 数请求字段（不含各响应变体的返回字段）。
func requestFieldCount(b *EndpointBundle) int {
	n := 0
	for _, f := range b.Fields {
		if f.VariantID == nil {
			n++
		}
	}
	return n
}

// documentKeyOf 返回该接口绑定的文档身份；未绑定返回 false。
func documentKeyOf(b *EndpointBundle) (apidoc.DocumentKey, bool) {
	if b.EP.TransportDocID == nil {
		return apidoc.DocumentKey{}, false
	}
	return apidoc.DocumentKeyOf(*b.EP.TransportDocID)
}

// BlockingProblems 只保留阻断发布的那几条。
//
// 发布闸门、管理台红点、AI 自查都调它：把「哪几条算阻断」判断散到三处，
// 迟早出现「管理台说能发、发布接口说不能」的自相矛盾。
func BlockingProblems(ps []Problem) []Problem {
	out := make([]Problem, 0, len(ps))
	for _, p := range ps {
		if p.Blocking {
			out = append(out, p)
		}
	}
	return out
}

// CheckNode 校验节点（能不能对外发布）。
//
// CheckNode 校验节点（能不能对外发布）。
//
// 两条内容闸门：页面上一个字都没有的节点不许发布。
// 空页面比 404 更误导 —— 读者会以为文档写完了、只是自己没看懂。
//
// **刻意不判「有没有绑定模型」**：文档是内置契约，与模型上架/启用无关。
// 早先的 `E_NODE_EMPTY`（族没有成员就不许发布）与 `E_MISSING_READ_FACE`
// （成员支持异步就必须有读面）都是拿模型数据当发布判据 —— 直接后果是
// 「图像 / 音乐整块没有文档」，而日志里只有两条 blocking。
func CheckNode(b *NodeBundle) []Problem {
	out := make([]Problem, 0, 4)
	if b.Node.Kind == model.DocNodeGuide {
		if len(guideBlocks(b.Node.Blocks, "")) == 0 {
			out = append(out, Problem{Code: "E_NODE_GUIDE_EMPTY", Field: "blocks", Blocking: true,
				Message: "说明页没有正文，发布后是一张白纸。"})
		}
	} else if visibleEndpointCount(b) == 0 {
		out = append(out, Problem{Code: "E_NODE_NO_ENDPOINT", Field: "endpoints", Blocking: true,
			Message: "这个节点下没有任何已发布的接口，发布后页面上看不到可调用的接口。"})
	}
	out = append(out, guardNode(b)...)
	return out
}

// CheckAll 全站体检（启动期日志 + 管理台总览）。
func CheckAll() []Problem {
	var nodes []model.DocNode
	if err := db.DB.Find(&nodes).Error; err != nil {
		return nil
	}
	out := make([]Problem, 0, 8)
	for _, n := range nodes {
		nb, err := LoadNode(n.Key)
		if err != nil {
			continue
		}
		out = append(out, CheckNode(nb)...)
		for _, e := range nb.Eps {
			eb, err := LoadEndpointByID(e.ID)
			if err != nil {
				continue
			}
			ps := CheckEndpoint(eb)
			for i := range ps {
				ps[i].Message = n.Key + "/" + e.Key + ": " + ps[i].Message
			}
			out = append(out, ps...)
		}
	}
	return out
}

// ── 小判据 ──

func hasField(b *EndpointBundle, name string) bool {
	for _, f := range b.Fields {
		if f.VariantID == nil && f.Name == name {
			return true
		}
	}
	return false
}

func hasVariant(b *EndpointBundle, status int) bool {
	for _, v := range b.Variants {
		if v.StatusCode == status {
			return true
		}
	}
	return false
}

func hasSuccessVariant(b *EndpointBundle) bool {
	for _, v := range b.Variants {
		if v.StatusCode >= 200 && v.StatusCode < 300 {
			return true
		}
	}
	return false
}

// visibleEndpointCount 数「节点发布后读者能看到几篇」：接口已发布且未隐藏。
//
// 刻意不看节点自身状态：这个函数是给「节点能不能发布」用的，节点此刻必然还是草稿。
func visibleEndpointCount(b *NodeBundle) int {
	n := 0
	for _, e := range b.Eps {
		if e.Status == model.DocPublished && !e.Hidden {
			n++
		}
	}
	return n
}

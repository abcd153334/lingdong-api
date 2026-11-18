package agent

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/docdocs"
)

// 本文件是 v7 文档工具面（10 个工具）。
//
// 三条硬边界（改之前先读）：
//  1. **没有 publish**。AI 只能写草稿；发布是人的动作。缺位比提示词里的"请不要发布"
//     可靠一万倍 —— 工具清单就是它能力的全集。
//  2. **写内容必须绑定平台已登记端点才能发布**，但"能不能发布"不由这里判断：
//     写层与发布闸门共用 `docdocs.CheckEndpoint`，工具只把 problems 原样回传。
//     工具自己再判一遍，迟早出现"AI 说可以、发布按钮说不行"。
//  3. **入口作用域**（scope）非空时，只有显式包含 `doc` 的入口能改文档。
//     从「新建适配器」弹窗进来的会话顺手把文档改掉，是这套系统最不该发生的事。
//
// 结果是给模型读的：失败一律 `{"ok":false,"code":...,"message":...,"hint":...}`，
// 并尽量带上"下一步该调哪个工具/哪个参数"。

// docToolsV7 是工具名清单（声明、分派、单测三处共用一份）。
var docToolsV7 = []string{
	"doc_tree", "doc_node_get", "doc_node_create", "doc_node_update",
	"doc_endpoint_get", "doc_endpoint_create", "doc_endpoint_update",
	"doc_fields_write", "doc_check", "doc_seed_diff",
}

/* ------------------------------ 读 ------------------------------ */

func toolDocTree(env *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Query   string `json:"query"`
		Section string `json:"section"`
		Limit   int    `json:"limit"`
	}
	_ = strictUnmarshal(args, &in)
	t, err := docdocs.LoadAdminTree("")
	if err != nil {
		return toolError("docs_unavailable", err.Error(), ""), nil
	}
	kw := strings.ToLower(strings.TrimSpace(in.Query))
	sec := strings.TrimSpace(in.Section)
	limit := clampLimit(in.Limit)
	secs := make([]map[string]any, 0, len(t.Sections))
	shown := 0
	for _, s := range t.Sections {
		if sec != "" && s.Key != sec {
			continue
		}
		nodes := make([]map[string]any, 0, len(s.Nodes))
		for _, n := range s.Nodes {
			if kw != "" && !strings.Contains(strings.ToLower(n.Key+" "+n.Title), kw) {
				continue
			}
			if shown >= limit {
				break
			}
			eps := make([]map[string]any, 0, len(n.Endpoints))
			for _, e := range n.Endpoints {
				eps = append(eps, map[string]any{
					"key": e.Key, "name": e.Name, "method": e.Method, "path": e.Path,
					"status": e.Status, "bound": e.Bound,
				})
			}
			nodes = append(nodes, map[string]any{
				"key": n.Key, "kind": n.Kind, "title": n.Title, "status": n.Status,
				"hidden": n.Hidden, "models": n.Models, "endpoints": eps,
			})
			shown++
		}
		if len(nodes) > 0 {
			secs = append(secs, map[string]any{"key": s.Key, "name": s.Name, "nodes": nodes})
		}
	}
	out := map[string]any{
		"ok": true, "contract_version": t.Contract, "sections": secs,
		"totals": map[string]any{
			"nodes": t.Totals.Nodes, "endpoints": t.Totals.Endpoints,
			"draft_nodes": t.Totals.DraftNodes, "draft_endpoints": t.Totals.DraftEndpoints,
		},
	}
	if shown >= limit {
		out["truncated"] = true
		out["hint"] = "结果被 limit 截断：用 section 或 query 缩小范围，别一次拉全站"
	}
	return out, nil
}

func toolDocNodeGet(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Key string `json:"key"`
	}
	if err := strictUnmarshal(args, &in); err != nil || strings.TrimSpace(in.Key) == "" {
		return toolError("invalid_arguments", "key 必填", "key"), nil
	}
	n, err := docdocs.LoadAdminNode(strings.TrimSpace(in.Key))
	if err != nil {
		return toolError("not_found", "节点不存在："+in.Key+"（先用 doc_tree 找 key）", "key"), nil
	}
	eps := make([]map[string]any, 0, len(n.Endpoints))
	for _, e := range n.Endpoints {
		eps = append(eps, map[string]any{
			"key": e.Key, "name": e.Name, "method": e.Method, "path": e.Path,
			"status": e.Status, "bound": e.Bound,
		})
	}
	models := make([]string, 0, len(n.Models))
	for _, m := range n.Models {
		models = append(models, m.ID)
	}
	out := map[string]any{
		"ok": true, "key": n.Key, "kind": n.Kind, "section": n.Section,
		"title": n.Title, "summary": n.Summary, "status": n.Status,
		"hidden": n.Hidden, "version": n.Version, "sort": n.Sort,
		"models":    models,
		"endpoints": eps, "problems": n.Problems,
	}
	if n.Kind == model.DocNodeGuide {
		out["blocks"] = n.Blocks
	}
	return out, nil
}

// toolDocEndpointGet 读一篇接口文档（含可绑定端点与体检结论）。
//
// 为什么把「同板块可绑定端点」放进来：AI 写文档最常见的一步错是
// 猜一个 transport_doc_id（如 "images/generate"），而正确值只能来自契约。
// 让它多调一个工具去查，它就会跳过并凭记忆写；直接给在同一份结果里最省事。
func toolDocEndpointGet(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Node string `json:"node"`
		Key  string `json:"key"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "node"), nil
	}
	node, key := strings.TrimSpace(in.Node), strings.TrimSpace(in.Key)
	if node == "" || key == "" {
		return toolError("invalid_arguments", "node 与 key 都必填", "node"), nil
	}
	ep, err := docdocs.LoadAdminEndpoint(node, key)
	if err != nil {
		return toolError("not_found", "接口文档不存在："+node+"/"+key+"（先用 doc_tree 找 key）", "key"), nil
	}
	// 同板块可绑定端点：只给本板块，避免一次把 40 个端点全灌进上下文。
	var bindable []map[string]any
	if n, err := docdocs.LoadAdminNode(node); err == nil {
		for _, t := range docdocs.TransportOptions() {
			if t.Section != n.Section {
				continue
			}
			bindable = append(bindable, map[string]any{
				"doc_id": t.DocID, "method": t.Method, "path": t.Path,
				"accepts_async": t.AcceptsAsync,
			})
		}
	}
	return map[string]any{
		"ok": true, "node": node, "key": ep.Key, "name": ep.Name, "summary": ep.Summary,
		"method": ep.Method, "path": ep.Path, "transport_doc_id": ep.TransportID,
		"status": ep.Status, "hidden": ep.Hidden, "version": ep.Version,
		"run_mode": ep.RunMode, "run_hint": ep.RunHint,
		"fields": ep.Fields, "response": ep.Response,
		"samples": ep.Samples, "samples2": ep.Samples2,
		"example_title": ep.ExampleTitle, "example_title2": ep.ExampleTitle2,
		"presets": ep.Presets, "errors": ep.Errors,
		"problems": ep.Problems, "bindable_transports": bindable,
		"next": "改内容用 doc_endpoint_update（只提交要改的键）；只改请求字段用 doc_fields_write；改完用 doc_check 复核",
	}, nil
}

// toolDocCheck 体检（单篇 / 单节点 / 全站）。
//
// 与发布按钮同一个函数（docdocs.CheckEndpoint / CheckNode / CheckAll），
// 所以"工具说没问题"与"能发布"是同一件事。
func toolDocCheck(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Node string `json:"node"`
		Key  string `json:"key"`
	}
	_ = strictUnmarshal(args, &in)
	node, key := strings.TrimSpace(in.Node), strings.TrimSpace(in.Key)
	switch {
	case node != "" && key != "":
		ep, err := docdocs.LoadAdminEndpoint(node, key)
		if err != nil {
			return toolError("not_found", "接口文档不存在："+node+"/"+key, "key"), nil
		}
		return map[string]any{
			"ok": true, "scope": "endpoint", "key": node + "/" + key,
			"problems": ep.Problems, "blocking": len(docdocs.BlockingProblems(ep.Problems)),
		}, nil
	case node != "":
		n, err := docdocs.LoadAdminNode(node)
		if err != nil {
			return toolError("not_found", "节点不存在："+node, "node"), nil
		}
		return map[string]any{
			"ok": true, "scope": "node", "key": node,
			"problems": n.Problems, "blocking": len(docdocs.BlockingProblems(n.Problems)),
		}, nil
	default:
		ps := docdocs.CheckAll()
		return map[string]any{
			"ok": true, "scope": "all",
			"problems": ps, "blocking": len(docdocs.BlockingProblems(ps)),
		}, nil
	}
}

// toolDocSeedDiff 只读差异：出厂种子里有哪些节点/接口还没落库。
//
// 不写库：播种在启动期做（SeedDocs），让 AI 能触发播种等于给它一个
// 「重建全站文档」的按钮，而它对后果不承担任何责任。
func toolDocSeedDiff(_ *toolEnv, _ json.RawMessage) (any, error) {
	var nodes []model.DocNode
	if err := db.DB.Find(&nodes).Error; err != nil {
		return toolError("docs_unavailable", err.Error(), ""), nil
	}
	have := map[string]bool{}
	for _, n := range nodes {
		have[n.Key] = true
	}
	var eps []model.DocEndpoint
	if err := db.DB.Select("node_id", "key").Find(&eps).Error; err != nil {
		return toolError("docs_unavailable", err.Error(), ""), nil
	}
	byID := map[uint64]string{}
	for _, n := range nodes {
		byID[n.ID] = n.Key
	}
	epHave := map[string]bool{}
	for _, e := range eps {
		epHave[byID[e.NodeID]+"/"+e.Key] = true
	}
	missingNodes := make([]string, 0, 8)
	missingEps := make([]string, 0, 16)
	for _, s := range apidoc.SeedDocs() {
		if !have[s.Key] {
			missingNodes = append(missingNodes, s.Key)
			continue
		}
		for _, e := range s.Endpoints {
			if !epHave[s.Key+"/"+e.Key] {
				missingEps = append(missingEps, s.Key+"/"+e.Key)
			}
		}
	}
	sort.Strings(missingNodes)
	sort.Strings(missingEps)
	return map[string]any{
		"ok": true, "missing_nodes": missingNodes, "missing_endpoints": missingEps,
		"note": "缺失项会在后端启动时自动补齐（种子只补缺、不覆盖已有行）；本工具只报告，不写库",
	}, nil
}

/* ------------------------------ 写 ------------------------------ */

// toolDocNodeCreate 新建节点（永远是草稿）。
func toolDocNodeCreate(env *toolEnv, args json.RawMessage) (any, error) {
	if !env.scopeAllows(EntityDoc) {
		return outOfScopeDoc(), nil
	}
	var in struct {
		Key     string `json:"key"`
		Kind    string `json:"kind"`
		Title   string `json:"title"`
		Section string `json:"section"`
		Summary string `json:"summary"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "key"), nil
	}
	n, err := docdocs.CreateNode(docdocs.NodeCreateInput{
		Key: in.Key, Kind: in.Kind,
		NodeInput: docdocs.NodeInput{
			Title: in.Title, Section: in.Section, Summary: in.Summary,
		},
	}, adminIDOf(env))
	if err != nil {
		return docWriteError(err), nil
	}
	audit.RecordOrWarn(db.DB, adminIDOf(env), audit.ActionDocsNodeCreate, "doc_node", n.Key, nil)
	return map[string]any{
		"ok": true, "key": n.Key, "status": n.Status, "version": n.Version,
		"next": "节点是草稿；接着用 doc_endpoint_create 建接口，或用 doc_node_update 改标题",
	}, nil
}

// toolDocNodeUpdate 改节点元信息（增量：只提交要改的键）。
func toolDocNodeUpdate(env *toolEnv, args json.RawMessage) (any, error) {
	if !env.scopeAllows(EntityDoc) {
		return outOfScopeDoc(), nil
	}
	var in struct {
		Key     string                    `json:"key"`
		Title   *string                   `json:"title"`
		Summary *string                   `json:"summary"`
		Section *string                   `json:"section"`
		Hidden  *bool                     `json:"hidden"`
		Sort    *int                      `json:"sort"`
		Blocks  *[]docdocs.GuideBlockView `json:"blocks"`
		Version int                       `json:"version"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "key"), nil
	}
	key := strings.TrimSpace(in.Key)
	if key == "" {
		return toolError("invalid_arguments", "key 必填", "key"), nil
	}
	cur, err := docdocs.LoadAdminNode(key)
	if err != nil {
		return toolError("not_found", "节点不存在："+key, "key"), nil
	}
	// 增量合并的基准必须是**库里的现值**：漏一项就会被零值覆盖，
	// 而零值在这里等于"把这栏清空"（标题空、隐藏=false 都会真的改库）。
	body := docdocs.NodeInput{
		Title: cur.Title, Summary: cur.Summary, Section: cur.Section,
		Hidden: cur.Hidden, Sort: cur.Sort,
		Version: cur.Version,
	}
	if cur.Kind == model.DocNodeGuide {
		body.Blocks = cur.Blocks
	}
	if in.Title != nil {
		body.Title = *in.Title
	}
	if in.Summary != nil {
		body.Summary = *in.Summary
	}
	if in.Section != nil {
		body.Section = *in.Section
	}
	if in.Hidden != nil {
		body.Hidden = *in.Hidden
	}
	if in.Sort != nil {
		body.Sort = *in.Sort
	}
	if in.Blocks != nil {
		body.Blocks = *in.Blocks
	}
	if in.Version > 0 {
		body.Version = in.Version
	}
	n, err := docdocs.SaveNode(key, body, model.DocOriginAI, adminIDOf(env))
	if err != nil {
		if err == docdocs.ErrDocVersion {
			return map[string]any{
				"ok": false, "code": "version_conflict",
				"message": "这篇节点刚被改过", "current_version": cur.Version,
				"hint": "重新 doc_node_get 读现值，再按现值重做本次改动",
			}, nil
		}
		return docWriteError(err), nil
	}
	audit.RecordOrWarn(db.DB, adminIDOf(env), audit.ActionDocsNodeSave, "doc_node", key, nil)
	return map[string]any{
		"ok": true, "key": n.Key, "status": n.Status, "version": n.Version,
		"problems": n.Problems, "blocking": len(docdocs.BlockingProblems(n.Problems)),
		"next": "已保存（内容立即生效）；发布由人工在管理台完成（doc_* 工具没有发布能力）",
	}, nil
}

// toolDocEndpointCreate 新建接口（草稿）。
func toolDocEndpointCreate(env *toolEnv, args json.RawMessage) (any, error) {
	if !env.scopeAllows(EntityDoc) {
		return outOfScopeDoc(), nil
	}
	var in struct {
		Node        string                `json:"node"`
		Key         string                `json:"key"`
		Name        string                `json:"name"`
		Summary     string                `json:"summary"`
		TransportID string                `json:"transport_doc_id"`
		Method      string                `json:"method"`
		Path        string                `json:"path"`
		RunMode     string                `json:"run_mode"`
		Fields      []docdocs.FieldView   `json:"fields"`
		Response    docdocs.AdminResponse `json:"response"`
		Samples     *docdocs.SamplesView  `json:"samples"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "node"), nil
	}
	node, key := strings.TrimSpace(in.Node), strings.TrimSpace(in.Key)
	if node == "" || key == "" {
		return toolError("invalid_arguments", "node 与 key 都必填", "node"), nil
	}
	ep, err := docdocs.CreateEndpoint(node, docdocs.EndpointCreateInput{
		Key: key,
		EndpointInput: docdocs.EndpointInput{
			Name: in.Name, Summary: in.Summary, TransportID: in.TransportID,
			Method: in.Method, Path: in.Path, RunMode: in.RunMode,
			Fields: in.Fields, Response: in.Response, Samples: in.Samples,
		},
	}, adminIDOf(env))
	if err != nil {
		return docWriteError(err), nil
	}
	audit.RecordOrWarn(db.DB, adminIDOf(env), audit.ActionDocsEndpointCreate, "doc_endpoint",
		node+"/"+key, nil)
	return map[string]any{
		"ok": true, "node": node, "key": ep.Key, "status": ep.Status, "version": ep.Version,
		"problems": ep.Problems, "blocking": len(docdocs.BlockingProblems(ep.Problems)),
		"next": "用 doc_endpoint_update 补字段/响应/示例；绑定不上的端点先看 doc_endpoint_get 的 bindable_transports",
	}, nil
}

// endpointPatch 是 doc_endpoint_update 的参数（指针 = 这次不改）。
type endpointPatch struct {
	Node    string `json:"node"`
	Key     string `json:"key"`
	Version int    `json:"version"`

	Name        *string   `json:"name"`
	Summary     *string   `json:"summary"`
	Method      *string   `json:"method"`
	Path        *string   `json:"path"`
	TransportID *string   `json:"transport_doc_id"`
	Aliases     *[]string `json:"aliases"`
	RunMode     *string   `json:"run_mode"`
	RunHint     *string   `json:"run_hint"`
	MaxBody     *string   `json:"max_body"`
	Hidden      *bool     `json:"hidden"`
	Sort        *int      `json:"sort"`

	Fields        *[]docdocs.FieldView   `json:"fields"`
	Response      *docdocs.AdminResponse `json:"response"`
	Samples       *docdocs.SamplesView   `json:"samples"`
	Samples2      *docdocs.SamplesView   `json:"samples2"`
	ExampleTitle  *string                `json:"example_title"`
	ExampleTitle2 *string                `json:"example_title2"`
	Presets       *[]docdocs.AdminPreset `json:"presets"`
	Errors        *[]docdocs.AdminError  `json:"errors"`
}

// toolDocEndpointUpdate 增量更新一篇接口文档（只提交要改的键）。
//
// 数组是**整体替换**而不是合并：字段表有顺序、变体有删除语义，
// "合并"会让模型想删一行时必须造一个魔法值。这一点必须写在 schema 里。
func toolDocEndpointUpdate(env *toolEnv, args json.RawMessage) (any, error) {
	if !env.scopeAllows(EntityDoc) {
		return outOfScopeDoc(), nil
	}
	var in endpointPatch
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "node"), nil
	}
	node, key := strings.TrimSpace(in.Node), strings.TrimSpace(in.Key)
	if node == "" || key == "" {
		return toolError("invalid_arguments", "node 与 key 都必填", "node"), nil
	}
	cur, err := docdocs.LoadAdminEndpoint(node, key)
	if err != nil {
		return toolError("not_found", "接口文档不存在："+node+"/"+key, "key"), nil
	}
	body := docdocs.EndpointInput{
		Name: cur.Name, Summary: cur.Summary, Method: cur.Method, Path: cur.Path,
		TransportID: cur.TransportID, Aliases: cur.Aliases, RunMode: cur.RunMode,
		RunHint: cur.RunHint, MaxBody: cur.MaxBody, Hidden: cur.Hidden, Sort: cur.Sort,
		Version:       cur.Version,
		Fields:        cur.Fields,
		Response:      cur.Response,
		Samples:       cur.Samples,
		Samples2:      cur.Samples2,
		ExampleTitle:  cur.ExampleTitle,
		ExampleTitle2: cur.ExampleTitle2,
		Presets:       cur.Presets,
		Errors:        cur.Errors,
	}
	if in.Name != nil {
		body.Name = *in.Name
	}
	if in.Summary != nil {
		body.Summary = *in.Summary
	}
	if in.Method != nil {
		body.Method = *in.Method
	}
	if in.Path != nil {
		body.Path = *in.Path
	}
	if in.TransportID != nil {
		body.TransportID = *in.TransportID
	}
	if in.Aliases != nil {
		body.Aliases = *in.Aliases
	}
	if in.RunMode != nil {
		body.RunMode = *in.RunMode
	}
	if in.RunHint != nil {
		body.RunHint = *in.RunHint
	}
	if in.MaxBody != nil {
		body.MaxBody = *in.MaxBody
	}
	if in.Hidden != nil {
		body.Hidden = *in.Hidden
	}
	if in.Sort != nil {
		body.Sort = *in.Sort
	}
	if in.Fields != nil {
		body.Fields = *in.Fields
	}
	if in.Response != nil {
		body.Response = *in.Response
	}
	if in.Samples != nil {
		body.Samples = in.Samples
	}
	if in.Samples2 != nil {
		body.Samples2 = in.Samples2
	}
	if in.ExampleTitle != nil {
		body.ExampleTitle = *in.ExampleTitle
	}
	if in.ExampleTitle2 != nil {
		body.ExampleTitle2 = *in.ExampleTitle2
	}
	if in.Presets != nil {
		body.Presets = *in.Presets
	}
	if in.Errors != nil {
		body.Errors = *in.Errors
	}
	if in.Version > 0 {
		body.Version = in.Version
	}
	ep, err := docdocs.SaveEndpoint(node, key, body, model.DocOriginAI, adminIDOf(env))
	if err != nil {
		if err == docdocs.ErrDocVersion {
			return map[string]any{
				"ok": false, "code": "version_conflict",
				"message": "这篇文档刚被改过", "current_version": cur.Version,
				"hint": "重新 doc_endpoint_get 读现值，再按现值重做本次改动（不要整份重写）",
			}, nil
		}
		return docWriteError(err), nil
	}
	audit.RecordOrWarn(db.DB, adminIDOf(env), audit.ActionDocsEndpointSave, "doc_endpoint",
		node+"/"+key, nil)
	return writeAck(node, ep, "已保存为草稿；发布由人工完成"), nil
}

// toolDocFieldsWrite 只替换请求字段（字段表很长时用它，避免整篇回传）。
func toolDocFieldsWrite(env *toolEnv, args json.RawMessage) (any, error) {
	if !env.scopeAllows(EntityDoc) {
		return outOfScopeDoc(), nil
	}
	var in struct {
		Node    string              `json:"node"`
		Key     string              `json:"key"`
		Version int                 `json:"version"`
		Fields  []docdocs.FieldView `json:"fields"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "node"), nil
	}
	node, key := strings.TrimSpace(in.Node), strings.TrimSpace(in.Key)
	if node == "" || key == "" {
		return toolError("invalid_arguments", "node 与 key 都必填", "node"), nil
	}
	if in.Version <= 0 {
		return toolError("invalid_arguments", "version 必填：先 doc_endpoint_get 读现值", "version"), nil
	}
	ep, err := docdocs.WriteFields(node, key, in.Fields, in.Version, model.DocOriginAI, adminIDOf(env))
	if err != nil {
		return docWriteError(err), nil
	}
	audit.RecordOrWarn(db.DB, adminIDOf(env), audit.ActionDocsEndpointFields, "doc_endpoint",
		node+"/"+key, nil)
	return writeAck(node, ep, "请求字段已整体替换（不是合并）；响应/示例未动"), nil
}

/* ------------------------------ 小工具 ------------------------------ */

// writeAck 是写工具的**统一回执**：模型靠 problems 自我修正，靠 next 决定下一步。
func writeAck(node string, ep *docdocs.AdminEndpoint, note string) map[string]any {
	return map[string]any{
		"ok": true, "node": node, "key": ep.Key, "status": ep.Status,
		"version": ep.Version, "problems": ep.Problems,
		"blocking": len(docdocs.BlockingProblems(ep.Problems)),
		"note":     note,
		"next":     "有 blocking 项就按 field 定位改；没有就让运营在管理台点发布（你不能发布）",
	}
}

// docWriteError 把写层错误翻成模型能行动的结构。
func docWriteError(err error) map[string]any {
	var blocked *docdocs.BlockedError
	switch {
	case errors.As(err, &blocked):
		return map[string]any{
			"ok": false, "code": "publish_blocked",
			"message": "体检未通过", "problems": blocked.Problems,
			"hint": "按 problems 里的 field 逐条改；不要整篇重写",
		}
	case err == docdocs.ErrDocKeyTaken:
		return map[string]any{
			"ok": false, "code": "key_exists",
			"message": "这个 key 已被占用（同一节点内唯一）", "hint": "换一个 key，不要删除已有那篇",
		}
	case err == docdocs.ErrDocNotFound:
		return map[string]any{"ok": false, "code": "not_found", "message": "节点不存在，先用 doc_tree 找 key"}
	case err == docdocs.ErrDocEndpointNotFound:
		return map[string]any{"ok": false, "code": "not_found", "message": "接口文档不存在，先用 doc_tree 找 key"}
	case err == docdocs.ErrDocVersion:
		return map[string]any{
			"ok": false, "code": "version_conflict",
			"message": "这篇文档刚被改过", "hint": "重新 doc_endpoint_get 读现值再改",
		}
	default:
		return toolError("invalid_arguments", err.Error(), "")
	}
}

// scopeAllows 判断本次入口是否允许触碰某类实体（空 scope = 不限制）。
func (e *toolEnv) scopeAllows(entity string) bool {
	if e == nil || len(e.Scope) == 0 {
		return true
	}
	for _, s := range e.Scope {
		if s == entity {
			return true
		}
	}
	return false
}

func outOfScopeDoc() map[string]any {
	return map[string]any{
		"ok": false, "code": "out_of_scope",
		"message": "当前入口只允许改配置，不允许改对外文档",
		"hint":    "请到「文档」页发起文档编辑，或让运营在管理台文档工作台里改",
	}
}

func adminIDOf(env *toolEnv) uint64 {
	if env == nil {
		return 0
	}
	return env.AdminID
}

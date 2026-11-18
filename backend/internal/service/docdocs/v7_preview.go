package docdocs

import (
	"strings"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 草稿投影：把**还没保存的编辑内容**渲染成对外页面。
//
// 为什么不在前端拼预览：字段表分组、响应变体、示例代码全在投影层，
// 前端再拼一份的结果是「预览好看、发布后变形」，而两边都自称是同一篇文档。
//
// 这里刻意**不写库**：预览是一个纯函数，输入编辑态、输出公开投影。
// 返回值与 `GET /api/docs/nodes/:key/endpoints/:ep` 的 data 完全同形，
// 前端因此可以同一套组件渲染「草稿」与「线上」。
//
// 构造链与保存完全共用（`draftOf` → bundle → `ProjectEndpoint`）：
// 以前预览自己拼了一遍行结构，于是漏 aliases、不设 phase、把顶层 kind/envelope
// 广播给所有变体 —— 预览与发布后长得不一样，运营根本看不出差异。

// PreviewEndpointInput 用编辑态内容生成公开投影（不落库）。
func PreviewEndpointInput(nodeKey, epKey string, in EndpointInput, base string) (*Endpoint, error) {
	nb, err := LoadNode(nodeKey)
	if err != nil {
		return nil, ErrDocNotFound
	}
	// 复用库里已有行的状态/版本：预览要回答的是「保存这一稿之后页面长什么样」，
	// 而不是「如果这篇是新写的」。
	var prev model.DocEndpoint
	if cur, e := loadEndpoint(db.DB, nodeKey, epKey); e == nil {
		prev = cur.EP
	}
	b, err := bundleOfInput(nb.Node, prev, in)
	if err != nil {
		return nil, err
	}
	out := ProjectEndpoint(b, base)
	return &out, nil
}

// PreviewNodeInput 用当前编辑内容渲染节点（说明页正文 / 接口清单）。
//
// 与保存共用同一套字段映射（标题/摘要/板块/正文块）；接口清单仍取库里已存在的接口，
// 因为「还没保存的接口」在节点预览里没有意义（它自己有一份预览）。
func PreviewNodeInput(key string, in NodeInput, base string) (*Node, error) {
	nb, err := LoadNode(key)
	if err != nil {
		return nil, ErrDocNotFound
	}
	n := nb.Node
	n.Title = strings.TrimSpace(in.Title)
	n.Summary = strings.TrimSpace(in.Summary)
	n.Hidden = in.Hidden
	n.Sort = in.Sort
	if s := strings.TrimSpace(in.Section); s != "" {
		n.Section = s
	}
	if n.Kind == model.DocNodeGuide {
		n.Blocks = blocksJSON(in.Blocks)
	}
	out := ProjectNode(&NodeBundle{Node: n, Eps: nb.Eps}, base)
	return &out, nil
}

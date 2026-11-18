package docdocs

import (
	"strings"

	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 加载层：三级按需，禁止全量快照。
//
// 为什么不能一次全装：字段行量级 = 接口数 × 15，加上变体/枚举/示例/预设/错误码
// 会上千行；每个页面请求全表扫一遍，首屏就会被拖成几百毫秒。
// 三级各司其职：Tree（侧栏索引）→ Node（节点 + 成员）→ Endpoint（单篇全文）。

// Tree 是目录所需的全部索引数据（不含字段）。
type Tree struct {
	Nodes     []model.DocNode
	Endpoints map[uint64][]model.DocEndpoint
}

// LoadTree 装载已发布的节点与接口索引。
func LoadTree() (*Tree, error) {
	var nodes []model.DocNode
	if err := db.DB.Where("status = ? AND hidden = ?", model.DocPublished, false).
		Order("section, sort, key").Find(&nodes).Error; err != nil {
		return nil, err
	}
	t := &Tree{Nodes: nodes, Endpoints: map[uint64][]model.DocEndpoint{}}
	if len(nodes) == 0 {
		return t, nil
	}
	ids := make([]uint64, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	var eps []model.DocEndpoint
	if err := db.DB.Where("node_id IN ? AND status = ? AND hidden = ?", ids, model.DocPublished, false).
		Order("node_id, sort, key").Find(&eps).Error; err != nil {
		return nil, err
	}
	for _, e := range eps {
		t.Endpoints[e.NodeID] = append(t.Endpoints[e.NodeID], e)
	}
	return t, nil
}

// NodeBundle 是一个节点的全部内容（含接口索引）。
type NodeBundle struct {
	Node model.DocNode
	Eps  []model.DocEndpoint
}

// LoadNode 装载单个节点。
func LoadNode(key string) (*NodeBundle, error) { return loadNode(db.DB, key) }

// loadNode 是带连接的版本：种子与发布闸门在事务里体检时必须传 tx，
// 否则会读到**提交前**的旧快照（表现是「刚插的字段体检说没有」）。
func loadNode(q *gorm.DB, key string) (*NodeBundle, error) {
	var n model.DocNode
	if err := q.Where("key = ?", key).Take(&n).Error; err != nil {
		return nil, err
	}
	return loadNodeByID(q, n.ID)
}

func loadNodeByID(q *gorm.DB, id uint64) (*NodeBundle, error) {
	var n model.DocNode
	if err := q.Where("id = ?", id).Take(&n).Error; err != nil {
		return nil, err
	}
	var eps []model.DocEndpoint
	if err := q.Where("node_id = ?", n.ID).Order("sort, key").Find(&eps).Error; err != nil {
		return nil, err
	}
	return &NodeBundle{Node: n, Eps: eps}, nil
}

// EndpointBundle 是一篇接口文档的全文。
type EndpointBundle struct {
	Node     model.DocNode
	EP       model.DocEndpoint
	Fields   []model.DocEndpointField
	Enums    map[uint64][]model.DocFieldEnum
	Variants []model.DocResponseVariant
	Presets  []model.DocPreset
	Errors   []model.DocEndpointError
}

// LoadPublicNode / LoadPublicEndpoint 是**公开读口的唯一入口**。
//
// 与 LoadNode/LoadEndpoint 的差别只有一条：所属板块被隐藏时视为不存在。
// 管理台必须仍能打开它（`LoadAdminNode` 走 LoadNode），所以这层单独存在，
// 而不是把可见性判断塞进装载函数里。
func LoadPublicNode(key string) (*NodeBundle, error) {
	b, err := LoadNode(key)
	if err != nil {
		return nil, err
	}
	if !sectionVisible(b.Node.Section) {
		return nil, ErrDocNotFound
	}
	return b, nil
}

func LoadPublicEndpoint(nodeKey, epKey string) (*EndpointBundle, error) {
	b, err := LoadEndpoint(nodeKey, epKey)
	if err != nil {
		return nil, err
	}
	if !sectionVisible(b.Node.Section) {
		return nil, ErrDocNotFound
	}
	return b, nil
}

// LoadEndpoint 装载单篇接口文档（页面/编辑器/体检共用）。
func LoadEndpoint(nodeKey, epKey string) (*EndpointBundle, error) {
	return loadEndpoint(db.DB, nodeKey, epKey)
}

// loadEndpoint 是带连接的版本（事务内体检用，理由见 loadNode）。
func loadEndpoint(q *gorm.DB, nodeKey, epKey string) (*EndpointBundle, error) {
	var n model.DocNode
	if err := q.Where("key = ?", nodeKey).Take(&n).Error; err != nil {
		return nil, err
	}
	var ep model.DocEndpoint
	if err := q.Where("node_id = ? AND key = ?", n.ID, epKey).Take(&ep).Error; err != nil {
		return nil, err
	}
	return loadBundleTx(q, n, ep)
}

// LoadEndpointByID 供写入/体检使用（已知主键）。
func LoadEndpointByID(id uint64) (*EndpointBundle, error) {
	return loadEndpointByID(db.DB, id)
}

func loadEndpointByID(q *gorm.DB, id uint64) (*EndpointBundle, error) {
	var ep model.DocEndpoint
	if err := q.Where("id = ?", id).Take(&ep).Error; err != nil {
		return nil, err
	}
	var n model.DocNode
	if err := q.Where("id = ?", ep.NodeID).Take(&n).Error; err != nil {
		return nil, err
	}
	return loadBundleTx(q, n, ep)
}

func loadBundle(n model.DocNode, ep model.DocEndpoint) (*EndpointBundle, error) {
	return loadBundleTx(db.DB, n, ep)
}

// loadBundleTx 读一篇接口的全部子行。q 可以是事务连接：
// 种子插完子行要立刻体检，读全局连接只会看到提交前的空数据。
func loadBundleTx(q *gorm.DB, n model.DocNode, ep model.DocEndpoint) (*EndpointBundle, error) {
	b := &EndpointBundle{Node: n, EP: ep, Enums: map[uint64][]model.DocFieldEnum{}}
	if err := q.Where("endpoint_id = ?", ep.ID).Order("variant_id NULLS FIRST, sort, id").
		Find(&b.Fields).Error; err != nil {
		return nil, err
	}
	if len(b.Fields) > 0 {
		ids := make([]uint64, 0, len(b.Fields))
		for _, f := range b.Fields {
			ids = append(ids, f.ID)
		}
		var enums []model.DocFieldEnum
		if err := q.Where("field_id IN ?", ids).Order("field_id, sort, value").Find(&enums).Error; err != nil {
			return nil, err
		}
		for _, e := range enums {
			b.Enums[e.FieldID] = append(b.Enums[e.FieldID], e)
		}
	}
	if err := q.Where("endpoint_id = ?", ep.ID).Order("sort, id").Find(&b.Variants).Error; err != nil {
		return nil, err
	}
	if err := q.Where("endpoint_id = ?", ep.ID).Order("sort, id").Find(&b.Presets).Error; err != nil {
		return nil, err
	}
	if err := q.Where("endpoint_id = ?", ep.ID).Order("sort, id").Find(&b.Errors).Error; err != nil {
		return nil, err
	}
	return b, nil
}

// visibleEndpoint 是**可见性唯一判据**（侧栏 / 面包屑 / 公开 URL / Markdown 全调它）。
//
// 拆成两个函数就会重演旧系统的坑：组页按「有在售成员」判、接口页按「没 hidden」判，
// 结果同一篇文档在两处一边可见一边 404，而没有任何报错。
func visibleEndpoint(n model.DocNode, e model.DocEndpoint) bool {
	return n.Status == model.DocPublished && !n.Hidden &&
		e.Status == model.DocPublished && !e.Hidden
}

// sectionOf 返回板块元信息（未知板块回退为 id，不隐藏页面）。
//
// 每次调用查一次 `doc_sections`（7 行）：一个页面最多两次，代价可以忽略，
// 换来的是"改板块名立刻生效"，不需要任何缓存与失效逻辑。
func sectionOf(id string) SectionView {
	// 纯单元测试（没连库）回退成 id：文档渲染与数据库无关这件事要一直成立，
	// 否则「投影层不许读模型/货架」这条规则会在测试环境里先破一次。
	if db.DB == nil {
		return SectionView{Key: id, Name: id}
	}
	idx, err := SectionIndex()
	if err == nil {
		if s, ok := idx[id]; ok {
			return s
		}
	}
	return SectionView{Key: id, Name: id}
}

// sectionVisible 只给公开读口用：板块隐藏 ⇒ 其下节点与接口一律视为不存在。
func sectionVisible(id string) bool {
	return !sectionOf(id).Hidden
}

// trimSlash 去掉 base 末尾的斜杠（示例里的 URL 不能出现 `//v1`）。
func trimSlash(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }

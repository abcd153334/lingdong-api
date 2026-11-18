package docdocs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pgerr"
)

// 写入层（v7）：**文档内容只有这一个写入口**。
//
// 五条边界（改之前先读）：
//  1. key 是身份，建了就不许改：改 key 等于换 URL，旧链接 404，且「这篇文档讲的是谁」
//     与模型的绑定会静默断开。想改就删了重建（管理台要确认）。
//  2. 保存一律退回草稿：改一个字立刻对外生效是危险的，发布必须是显式动作。
//  3. 子行整篇替换（字段/变体/示例/预设/错误码）：编辑器一次能改十几行，
//     逐行 diff 省下的写放大抵不过「漏删一行 ⇒ 对外多出一个不存在的参数」。
//     并发由 version 乐观锁兜住（409），不靠逐行锁。
//  4. 能写什么由 CheckEndpoint 决定（唯一判据），这里只拒「存不进去」的输入
//     （key 非法 / 超长 / 模型已被别的族占用）—— 不要在保存层再发明一套内容规则，
//     两套规则打架时，用户会看到「保存成功但发布说不行」。
//  5. 未绑定平台端点的文档可以存草稿、不能发布（E_UNBOUND）。这是「不许伪造能力」
//     的物理闸门，任何写路径都不得绕过它。

var (
	// ErrDocNotFound 文档节点不存在。
	ErrDocNotFound = errors.New("文档节点不存在")
	// ErrDocEndpointNotFound 接口文档不存在。
	ErrDocEndpointNotFound = errors.New("这篇接口文档不存在")
	// ErrDocVersion 乐观锁冲突（别人先保存了）。
	ErrDocVersion = errors.New("文档已被其他人保存，请刷新后再改")
	// ErrDocKeyTaken 标识被占用（节点 key 全站唯一；接口 key 也全站唯一 ——
	// 三族的同名页面各自有标识，撞车说明复制了别人的那一篇）。
	ErrDocKeyTaken = errors.New("这个标识已被占用")
)

// InputError 是「内容不合法」的错误（key 格式、板块不存在、枚举重复、模型被占用…）。
//
// 必须是独立类型而不是 fmt.Errorf：控制器要靠它决定 400 还是 500。
// 用字符串匹配（strings.Contains）判"是不是输入错误"会在某天改一句文案后
// 把 500 变成 400 —— 那是最难查的一类假象（用户看到"参数不对"，日志里却是数据库挂了）。
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

// inputErrf 构造一条输入错误（与 fmt.Errorf 同参，只是带上类型）。
func inputErrf(format string, a ...any) error {
	return &InputError{Msg: fmt.Sprintf(format, a...)}
}

// BlockedError 是「体检没通过」：带上逐条结论，让前端能列出改哪里。
type BlockedError struct{ Problems []Problem }

func (e *BlockedError) Error() string { return "文档体检未通过，不能发布" }

// NodeInput 是节点的可编辑内容（kind/key 只在创建时给）。
type NodeInput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Section string `json:"section"`
	// Protocol 只在创建时选一次，之后由节点的 kind 决定。
	Protocol string           `json:"protocol"`
	Hidden   bool             `json:"hidden"`
	Sort     int              `json:"sort"`
	Version  int              `json:"version"`
	Blocks   []GuideBlockView `json:"blocks"`
}

// NodeCreateInput 在 NodeInput 之上加身份字段。
type NodeCreateInput struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	NodeInput
}

// EndpointInput 是一篇接口文档的可编辑内容。
//
// `doc` tag 是表单元信息的唯一声明（见 v7_spec.go）：前台显示的每一列，
// 后台必须有一个能改它的控件；标 `-` 的是不许手填的派生量。
type EndpointInput struct {
	Name        string `json:"name"`
	Summary     string `json:"summary"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	TransportID string `json:"transport_doc_id"`
	// Aliases 是同一路径的其他写法，前台显示在标题路径旁。
	Aliases []string `json:"aliases"`
	// RunMode/RunHint/MaxBody 是「怎么试跑」的元信息，属文档内容，可编辑。
	RunMode string `json:"run_mode"`
	RunHint string `json:"run_hint"`
	MaxBody string `json:"max_body"`
	// Notes 是接口级注意事项，一行一条。它属于内容，不是结构：
	// 计费口径、超时、409 语义都会变，必须能在这里改。
	Notes   []string `json:"notes"`
	Hidden  bool     `json:"hidden"`
	Sort    int      `json:"sort"`
	Version int      `json:"version"`
	// 示例代码是内容：编辑器改的就是这四段，保存后页面上的就是它。
	// 只写 `{{base_url}}` 占位符，域名在投影期替换（换域名不用逐篇改文档）。
	Samples       *SamplesView `json:"samples"`
	Samples2      *SamplesView `json:"samples2"`
	ExampleTitle  string       `json:"example_title"`
	ExampleTitle2 string       `json:"example_title2"`

	Fields   []FieldView   `json:"fields"`
	Response AdminResponse `json:"response"`
	Presets  []AdminPreset `json:"presets"`
	Errors   []AdminError  `json:"errors"`
}

// AdminError 是错误码的**编辑态**（比 ErrorView 多一个来源标记）。
//
// 来源（seed/ai/manual）不对外下发：读者只关心「什么情况会拿到这条错误」。
// 但编辑时必须能原样回传，否则每次保存都把种子错误码改写成 manual，
// 「哪些是出厂、哪些是人写的」这条线索就永久丢了。
type AdminError struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Desc   string `json:"desc"`
	Source string `json:"source"`
}

// EndpointCreateInput 在 EndpointInput 之上加身份字段。
type EndpointCreateInput struct {
	Key string `json:"key"`
	EndpointInput
}

// ── 节点 ──

// CreateNode 新建一个节点（草稿）。
func CreateNode(in NodeCreateInput, adminID uint64) (*AdminNode, error) {
	key := strings.TrimSpace(in.Key)
	if err := validKey(key, 96, "节点标识"); err != nil {
		return nil, err
	}
	switch in.Kind {
	case model.DocNodeProtocol, model.DocNodeFamily, model.DocNodeGeneral, model.DocNodeGuide:
	default:
		return nil, inputErrf("节点类型只能是 protocol/family/general/guide")
	}
	if err := limit(in.Title, 160, "标题"); err != nil {
		return nil, err
	}
	sec := strings.TrimSpace(in.Section)
	if !sectionExists(sec) {
		return nil, inputErrf("板块 %q 不存在，请选择已有板块", sec)
	}
	proto := ""
	if in.Kind == model.DocNodeProtocol {
		proto = strings.TrimSpace(in.Protocol)
		if _, ok := apidoc.ProtocolDocOf(proto); !ok {
			return nil, inputErrf("协议 %q 不存在（只允许协议文档里已声明的那几篇）", proto)
		}
	}
	n := model.DocNode{
		Key: key, Kind: in.Kind, Section: sec, Protocol: proto,
		Title: strings.TrimSpace(in.Title), Summary: strings.TrimSpace(in.Summary),
		Sort: in.Sort, Hidden: in.Hidden,
		Status: model.DocDraft, Origin: model.DocOriginManual, UpdatedBy: adminID,
		Version: 1,
	}
	if in.Kind == model.DocNodeGuide {
		n.Blocks = blocksJSON(in.Blocks)
	}
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if n.Sort == 0 {
			n.Sort = nextSort(tx, "doc_nodes", "section = ?", sec)
		}
		if err := tx.Create(&n).Error; err != nil {
			return mapWriteError(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return LoadAdminNode(key)
}

// SaveNode 保存节点（退回草稿）。
//
// origin 由调用方给（manual / ai）：管理台与 AI 的改动必须能区分开，
// 否则「这一行是谁写的」永远查不出来（种子刷新、审计都依赖它）。
func SaveNode(key string, in NodeInput, origin string, adminID uint64) (*AdminNode, error) {
	if err := limit(in.Title, 160, "标题"); err != nil {
		return nil, err
	}
	if in.Version <= 0 {
		return nil, inputErrf("保存必须带上当前版本号：先刷新页面再改，避免覆盖别人的改动")
	}
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var n model.DocNode
		if err := tx.Where("key = ?", key).Take(&n).Error; err != nil {
			return ErrDocNotFound
		}
		upd := map[string]any{
			"title": strings.TrimSpace(in.Title), "summary": strings.TrimSpace(in.Summary),
			"hidden": in.Hidden, "sort": in.Sort,
			"version": in.Version + 1, "updated_by": adminID,
			"origin": origin, "updated_at": time.Now(),
		}
		// 节点同理：保存不改可见性。已发布的节点改个标题不该把整族接口从侧栏摘掉。
		if n.Status == model.DocPublished {
			upd["status"] = model.DocPublished
		} else {
			upd["status"] = model.DocDraft
			upd["published_at"] = nil
		}
		if in.Section != "" && in.Section != n.Section {
			if !sectionExists(in.Section) {
				return inputErrf("板块 %q 不存在", in.Section)
			}
			upd["section"] = in.Section
		}
		if n.Kind == model.DocNodeGuide {
			upd["blocks"] = blocksJSON(in.Blocks)
		}
		// 乐观锁必须是**一条 SQL 的条件**：读出来比较再无条件更新，
		// 两个请求同时通过比较、后写的静默覆盖前一个（409 永远不会出现）。
		res := tx.Model(&model.DocNode{}).Where("id = ? AND version = ?", n.ID, in.Version).Updates(upd)
		if res.Error != nil {
			return mapWriteError(res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrDocVersion
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return LoadAdminNode(key)
}

// DeleteNode 删除节点及其全部接口与子行（不可恢复）。
func DeleteNode(key string) error {
	return db.DB.Transaction(func(tx *gorm.DB) error {
		var n model.DocNode
		if err := tx.Where("key = ?", key).Take(&n).Error; err != nil {
			return ErrDocNotFound
		}
		var eps []model.DocEndpoint
		if err := tx.Where("node_id = ?", n.ID).Find(&eps).Error; err != nil {
			return err
		}
		for _, e := range eps {
			if err := deleteEndpointRows(tx, e.ID); err != nil {
				return err
			}
		}
		return tx.Delete(&n).Error
	})
}

// PublishNode 发布节点（体检不过则拒绝）。
//
// 语义边界：
//   - 节点发布**不级联**接口：接口仍逐篇发布（误发布面最小，改一篇也只影响一篇）；
//   - 因此 E_NODE_NO_ENDPOINT 要求「至少有一篇已发布接口」；
//   - 反方向（接口发布 → 节点自动跟上）由 PublishEndpoint 负责，不存在"发了一半"。
func PublishNode(key string, adminID uint64) (*AdminNode, error) {
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var n model.DocNode
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("key = ?", key).Take(&n).Error; err != nil {
			return ErrDocNotFound
		}
		nb, err := loadNodeByID(tx, n.ID)
		if err != nil {
			return err
		}
		if probs := CheckNode(nb); len(BlockingProblems(probs)) > 0 {
			return &BlockedError{Problems: probs}
		}
		now := time.Now()
		res := tx.Model(&model.DocNode{}).Where("id = ? AND version = ?", n.ID, n.Version).
			Updates(map[string]any{
				"status": model.DocPublished, "published_at": now,
				"contract_ver": apidoc.ContractVersion, "version": n.Version + 1,
				"updated_by": adminID, "updated_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrDocVersion
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return LoadAdminNode(key)
}

// UnpublishNode 下架节点（内容不动，只改状态）。
func UnpublishNode(key string, adminID uint64) (*AdminNode, error) {
	if err := db.DB.Model(&model.DocNode{}).Where("key = ?", key).Updates(map[string]any{
		"status": model.DocDraft, "published_at": nil,
		"updated_by": adminID, "updated_at": time.Now(),
	}).Error; err != nil {
		return nil, err
	}
	b, err := LoadAdminNode(key)
	if err != nil {
		return nil, ErrDocNotFound
	}
	return b, nil
}

// ── 接口 ──

// CreateEndpoint 在节点下新建一篇接口文档（草稿）。
func CreateEndpoint(nodeKey string, in EndpointCreateInput, adminID uint64) (*AdminEndpoint, error) {
	key := strings.TrimSpace(in.Key)
	if err := validKey(key, 64, "接口标识"); err != nil {
		return nil, err
	}
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var n model.DocNode
		if err := tx.Where("key = ?", nodeKey).Take(&n).Error; err != nil {
			return ErrDocNotFound
		}
		ep := model.DocEndpoint{
			NodeID: n.ID, Key: key, Status: model.DocDraft,
			Origin: model.DocOriginManual, UpdatedBy: adminID, Version: 1,
			Sort: nextSort(tx, "doc_endpoints", "node_id = ?", n.ID),
		}
		if err := tx.Create(&ep).Error; err != nil {
			return mapWriteError(err)
		}
		// 新建时「排在最后」是默认值，但 `EndpointInput.Sort` 的零值也是 0：
		// 直接把 in 交给 applyEndpointInput 会让新建的接口永远排在最前（曾被覆盖过）。
		body := in.EndpointInput
		if body.Sort == 0 {
			body.Sort = ep.Sort
		}
		return applyEndpointInput(tx, &ep, body, model.DocOriginManual, adminID)
	})
	if err != nil {
		return nil, err
	}
	return LoadAdminEndpoint(nodeKey, key)
}

// SaveEndpoint 保存整篇（内容全量替换，退回草稿）。
func SaveEndpoint(nodeKey, epKey string, in EndpointInput, origin string, adminID uint64) (*AdminEndpoint, error) {
	if in.Version <= 0 {
		return nil, inputErrf("保存必须带上当前版本号：先刷新页面再改，避免覆盖别人的改动")
	}
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var ep model.DocEndpoint
		if _, err := takeNodeEndpoint(tx, nodeKey, epKey, &ep); err != nil {
			return err
		}
		if in.Version != ep.Version {
			return ErrDocVersion
		}
		return applyEndpointInput(tx, &ep, in, origin, adminID)
	})
	if err != nil {
		return nil, err
	}
	return LoadAdminEndpoint(nodeKey, epKey)
}

// WriteFields 只替换请求字段（AI 局部写用：不碰示例/响应/变体）。
//
// origin 由调用方给（ai/manual）：审计与种子刷新都靠这一列分辨「谁写的」，
// 在这里写死会让 AI 的改动看起来像人工的，反过来也一样。
//
// 只删 `variant_id IS NULL` 的行：返回字段挂在响应变体上，由整篇保存
// （PUT /endpoints/:ep）负责重建。旧实现按 endpoint_id 全删，症状是
// 「AI 补了几个字段，页面上所有返回字段全空了」——而体检不会报。
func WriteFields(nodeKey, epKey string, fields []FieldView, version int, origin string, adminID uint64) (*AdminEndpoint, error) {
	if version <= 0 {
		return nil, inputErrf("保存必须带上当前版本号：先读取现值再改")
	}
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var ep model.DocEndpoint
		if _, err := takeNodeEndpoint(tx, nodeKey, epKey, &ep); err != nil {
			return err
		}
		if err := clearRequestFields(tx, ep.ID); err != nil {
			return err
		}
		for i, f := range fields {
			if err := insertField(tx, ep.ID, nil, f, i); err != nil {
				return err
			}
		}
		res := tx.Model(&model.DocEndpoint{}).Where("id = ? AND version = ?", ep.ID, version).
			Updates(map[string]any{
				"status": model.DocDraft, "published_at": nil,
				"version": version + 1, "updated_by": adminID,
				"origin": origin, "updated_at": time.Now(),
			})
		if res.Error != nil {
			return mapWriteError(res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrDocVersion
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return LoadAdminEndpoint(nodeKey, epKey)
}

// PublishEndpoint 发布一篇（体检不过则拒绝，未绑定一律拒绝）。
//
// **一个动作上线一整页**：接口置发布态之后，若所在节点仍是草稿就一并发布。
// 以前是两层各自发布，运营点完「发布」页面还是不出现（节点草稿），而界面上
// 没有任何东西提示他还要去点第二次 —— 「我明明发布了」就是这么来的。
//
// 节点侧沿用同一个 CheckNode（不写第二套规则）：此刻接口已在本事务里可见，
// 所以「至少一篇已发布接口」这条判据自然成立。
func PublishEndpoint(nodeKey, epKey string, adminID uint64) (*AdminEndpoint, *AdminNode, error) {
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var ep model.DocEndpoint
		if _, err := takeNodeEndpoint(tx, nodeKey, epKey, &ep); err != nil {
			return err
		}
		var n model.DocNode
		if err := tx.Where("id = ?", ep.NodeID).Take(&n).Error; err != nil {
			return ErrDocNotFound
		}
		b, err := loadBundleTx(tx, n, ep)
		if err != nil {
			return err
		}
		if probs := BlockingProblems(CheckEndpoint(b)); len(probs) > 0 {
			return &BlockedError{Problems: probs}
		}
		now := time.Now()
		if err := tx.Model(&model.DocEndpoint{}).Where("id = ?", ep.ID).Updates(map[string]any{
			"status": model.DocPublished, "published_at": now,
			"contract_ver": apidoc.ContractVersion, "updated_by": adminID,
			"updated_at": now,
		}).Error; err != nil {
			return err
		}
		// 节点仍草稿 → 一并发布（同一事务：要么都上线，要么都不动）。
		if n.Status != model.DocPublished {
			nb, err := loadNodeByID(tx, n.ID)
			if err != nil {
				return err
			}
			if probs := BlockingProblems(CheckNode(nb)); len(probs) > 0 {
				return &BlockedError{Problems: probs}
			}
			if err := tx.Model(&model.DocNode{}).Where("id = ?", n.ID).Updates(map[string]any{
				"status": model.DocPublished, "published_at": now,
				"contract_ver": apidoc.ContractVersion, "version": n.Version + 1,
				"updated_by": adminID, "updated_at": now,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return loadAdminPair(nodeKey, epKey)
}

// UnpublishEndpoint 撤回到草稿；节点没有已发布接口时一并转草稿。
//
// 否则会留下「节点已发布、页面空白」的悬挂态：侧栏有一个点不进去的分组。
func UnpublishEndpoint(nodeKey, epKey string, adminID uint64) (*AdminEndpoint, *AdminNode, error) {
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var ep model.DocEndpoint
		if _, err := takeNodeEndpoint(tx, nodeKey, epKey, &ep); err != nil {
			return err
		}
		if err := draftEndpoint(tx, ep.ID, adminID); err != nil {
			return err
		}
		return draftNodeIfEmpty(tx, ep.NodeID, adminID)
	})
	if err != nil {
		return nil, nil, err
	}
	return loadAdminPair(nodeKey, epKey)
}

// DeleteEndpoint 删除一篇接口文档（含子行）；删掉最后一篇已发布接口时节点转草稿。
func DeleteEndpoint(nodeKey, epKey string) error {
	return db.DB.Transaction(func(tx *gorm.DB) error {
		var ep model.DocEndpoint
		if _, err := takeNodeEndpoint(tx, nodeKey, epKey, &ep); err != nil {
			return err
		}
		if err := deleteEndpointRows(tx, ep.ID); err != nil {
			return err
		}
		return draftNodeIfEmpty(tx, ep.NodeID, 0)
	})
}

// draftEndpoint 把接口转草稿（内容不动）。
func draftEndpoint(tx *gorm.DB, id uint64, adminID uint64) error {
	return tx.Model(&model.DocEndpoint{}).Where("id = ?", id).Updates(map[string]any{
		"status": model.DocDraft, "published_at": nil,
		"updated_by": adminID, "updated_at": time.Now(),
	}).Error
}

// draftNodeIfEmpty 在节点下**没有已发布接口**时把节点转草稿。
//
// 判据只有一处（visibleEndpoint 的组合条件），不另写一套"有没有内容"的判断。
func draftNodeIfEmpty(tx *gorm.DB, nodeID uint64, adminID uint64) error {
	var n model.DocNode
	if err := tx.Where("id = ?", nodeID).Take(&n).Error; err != nil {
		return nil // 节点已被删：没有需要收尾的状态
	}
	if n.Status != model.DocPublished {
		return nil
	}
	var eps []model.DocEndpoint
	if err := tx.Where("node_id = ?", nodeID).Find(&eps).Error; err != nil {
		return err
	}
	for _, e := range eps {
		if visibleEndpoint(n, e) {
			return nil
		}
	}
	return tx.Model(&model.DocNode{}).Where("id = ?", nodeID).Updates(map[string]any{
		"status": model.DocDraft, "published_at": nil,
		"version": n.Version + 1, "updated_by": adminID, "updated_at": time.Now(),
	}).Error
}

// loadAdminPair 把「接口 + 它的节点」一起读回：发布/下架会同时改两层状态，
// 前端只重置其中一个 base，另一个就会立刻显示"未保存"。
func loadAdminPair(nodeKey, epKey string) (*AdminEndpoint, *AdminNode, error) {
	ep, err := LoadAdminEndpoint(nodeKey, epKey)
	if err != nil {
		return nil, nil, err
	}
	n, err := LoadAdminNode(nodeKey)
	if err != nil {
		return nil, nil, err
	}
	return ep, n, nil
}

// ── 内部实现 ──

// applyEndpointInput 把输入写进主行并全量替换子行。
//
// 创建工作也走这里，所以新建出来的 version 是 2 —— 版本只是**乐观锁令牌**，
// 不是「保存过几次」。想让它从 1 开始就得给创建路径单开一支，那是不必要的分支。
//
// 规范化（方法/路径/phase/badge/变体兜底/状态保持）全部在 draftOf 里：
// 预览走同一条链，所以「预览看到什么、保存后就是什么」。
func applyEndpointInput(tx *gorm.DB, ep *model.DocEndpoint, in EndpointInput, origin string, adminID uint64) error {
	d, err := draftOf(*ep, in)
	if err != nil {
		return err
	}
	row := d.EP
	upd := map[string]any{
		"name": row.Name, "summary": row.Summary,
		"method": row.Method, "path": row.Path,
		"aliases": row.Aliases, "transport_doc_id": row.TransportDocID,
		"phase": row.Phase, "badge": row.Badge,
		"run_mode": row.RunMode, "run_hint": row.RunHint, "max_body": row.MaxBody,
		"notes":   row.Notes,
		"samples": row.Samples, "samples2": row.Samples2,
		"example_title": row.ExampleTitle, "example_title2": row.ExampleTitle2,
		"hidden": row.Hidden, "sort": row.Sort, "status": row.Status,
		"published_at": row.PublishedAt,
		"method_check": row.MethodCheck, "path_check": row.PathCheck,
		"version": ep.Version + 1, "updated_by": adminID,
		"origin": origin, "updated_at": time.Now(),
	}
	res := tx.Model(&model.DocEndpoint{}).Where("id = ? AND version = ?", ep.ID, ep.Version).Updates(upd)
	if res.Error != nil {
		return mapWriteError(res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrDocVersion
	}
	*ep = row
	ep.Version++
	if err := writeDraft(tx, ep.ID, d, origin); err != nil {
		return err
	}
	// 发布态写完就地体检：拦下来等于回滚到保存前，读者永远看不到半成品。
	if ep.Status == model.DocPublished {
		b, err := loadEndpointByID(tx, ep.ID)
		if err != nil {
			return err
		}
		if probs := BlockingProblems(CheckEndpoint(b)); len(probs) > 0 {
			return &BlockedError{Problems: probs}
		}
	}
	return nil
}

// writeDraft 全量替换子行（顺序：先清，再建变体，再建请求字段）。
//
// 顺序有意义：返回字段带 variant_id，必须等变体拿到主键才能插。
func writeDraft(tx *gorm.DB, epID uint64, d *endpointDraft, origin string) error {
	if err := clearChildren(tx, epID); err != nil {
		return err
	}
	for vi := range d.Variants {
		v := &d.Variants[vi]
		v.Row.ID = 0
		v.Row.EndpointID = epID
		if err := tx.Create(&v.Row).Error; err != nil {
			return mapWriteError(err)
		}
		for fi := range v.Fields {
			if err := insertFieldRow(tx, epID, &v.Row.ID, v.Fields[fi], v.Enums[fi]); err != nil {
				return err
			}
		}
	}
	for fi := range d.Fields {
		if err := insertFieldRow(tx, epID, nil, d.Fields[fi], d.Enums[fi]); err != nil {
			return err
		}
	}
	for i := range d.Presets {
		pr := d.Presets[i]
		pr.ID, pr.EndpointID = 0, epID
		if err := tx.Create(&pr).Error; err != nil {
			return mapWriteError(err)
		}
	}
	for i := range d.Errors {
		er := d.Errors[i]
		er.ID, er.EndpointID = 0, epID
		if er.Source == "" {
			er.Source = origin
		}
		if err := tx.Create(&er).Error; err != nil {
			return mapWriteError(err)
		}
	}
	return nil
}

// clearChildren 删除一篇文档的全部子行（枚举要先删：它挂在字段主键上，没有级联）。
func clearChildren(tx *gorm.DB, epID uint64) error {
	if err := deleteFields(tx, epID, false); err != nil {
		return err
	}
	if err := tx.Where("endpoint_id = ?", epID).Delete(&model.DocResponseVariant{}).Error; err != nil {
		return err
	}
	if err := tx.Where("endpoint_id = ?", epID).Delete(&model.DocPreset{}).Error; err != nil {
		return err
	}
	return tx.Where("endpoint_id = ?", epID).Delete(&model.DocEndpointError{}).Error
}

// clearRequestFields 只清请求字段（`variant_id IS NULL`），返回字段一概不动。
//
// 返回字段挂在响应变体上，归整篇保存（replaceEndpointContent）管；
// 两条路径的边界就在这里 —— 越过它，AI 补几个请求字段就会把全部返回字段删掉。
func clearRequestFields(tx *gorm.DB, epID uint64) error {
	return deleteFields(tx, epID, true)
}

// deleteFields 删字段行（requestOnly 时只删请求字段），枚举先行。
func deleteFields(tx *gorm.DB, epID uint64, requestOnly bool) error {
	q := tx.Model(&model.DocEndpointField{}).Where("endpoint_id = ?", epID)
	if requestOnly {
		q = q.Where("variant_id IS NULL")
	}
	var ids []uint64
	if err := q.Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) > 0 {
		if err := tx.Where("field_id IN ?", ids).Delete(&model.DocFieldEnum{}).Error; err != nil {
			return err
		}
	}
	return q.Delete(&model.DocEndpointField{}).Error
}

// deleteEndpointRows 删除一篇接口文档的全部行。
func deleteEndpointRows(tx *gorm.DB, epID uint64) error {
	if err := clearChildren(tx, epID); err != nil {
		return err
	}
	return tx.Where("id = ?", epID).Delete(&model.DocEndpoint{}).Error
}

func insertField(tx *gorm.DB, epID uint64, variantID *uint64, f FieldView, sort int) error {
	return insertFieldRow(tx, epID, variantID, fieldRowOf(f, sort), enumRowsOf(f))
}

// insertFieldRow 插入一个字段行及其枚举。
//
// 枚举重复在**落库前**拒：唯一索引也会拒，但报出来的是"与已有数据重复"，
// 而用户需要知道的是"字段 x 的枚举值 y 写了两遍"。
func insertFieldRow(tx *gorm.DB, epID uint64, variantID *uint64, row model.DocEndpointField, enums []model.DocFieldEnum) error {
	row.ID = 0
	row.EndpointID, row.VariantID = epID, variantID
	if err := tx.Create(&row).Error; err != nil {
		return mapWriteError(err)
	}
	seen := make(map[string]bool, len(enums))
	for i := range enums {
		e := enums[i]
		v := strings.TrimSpace(e.Value)
		if v == "" {
			continue
		}
		if seen[v] {
			return inputErrf("字段 %s 的枚举值 %q 重复", row.Name, v)
		}
		seen[v] = true
		e.FieldID, e.Value = row.ID, v
		if err := tx.Create(&e).Error; err != nil {
			return mapWriteError(err)
		}
	}
	return nil
}

// enumSourceFor 补齐枚举来源（与播种的 enumSourceOf 同语义）。
//
// 不补会存空串：页面靠这一列决定「可选值」是静态列表、模型清单还是档位表，
// 空串会让「取值随模型变化」的字段看起来像没有取值范围。
func enumSourceFor(f FieldView) string {
	if v := strings.TrimSpace(f.ValueSource); v != "" {
		return v
	}
	if len(f.Enum) > 0 {
		return "static"
	}
	return "none"
}

// takeNodeEndpoint 取节点 + 接口（两层都不存在时给出可分辨的错误）。
func takeNodeEndpoint(tx *gorm.DB, nodeKey, epKey string, ep *model.DocEndpoint) (model.DocNode, error) {
	var n model.DocNode
	if err := tx.Where("key = ?", nodeKey).Take(&n).Error; err != nil {
		return n, ErrDocNotFound
	}
	if err := tx.Where("node_id = ? AND key = ?", n.ID, epKey).Take(ep).Error; err != nil {
		return n, ErrDocEndpointNotFound
	}
	return n, nil
}

// nextSort 取同组最大 sort + 10（新建的排最后）。
func nextSort(tx *gorm.DB, table string, where string, args ...any) int {
	var max int
	_ = tx.Table(table).Where(where, args...).Select("COALESCE(MAX(sort), 0)").Scan(&max).Error
	return max + 10
}

// ── 小工具 ──

func validKey(key string, max int, label string) error {
	if key == "" {
		return inputErrf("%s不能为空", label)
	}
	if utf8.RuneCountInString(key) > max {
		return inputErrf("%s最多 %d 个字符", label, max)
	}
	for i, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_' || r == '.') && i > 0:
		default:
			return inputErrf("%s只能用小写字母、数字和 - _ .（且不能以符号开头）", label)
		}
	}
	return nil
}

func limit(s string, max int, label string) error {
	if utf8.RuneCountInString(s) > max {
		return inputErrf("%s最多 %d 个字，当前 %d 个", label, max, utf8.RuneCountInString(s))
	}
	return nil
}

func def(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}

func blocksJSON(bs []GuideBlockView) model.JSON {
	if len(bs) == 0 {
		return model.MustJSON([]any{})
	}
	return model.MustJSON(bs)
}

// constraintsOf 把编辑态的约束列表落库（形状与迁移/播种完全一致）。
func constraintsOf(cs []ConstraintView) model.JSON {
	if len(cs) == 0 {
		return model.MustJSON([]any{})
	}
	return model.MustJSON(cs)
}

// kvItems 把库里的有序 KV 读成编辑态（保留原顺序）。
func kvItems(j model.JSON) []KVItem {
	out := make([]KVItem, 0, 8)
	if len(j) == 0 {
		return out
	}
	var raw []struct {
		K string          `json:"k"`
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(j, &raw); err != nil {
		return out
	}
	for _, it := range raw {
		var v any
		dec := json.NewDecoder(bytes.NewReader(it.V))
		// UseNumber：`max_n: 4` 不能被读成 `4.0`，示例里的数字要跟用户写的一模一样。
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			v = nil
		}
		out = append(out, KVItem{K: it.K, V: v})
	}
	return out
}

// kvsJSON 是 kvItems 的逆运算。
func kvsJSON(items []KVItem) model.JSON {
	if len(items) == 0 {
		return model.MustJSON([]any{})
	}
	return model.MustJSON(items)
}

// ParseKVJSON 把「示例体」解析成保序 KV 数组。
//
// 接受两种写法：
//   - `{"model":"x","prompt":"猫"}`（人/AI 手写，最自然）
//   - `[{"k":"model","v":"x"}]`（库里存的形式，管理台回传的就是它）
//
// 为什么必须保序解析：对象走 `map[string]any` 会把键序打散，示例存一次就可能
// 从 `{model,prompt}` 变成 `{prompt,model}` —— 读者会以为内容被改过。
func ParseKVJSON(raw json.RawMessage) ([]KVItem, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return []KVItem{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, inputErrf("示例体不是合法 JSON: %v", err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil, inputErrf("示例体必须是 JSON 对象或 [{k,v}] 数组")
	}
	out := make([]KVItem, 0, 8)
	switch d {
	case '[':
		// 已是 KV 数组：逐项读 {k,v}。
		for dec.More() {
			var it KVItem
			if err := dec.Decode(&it); err != nil {
				return nil, inputErrf("示例体的数组项必须是 {k,v}: %v", err)
			}
			out = append(out, it)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
	case '{':
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := kt.(string)
			var v any
			if err := dec.Decode(&v); err != nil {
				return nil, err
			}
			out = append(out, KVItem{K: key, V: v})
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
	default:
		return nil, inputErrf("示例体必须是 JSON 对象或 [{k,v}] 数组")
	}
	return out, nil
}

// ── 数据库错误的人话翻译 ──

// mapWriteError 把唯一约束冲突翻成「用户能照着改」的错误。
//
// 为什么不直接回 500：8 个唯一索引对应 8 句不同的人话（标识重名 / 协议重复 /
// 同位置字段重名 / 同状态码变体重复…）。统一回「服务器开小差」的结果是用户
// 反复重试同一个操作，而真正该做的是改一个名字。
//
// 约束名唯一来源是 db/indexes.go；这里只做映射，不重复定义判据。
func mapWriteError(err error) error {
	if err == nil {
		return nil
	}
	name, ok := pgerr.Unique(err)
	if !ok {
		return err
	}
	switch name {
	case "uq_doc_nodes_key", "uq_doc_endpoints_key", "uq_doc_endpoints_key_v2":
		return ErrDocKeyTaken
	case "uq_doc_nodes_protocol":
		return inputErrf("这个协议已经有一篇文档了（协议篇与协议一一对应）")
	case "uq_doc_endpoints_transport":
		return inputErrf("这个契约端点已被同一节点下的另一篇文档占用")
	case "uq_doc_node_models_family":
		return inputErrf("有模型已经属于别的模型族（一个模型只能出现在一篇族文档里）")
	case "uq_doc_fields_req":
		return inputErrf("同一位置下有重名字段")
	case "uq_doc_fields_resp":
		return inputErrf("同一条响应里有重名返回字段")
	case "uq_doc_response_variants":
		return inputErrf("同状态码 + 同标签的响应重复")
	case "uq_doc_presets_key":
		return inputErrf("调用方式的标识重复")
	case "uq_doc_endpoint_errors":
		return inputErrf("同状态码 + 同错误码重复")
	}
	return inputErrf("与已有数据重复，请检查标识是否被占用")
}

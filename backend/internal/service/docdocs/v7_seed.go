package docdocs

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// 出厂文档写入（系统初始化时唯一的一次写入）。
//
// 三条边界：
//  1. 节点清单来自 `apidoc.SeedDocs()`（协议 + 通用 + 模型族），与模型表无关；
//  2. 接口按 `transport_doc_id` 判存在 —— key 是展示名，可被人工改名；
//  3. **已存在的一律跳过**：不再有"契约版本变了就整篇重写"、"给新列补空值"、
//     "按白名单删掉不在契约里的接口"。这三件事都是代码在运营改完之后继续写库，
//     表现就是「后台改完、重启又变回去」。
//
// 新插入的接口立刻体检：通过即发布，不通过留草稿并打 ERROR。
// 出厂内容本该开箱可用；让运营先逐个点发布才能看到文档，等于把默认值定成"不可用"。
//
// **不看 `models` 表**：文档是内置契约，不该因为此刻没有在售模型就整块消失，
// 也不该因为某个模型上架/下架而多出或少了几个字段。
//
// 曾经这里还有一个「规格扩展字段」的合并（`withSpecFields`）：把数据驱动接入
// 在 `adapter_specs` 里声明的 request_fields 并进族文档，并按模型 ID 打 `AppliesTo`。
// 它让文档内容取决于**货架**（哪些模型此刻绑到了哪些规格），与"文档描述协议允许什么"
// 直接冲突；而且合并出来的字段带着"仅某个模型"的收窄标记，正是三族同文的另一半成因。
// 现在删掉了：规格作者要补文档，请在管理台把字段写进那一篇。

// SeedReport 是播种结果（启动日志与管理台用）。
type SeedReport struct {
	// Sections 是这次新建的板块数（已存在的不计）。
	Sections  int
	Nodes     int
	Endpoints int
	Fields    int
	Skipped   int
	// Refreshed 是这次**更新了正文**的说明页数（origin=seed 且内容与基线不同）。
	// 接口页一律不在这里：它们的正文是运营资产，只在专门的 M 迁移里重播。
	Refreshed int
	// Damaged 是「已落库但体检不过、只能留草稿」的条目（`node` 或 `node/ep`）。
	// 它不是错误：数据在库里、管理台能编辑；但要有人知道它现在对读者不可见。
	Damaged []string
}

// SeedDocs 写入**出厂文档**（系统自带的初始 API 文档）。
//
// 一条规则：**只在不存在时插入，之后一个字节都不碰**。
//
// 这套文档是系统的初始内容，不是"代码生成的渲染结果"。首次部署时把契约里
// 声明的节点/接口/字段写进库，从此它们与后台里手写的任何一篇文档没有区别：
// 运营改了就是改了，重启不会变回去，契约也不会把文案改回来。
//
// 因此这里没有"刷新"、"回填"、"下架"、"删除"：
//
//	曾经的做法是每次启动按契约整篇重写种子行，再给新列补一次空值，
//	再按白名单删掉不在契约里的接口。三者都是**同一个错误**：让代码在
//	运营改完之后继续持有写权。表现是「后台改完、重启又变回去」，
//	而日志里没有任何异常 —— 这类 bug 只能靠删掉写权来根治。
func SeedDocs() (SeedReport, error) {
	var rep SeedReport
	// 板块必须最先就位：节点的 `section` 指向它，缺一行节点就没有归属。
	secCount, err := SeedSections()
	if err != nil {
		return rep, err
	}
	rep.Sections = secCount

	nodes := apidoc.SeedDocs()
	for _, n := range nodes {
		spec := seedSpec{
			Node: model.DocNode{
				Key: n.Key, Kind: n.Kind, Section: n.Section, Protocol: n.Protocol,
				Title: n.Title, Summary: n.Summary, Sort: n.Sort,
				Origin: model.DocOriginSeed, Blocks: model.MustJSON([]any{}),
			},
			Endpoints: n.Endpoints,
		}
		if err := seedOneNode(spec, &rep); err != nil {
			return rep, err
		}
	}

	for _, f := range apidoc.FamilyDocs() {
		spec := seedSpec{
			Node: model.DocNode{
				Key: f.Key, Kind: model.DocNodeFamily, Section: f.Section,
				Title: f.Title, Summary: f.Summary, Sort: f.Sort,
				Origin: model.DocOriginSeed, Blocks: model.MustJSON([]any{}),
			},
			Endpoints: apidoc.SeedFamilyEndpoints(f),
		}
		if err := seedOneNode(spec, &rep); err != nil {
			return rep, err
		}
	}

	// 说明页：正文块来自出厂基线（与旧 SeedPages 同一份内容）。
	for _, p := range baselinePages() {
		spec := seedSpec{Node: model.DocNode{
			Key: p.Slug, Kind: model.DocNodeGuide, Section: apidoc.SectionStart,
			Title: p.Title, Sort: p.Sort, Origin: model.DocOriginSeed,
			Blocks: model.MustJSON(nonNilBlocks(p.Blocks)),
		}}
		if err := seedOneNode(spec, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// seedSpec 是一个节点的「应有状态」。
type seedSpec struct {
	Node      model.DocNode
	Endpoints []apidoc.SeedEndpoint
}

// seedOneNode 在一个事务里补齐一个节点缺的出厂内容。
//
// 单节点一个事务：某个节点写坏只回滚它自己，不影响其余节点上线。
//
// 只做三件事：节点不存在就建、接口不存在就建、建出来的节点定一次状态。
// **已存在的行一律跳过** —— 那是运营的内容。
func seedOneNode(spec seedSpec, rep *SeedReport) error {
	return db.DB.Transaction(func(tx *gorm.DB) error {
		node, created, err := ensureNode(tx, spec.Node, rep)
		if err != nil {
			return err
		}
		have, err := seededEndpoints(tx, node.ID)
		if err != nil {
			return err
		}
		for i, s := range spec.Endpoints {
			if _, ok := have[s.DocID]; ok {
				rep.Skipped++
				continue
			}
			rec, err := insertEndpoint(tx, node, s, i*10, rep)
			if err != nil {
				return err
			}
			if err := settleSeededEndpoint(tx, node, rec, rep); err != nil {
				return err
			}
		}
		if !created {
			// 节点早就有了：状态、标题归运营，启动不碰。
			//
			// 例外只有说明页的**正文**（见 syncGuideBlocks）：那是出厂基线，
			// `origin='seed'` 表示没有任何人编辑过它。若代码里改了这段文字却
			// 不同步，"快速开始"这类页会永远停在首版 —— 新加的通用约定
			// （例如 Idempotency-Key）在文档站上根本不存在，而功能是真的。
			if err := syncGuideBlocks(tx, node, spec.Node, rep); err != nil {
				return err
			}
			return nil
		}
		return settleSeededNode(tx, node, rep)
	})
}

// syncGuideBlocks 把出厂说明页的正文同步到库里（**只对 origin=seed 的页面**）。
//
// 为什么需要这条例外：说明页正文完全由代码里的基线决定，而播种规则是
// "已存在就跳过"。于是只要改一句文案，线上页面就永远停在首版 ——
// 这一轮就踩到了：`Idempotency-Key` 已经实现并上线，但文档站上没有任何一处
// 提到它，读者不可能知道有这个东西。
//
// 边界必须卡死，否则会覆盖运营的内容：
//   - 只认 `kind=guide`（接口页是运营资产，改正文走专门的 M 迁移）；
//   - 只认 `origin=seed`（管理台保存会把 origin 改成 manual/ai，改过就永远不碰）；
//   - 比较用**结构化深比较**而不是字符串：`blocks` 是 jsonb，落库后键序与空白
//     都会被规范化，按字符串比会每次启动都判定"变了"并白刷一次版本号。
func syncGuideBlocks(tx *gorm.DB, row model.DocNode, want model.DocNode, rep *SeedReport) error {
	if row.Kind != model.DocNodeGuide || want.Kind != model.DocNodeGuide {
		return nil
	}
	if row.Origin != model.DocOriginSeed || want.Origin != model.DocOriginSeed {
		return nil
	}
	var have, target []apidoc.GuideBlock
	// 解析不了说明库里是别的形状（历史数据），不碰：宁可少同步一次，也不覆盖。
	if err := json.Unmarshal(row.Blocks, &have); err != nil {
		return nil
	}
	if err := json.Unmarshal(want.Blocks, &target); err != nil {
		return nil
	}
	if len(target) == 0 || reflect.DeepEqual(have, target) {
		return nil
	}
	// 已发布仍是发布态：文案同步不是下架，读者不该因为一次措辞更新看不到这篇。
	if err := tx.Model(&model.DocNode{}).Where("id = ?", row.ID).Updates(map[string]any{
		"blocks": want.Blocks, "version": row.Version + 1, "updated_at": time.Now(),
	}).Error; err != nil {
		return fmt.Errorf("同步说明页正文 %s: %w", row.Key, err)
	}
	rep.Refreshed++
	return nil
}

// ensureNode 取或建一个节点（不覆盖任何已存在的字段）。
func ensureNode(tx *gorm.DB, n model.DocNode, rep *SeedReport) (model.DocNode, bool, error) {
	var row model.DocNode
	err := tx.Where("key = ?", n.Key).Take(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		now := time.Now()
		// 先落草稿、体检通过再改已发布：即使体检 panic，库里也不会留下
		// 一行「published 但内容残缺」的节点。
		n.CreatedAt, n.UpdatedAt = now, now
		n.Version, n.Status, n.PublishedAt = 1, model.DocDraft, nil
		if n.Blocks == nil {
			n.Blocks = model.MustJSON([]any{})
		}
		if err := tx.Create(&n).Error; err != nil {
			return row, false, err
		}
		rep.Nodes++
		return n, true, nil
	case err != nil:
		return row, false, err
	default:
		rep.Skipped++
		return row, false, nil
	}
}

// seededEndpoints 取节点下已存在的接口（按契约身份索引）。
func seededEndpoints(tx *gorm.DB, nodeID uint64) (map[string]model.DocEndpoint, error) {
	var rows []model.DocEndpoint
	if err := tx.Where("node_id = ? AND transport_doc_id IS NOT NULL", nodeID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]model.DocEndpoint, len(rows))
	for _, r := range rows {
		out[*r.TransportDocID] = r
	}
	return out, nil
}

// insertEndpoint 写一个接口与它的全部子行（状态先草稿，由 settleSeededEndpoint 定稿）。
func insertEndpoint(tx *gorm.DB, node model.DocNode, s apidoc.SeedEndpoint, sort int, rep *SeedReport) (*model.DocEndpoint, error) {
	docID := s.DocID
	tr, _ := apidoc.TransportOfDocID(docID)
	rec := model.DocEndpoint{
		NodeID: node.ID, Key: s.Key, Name: s.Doc.Name, Summary: s.Doc.Summary,
		Method: tr.Method, Path: tr.Path, Aliases: strJSON(tr.Aliases),
		TransportDocID: &docID,
		MethodCheck:    tr.Method, PathCheck: tr.Path,
		Phase: phaseOf(tr), RunMode: string(s.Doc.RunMode), RunHint: s.Doc.RunHint,
		MaxBody: s.Doc.MaxBody, Badge: badgeOf(tr),
		// 出厂注意事项只在**首次插入**时写一次；此后这一列归后台所有，
		// 契约再怎么改也不会碰它（人工编辑的接口连刷新都不做）。
		Notes: strJSON(s.Doc.Notes),
		Sort:  sort, Status: model.DocDraft, Origin: model.DocOriginSeed, Version: 1,
	}
	if err := tx.Create(&rec).Error; err != nil {
		return nil, err
	}
	rep.Endpoints++
	if err := insertEndpointChildren(tx, node, rec.ID, s, rep); err != nil {
		return nil, err
	}
	// 示例代码**在这里生成一次，之后就是内容**。
	//
	// 生成输入是刚写进去的字段表与字段示例值（不是契约里的示例体）：
	// 这样「参数表里的示例 chip」与「curl 里的请求体」永远是同一个值。
	// 写进去的是 `{{base_url}}` 占位符，域名在投影期替换。
	b, err := loadBundleTx(tx, node, rec)
	if err != nil {
		return nil, err
	}
	// 主示例没有标题（页面上就写"请求示例"），第二段有场景名。
	upd := map[string]any{"example_title2": s.Doc.ExampleTitle2}
	if sv, ok := BuildSamplesOf(b); ok {
		upd["samples"] = samplesJSON(&sv)
	}
	if len(s.Doc.Example2) > 0 {
		if sv, ok := BuildSamplesWithBody(b, s.Doc.Example2); ok {
			upd["samples2"] = samplesJSON(&sv)
		}
	}
	if err := tx.Model(&model.DocEndpoint{}).Where("id = ?", rec.ID).Updates(upd).Error; err != nil {
		return nil, err
	}
	rec.ExampleTitle2 = s.Doc.ExampleTitle2
	if j, ok := upd["samples"].(model.JSON); ok {
		rec.Samples = j
	}
	if j, ok := upd["samples2"].(model.JSON); ok {
		rec.Samples2 = j
	}
	return &rec, nil
}

// settleSeededEndpoint 给新插入的接口定状态：体检通过即发布，否则留草稿。
//
// 不走 `PublishEndpoint`：那个函数会顺带把节点也发布掉（一个动作上线一整页），
// 而种子必须先让全部接口定稿、再统一判节点状态，否则节点在只插了第一篇接口时
// 就被发布，随后又被第二次判定的结果改回去 —— 两次写状态没有必要。
func settleSeededEndpoint(tx *gorm.DB, node model.DocNode, ep *model.DocEndpoint, rep *SeedReport) error {
	b, err := loadBundleTx(tx, node, *ep)
	if err != nil {
		return err
	}
	probs := BlockingProblems(CheckEndpoint(b))
	if len(probs) > 0 {
		rep.Damaged = append(rep.Damaged, node.Key+"/"+ep.Key)
		logger.L().Error("种子接口未通过体检，保持草稿（对外不可见）",
			zap.String("node", node.Key), zap.String("endpoint", ep.Key),
			zap.String("code", probs[0].Code), zap.String("msg", probs[0].Message))
		return nil
	}
	now := time.Now()
	if err := tx.Model(&model.DocEndpoint{}).Where("id = ?", ep.ID).Updates(map[string]any{
		"status": model.DocPublished, "published_at": now,
		"contract_ver": apidoc.ContractVersion, "updated_at": now,
	}).Error; err != nil {
		return err
	}
	ep.Status = model.DocPublished
	return nil
}

// settleSeededNode 给节点定状态（必须在接口定稿之后判，否则 E_NODE_NO_ENDPOINT 恒真）。
func settleSeededNode(tx *gorm.DB, node model.DocNode, rep *SeedReport) error {
	nb, err := loadNodeByID(tx, node.ID)
	if err != nil {
		return err
	}
	now := time.Now()
	upd := map[string]any{"updated_at": now}
	if probs := BlockingProblems(CheckNode(nb)); len(probs) > 0 {
		upd["status"], upd["published_at"] = model.DocDraft, nil
		rep.Damaged = append(rep.Damaged, node.Key)
		logger.L().Error("种子节点未通过体检，保持草稿（对外不可见）",
			zap.String("node", node.Key), zap.Int("blocking", len(probs)),
			zap.String("code", probs[0].Code), zap.String("msg", probs[0].Message))
	} else {
		upd["status"], upd["published_at"] = model.DocPublished, now
		upd["contract_ver"] = apidoc.ContractVersion
	}
	return tx.Model(&model.DocNode{}).Where("id = ?", node.ID).Updates(upd).Error
}

// insertEndpointChildren 写字段 / 变体 / 示例 / 预设 / 错误码。
func insertEndpointChildren(tx *gorm.DB, node model.DocNode, epID uint64, s apidoc.SeedEndpoint, rep *SeedReport) error {
	// 契约里的请求示例体按键注入到字段示例值：请求体 = 字段示例值，只有这一条来源。
	// 已写了示例值的字段不动（契约是种子，不是权威）。
	ex := make(map[string]any, len(s.Doc.Example))
	for _, kv := range s.Doc.Example {
		ex[kv.K] = kv.V
	}
	// 请求字段
	for i, f := range s.Doc.Fields {
		if f.Example == nil {
			if v, ok := ex[f.Name]; ok {
				f.Example = v
			}
		}
		if err := createField(tx, epID, nil, f, i, rep); err != nil {
			return err
		}
	}
	// 响应变体（含各自返回字段）。
	//
	// **没有变体但有 Returns 时补一个 200**：投影层本来就会合成一个空的成功变体
	// （`responseView`），如果这里不落库，页面与库里就是两回事 —— 症状是
	// 「接口页一个返回字段都没有」，而发布闸门还会报 E_NO_SUCCESS_VARIANT
	//（19 个协议/通用接口曾经就这样：保存一次就再也发布不出去）。
	variants := s.Doc.Response.Variants
	if len(variants) == 0 && len(s.Doc.Returns) > 0 {
		variants = []apidoc.ResponseVariant{{Status: 200, Label: "成功"}}
	}
	for vi, v := range variants {
		notes := v.Notes
		// 整篇响应的公共说明并进第一条变体：它是读者唯一能看见的地方
		// （页面上没有"响应级说明"这一块，丢在契约里等于没写）。
		if vi == 0 && len(s.Doc.Response.Notes) > 0 {
			notes = append(append([]string{}, s.Doc.Response.Notes...), notes...)
		}
		rec := model.DocResponseVariant{
			EndpointID: epID, StatusCode: v.Status, Label: v.Label,
			Kind: string(s.Doc.Response.Kind), ContentType: v.ContentType,
			Envelope: s.Doc.Response.Envelope, Example: objJSON(v.Example),
			Raw: v.Raw, Notes: strJSON(notes), Sort: vi * 10,
		}
		if rec.Kind == "" {
			rec.Kind = string(apidoc.ResponseJSON)
		}
		if err := tx.Create(&rec).Error; err != nil {
			return err
		}
		returns := v.Returns
		if len(returns) == 0 {
			returns = s.Doc.Returns
		}
		for i, f := range returns {
			if err := createField(tx, epID, &rec.ID, f, i, rep); err != nil {
				return err
			}
		}
	}
	// 预设（调用方式）：`$model` 在这里一次性换成 model 字段的示例值，
	// 之后库里就是字面量 —— 投影期不再做任何占位符解析。
	modelVal := modelExampleOf(s.Doc)
	for i, p := range s.Presets {
		rec := model.DocPreset{
			EndpointID: epID, Key: p.Key, Label: p.Label, Summary: p.Summary,
			Body:   objJSON(resolvePresetModel(p.Body, modelVal)),
			Fields: strJSON(p.Fields), Uploads: strJSON(p.Uploads),
			Note: p.Note, Sort: i * 10,
		}
		if err := tx.Create(&rec).Error; err != nil {
			return err
		}
	}

	// 每个预设的示例代码同样生成一次（用该预设自己的 body）。
	// 放在新建完预设之后单独走一遍：需要预设行上的 Body 与整篇 bundle。
	if len(s.Presets) > 0 {
		b, err := loadBundleTx(tx, node, model.DocEndpoint{ID: epID})
		if err != nil {
			return err
		}
		for pi := range b.Presets {
			pr := &b.Presets[pi]
			sv, ok := BuildPresetSamples(pr, b)
			if !ok {
				continue
			}
			if err := tx.Model(&model.DocPreset{}).Where("id = ?", pr.ID).
				Update("samples", samplesJSON(&sv)).Error; err != nil {
				return err
			}
		}
	}
	// 错误码
	for i, e := range s.Errors {
		if err := tx.Create(&model.DocEndpointError{
			EndpointID: epID, StatusCode: e.Status, Code: e.Code, Message: e.Desc,
			Source: model.DocOriginSeed, Sort: i * 10,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func createField(tx *gorm.DB, epID uint64, variantID *uint64, f apidoc.Field, sort int, rep *SeedReport) error {
	rec := model.DocEndpointField{
		EndpointID: epID, VariantID: variantID,
		In: string(f.In), Name: f.Name, Type: string(f.Type),
		Required: string(f.Required), RequiredNote: f.RequiredNote,
		DefaultValue: f.Default, Desc: f.Desc,
		EnumSource: enumSourceOf(f), Constraints: constraintsJSON(f.Constraints),
		// 出厂标注同样只写一次，之后归后台所有。
		OnlyModels: f.OnlyModels,
		Tier:       string(f.Tier), Passthrough: f.Passthrough, SketchSkip: f.SketchSkip,
		Example: anyJSON(f.Example), Sort: sort,
	}
	if err := tx.Create(&rec).Error; err != nil {
		return err
	}
	rep.Fields++
	for i, v := range f.Enum {
		if err := tx.Create(&model.DocFieldEnum{
			FieldID: rec.ID, Value: v, Sort: i * 10,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

// ── 小工具（转换只在这里，别到处手写） ──

func phaseOf(tr apidoc.Transport) string {
	switch tr.Phase {
	case apidoc.PhaseSubmit:
		return "submit"
	case apidoc.PhaseRead:
		return "read"
	}
	return "sync"
}

func badgeOf(tr apidoc.Transport) string {
	switch {
	case tr.AcceptsAsync:
		return "async"
	case tr.Stream:
		return "stream"
	case tr.Binary:
		return "binary"
	}
	return ""
}

func enumSourceOf(f apidoc.Field) string {
	if len(f.Enum) == 0 {
		return "none"
	}
	if f.EnumSource == "" {
		return "static"
	}
	return string(f.EnumSource)
}

// objJSON 把有序对象存成 `[{k,v}]` 数组。
//
// 为什么不直接存 JSON 对象：Postgres jsonb **不保序**，读回来键会按长度/字节重排，
// 示例里 `model` 排到中间、`prompt` 跑到最前 —— 看起来就不像人写的请求体。
func objJSON(o apidoc.Obj) model.JSON {
	if len(o) == 0 {
		return model.MustJSON([]any{})
	}
	type kv struct {
		K string `json:"k"`
		V any    `json:"v"`
	}
	out := make([]kv, 0, len(o))
	for _, it := range o {
		out = append(out, kv{K: it.K, V: it.V})
	}
	return model.MustJSON(out)
}

// jsonObj 是 objJSON 的逆运算。
func jsonObj(j model.JSON) apidoc.Obj {
	if len(j) == 0 {
		return nil
	}
	var raw []struct {
		K string          `json:"k"`
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(j, &raw); err != nil {
		return nil
	}
	out := make(apidoc.Obj, 0, len(raw))
	for _, it := range raw {
		var v any
		_ = json.Unmarshal(it.V, &v)
		out = append(out, apidoc.KV{K: it.K, V: v})
	}
	return out
}

func strJSON(items []string) model.JSON {
	if items == nil {
		items = []string{}
	}
	return model.MustJSON(items)
}

// cleanLines 去掉空行与首尾空白（注意事项 / 备注类多行文本的唯一清洗点）。
//
// 编辑框里敲回车会留下空行：在编辑态保留它是必要的（否则用户没法换行），
// 但入库前进必须清掉，否则页面会渲染出一排空 <li>。
func cleanLines(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func jsonStrings(j model.JSON) []string {
	out := make([]string, 0, 4)
	if len(j) == 0 {
		return out
	}
	_ = json.Unmarshal(j, &out)
	if out == nil {
		out = []string{}
	}
	return out
}

func anyJSON(v any) model.JSON {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return model.JSON(b)
}

func jsonAny(j model.JSON) any {
	if len(j) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		return nil
	}
	return v
}

func constraintsJSON(cs []apidoc.Constraint) model.JSON {
	if len(cs) == 0 {
		return model.MustJSON([]any{})
	}
	type c struct {
		Kind  string  `json:"kind"`
		Value float64 `json:"value"`
		Text  string  `json:"text"`
	}
	out := make([]c, 0, len(cs))
	for _, it := range cs {
		out = append(out, c{Kind: it.Kind, Value: it.Value, Text: it.Text})
	}
	return model.MustJSON(out)
}

func jsonConstraints(j model.JSON) []ConstraintView {
	out := make([]ConstraintView, 0, 4)
	if len(j) == 0 {
		return nil
	}
	_ = json.Unmarshal(j, &out)
	return out
}

// nonNilBlocks 保证落库的是 `[]` 而不是 NULL：`blocks` 列 NOT NULL，
// 而 nil 切片会让 GORM 写 NULL → 插入直接报错（症状是「保存一篇空页面 500」）。
func nonNilBlocks(blocks []apidoc.GuideBlock) []apidoc.GuideBlock {
	if blocks == nil {
		return []apidoc.GuideBlock{}
	}
	return blocks
}

// 协议节点的「适用模型」不再落表：它由 `apidoc.AppliesDoc` 在投影期现算
// （见 v7_project.go 的 nodeModelsOf）。曾经的 doc_node_models 表在族改静态后
// 没有任何写入方，留着只会多一条会漂移的数据与一个没人命中的唯一索引。

// presetModelToken 是预设体里「换成这个接口的 model 示例值」的占位符。
//
// 它只存在于**种子**这一侧：写库前就换成字面量，投影期不再解析任何占位符
// （曾经投影期替换，于是后台改了预设体、页面还是旧模型名）。
const presetModelToken = "$model"

// modelExampleOf 取这个接口 model 字段的示例值（字段优先，其次请求示例体）。
func modelExampleOf(doc apidoc.EndpointDoc) any {
	for _, f := range doc.Fields {
		if f.Name == "model" && f.Example != nil {
			return f.Example
		}
	}
	if v, ok := doc.Example.Get("model"); ok {
		return v
	}
	return nil
}

// resolvePresetModel 把预设体里的 `$model` 换成具体值；取不到值就删键。
//
// 删键而不是保留 `$model`：`"model": "$model"` 抄进请求就是一个必然 404 的
// 模型名，读者还以为平台坏了。取不到值说明这篇没有 model 字段（比如任务查询），
// 那这个键本来就不该出现。
func resolvePresetModel(o apidoc.Obj, val any) apidoc.Obj {
	if len(o) == 0 {
		return o
	}
	out := make(apidoc.Obj, 0, len(o))
	for _, kv := range o {
		if s, ok := kv.V.(string); ok && s == presetModelToken {
			if val == nil {
				continue
			}
			out = append(out, apidoc.KV{K: kv.K, V: val})
			continue
		}
		out = append(out, kv)
	}
	return out
}

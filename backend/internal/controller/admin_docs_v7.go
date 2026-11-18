package controller

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/docdocs"
)

// 管理台 · 文档工作台（v7）HTTP 面。
//
// 只有两类 handler：读（tree/node/endpoint/preview/transports/check）与写
// （create/save/delete/publish/unpublish）。**没有任何渲染逻辑**：
// 预览走 docdocs.ProjectEndpoint（与公开页同一个函数），
// 否则「预览好看、发布后变形」这类错误两边都看起来正常。
//
// 所有路径都以 `/admin/docs/` 开头，与公开的 `/api/docs/` 物理分开 ——
// 管理接口误挂到公开路由组是这类系统最常见的事故。

// AdminDocV7Tree 返回工作台目录（含草稿、隐藏项）。
func AdminDocV7Tree(c *gin.Context) {
	t, err := docdocs.LoadAdminTree(siteurl.Base(c))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, t)
}

// AdminDocV7Transports 返回「绑定平台端点」下拉的可选清单。
func AdminDocV7Transports(c *gin.Context) {
	response.OK(c, docdocs.TransportOptions())
}

// AdminDocV7NodeGet 返回节点编辑页。
func AdminDocV7NodeGet(c *gin.Context) {
	n, err := docdocs.LoadAdminNode(c.Param("key"))
	if err != nil {
		response.NotFound(c)
		return
	}
	response.OK(c, n)
}

// AdminDocV7NodeCreate 新建节点（草稿）。
func AdminDocV7NodeCreate(c *gin.Context) {
	var in docdocs.NodeCreateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	adminID := c.GetUint64("user_id")
	n, err := docdocs.CreateNode(in, adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsNodeCreate, "doc_node", in.Key, nil)
	response.OK(c, n)
}

// AdminDocV7NodeSave 保存节点内容（退回草稿）。
func AdminDocV7NodeSave(c *gin.Context) {
	key := strings.TrimSpace(c.Param("key"))
	var in docdocs.NodeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	adminID := c.GetUint64("user_id")
	n, err := docdocs.SaveNode(key, in, model.DocOriginManual, adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsNodeSave, "doc_node", key, nil)
	response.OK(c, n)
}

// AdminDocV7NodeDelete 删除节点（含其下全部接口）。
func AdminDocV7NodeDelete(c *gin.Context) {
	key := strings.TrimSpace(c.Param("key"))
	if err := docdocs.DeleteNode(key); err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionDocsNodeDelete, "doc_node", key, nil)
	response.OK(c, gin.H{"deleted": key})
}

// AdminDocV7NodePublish 发布节点（体检不过 → 422 + problems）。
func AdminDocV7NodePublish(c *gin.Context) {
	key := strings.TrimSpace(c.Param("key"))
	adminID := c.GetUint64("user_id")
	n, err := docdocs.PublishNode(key, adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsNodePublish, "doc_node", key, nil)
	response.OK(c, n)
}

// AdminDocV7NodeUnpublish 节点下架。
func AdminDocV7NodeUnpublish(c *gin.Context) {
	key := strings.TrimSpace(c.Param("key"))
	adminID := c.GetUint64("user_id")
	n, err := docdocs.UnpublishNode(key, adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsNodeUnpublish, "doc_node", key, nil)
	response.OK(c, n)
}

// AdminDocV7EndpointGet 返回接口编辑页（含体检结论）。
func AdminDocV7EndpointGet(c *gin.Context) {
	e, err := docdocs.LoadAdminEndpoint(c.Param("key"), c.Param("ep"))
	if err != nil {
		response.NotFound(c)
		return
	}
	response.OK(c, e)
}

// AdminDocV7EndpointCreate 在节点下新建接口（草稿）。
func AdminDocV7EndpointCreate(c *gin.Context) {
	var in docdocs.EndpointCreateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	adminID := c.GetUint64("user_id")
	e, err := docdocs.CreateEndpoint(c.Param("key"), in, adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsEndpointCreate, "doc_endpoint",
		c.Param("key")+"/"+in.Key, nil)
	response.OK(c, e)
}

// AdminDocV7EndpointSave 保存整篇（字段/变体/示例/预设/错误码全量替换）。
func AdminDocV7EndpointSave(c *gin.Context) {
	var in docdocs.EndpointInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	adminID := c.GetUint64("user_id")
	e, err := docdocs.SaveEndpoint(c.Param("key"), c.Param("ep"), in, model.DocOriginManual, adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsEndpointSave, "doc_endpoint",
		c.Param("key")+"/"+c.Param("ep"), nil)
	response.OK(c, e)
}

// AdminDocV7EndpointFields 只替换请求字段（给 AI 局部写与「字段表」单独保存）。
func AdminDocV7EndpointFields(c *gin.Context) {
	var in struct {
		Version int                 `json:"version"`
		Fields  []docdocs.FieldView `json:"fields"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	adminID := c.GetUint64("user_id")
	e, err := docdocs.WriteFields(c.Param("key"), c.Param("ep"), in.Fields, in.Version, model.DocOriginManual, adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsEndpointFields, "doc_endpoint",
		c.Param("key")+"/"+c.Param("ep"), nil)
	response.OK(c, e)
}

// AdminDocV7EndpointDelete 删除一篇接口文档。
func AdminDocV7EndpointDelete(c *gin.Context) {
	if err := docdocs.DeleteEndpoint(c.Param("key"), c.Param("ep")); err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionDocsEndpointDelete,
		"doc_endpoint", c.Param("key")+"/"+c.Param("ep"), nil)
	response.OK(c, gin.H{"deleted": c.Param("ep")})
}

// AdminDocV7EndpointPublish 发布一篇（未绑定/体检不过 → 422 + problems）。
func AdminDocV7EndpointPublish(c *gin.Context) {
	adminID := c.GetUint64("user_id")
	e, n, err := docdocs.PublishEndpoint(c.Param("key"), c.Param("ep"), adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsEndpointPublish, "doc_endpoint",
		c.Param("key")+"/"+c.Param("ep"), nil)
	// 一次发布同时可能改了节点状态：两个对象一起回，前端才能把两份 base 都重置掉。
	response.OK(c, gin.H{"endpoint": e, "node": n})
}

// AdminDocV7EndpointUnpublish 撤回到草稿。
func AdminDocV7EndpointUnpublish(c *gin.Context) {
	adminID := c.GetUint64("user_id")
	e, n, err := docdocs.UnpublishEndpoint(c.Param("key"), c.Param("ep"), adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsEndpointUnpublish, "doc_endpoint",
		c.Param("key")+"/"+c.Param("ep"), nil)
	response.OK(c, gin.H{"endpoint": e, "node": n})
}

// AdminDocV7EndpointPreview 用**公开渲染器**渲染草稿（右栏实时预览）。
//
// 不复用 DocsEndpointV7：那个会因 status 未发布而 404，而预览的全部意义
// 就是看未发布的东西。差别只在可见性判断，渲染必须是同一个函数。
func AdminDocV7EndpointPreview(c *gin.Context) {
	b, err := docdocs.LoadEndpoint(c.Param("key"), c.Param("ep"))
	if err != nil {
		response.NotFound(c)
		return
	}
	response.OK(c, docdocs.ProjectEndpoint(b, siteurl.Base(c)))
}

// AdminDocV7EndpointPreviewDraft 用**当前编辑内容**（可能未保存）生成预览。
//
// 为什么必须是 POST + 整份内容：右栏预览要跟着输入框变。若只在保存后刷新预览，
// 运营会养成「先保存再看好不好看」的习惯，而保存会把已发布文档退回草稿 ——
// 为了看一眼预览就下线一篇线上文档，是这套系统最不该鼓励的操作。
func AdminDocV7EndpointPreviewDraft(c *gin.Context) {
	var in docdocs.EndpointInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	out, err := docdocs.PreviewEndpointInput(c.Param("key"), c.Param("ep"), in, siteurl.Base(c))
	if err != nil {
		response.NotFound(c)
		return
	}
	response.OK(c, out)
}

// AdminDocV7Sections 返回板块清单（含隐藏项与版本号）。
func AdminDocV7Sections(c *gin.Context) {
	list, err := docdocs.LoadSections()
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, list)
}

// AdminDocV7SectionSave 保存板块（改名/改序/改色/隐藏）。
func AdminDocV7SectionSave(c *gin.Context) {
	key := strings.TrimSpace(c.Param("key"))
	var in docdocs.SectionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	adminID := c.GetUint64("user_id")
	out, err := docdocs.SaveSection(key, in, adminID)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionDocsSectionSave, "doc_section", key, nil)
	response.OK(c, out)
}

// AdminDocV7NodePreviewDraft 用当前编辑内容渲染说明页（未保存也可看）。
func AdminDocV7NodePreviewDraft(c *gin.Context) {
	var in docdocs.NodeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	out, err := docdocs.PreviewNodeInput(c.Param("key"), in, siteurl.Base(c))
	if err != nil {
		response.NotFound(c)
		return
	}
	response.OK(c, out)
}

// AdminDocV7SamplesGenerate 按当前草稿的字段示例值重新生成四段示例代码（不落库）。
//
// 不落库是刻意的：生成结果先回到输入框，运营看着满意再保存 ——
// 否则"点一下生成"就改库，等于给示例代码开了第二条写路径。
func AdminDocV7SamplesGenerate(c *gin.Context) {
	var in docdocs.EndpointInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	sv, err := docdocs.GenerateEndpointSamples(c.Param("key"), c.Param("ep"), in)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	response.OK(c, gin.H{"samples": sv})
}

// AdminDocV7PresetSamplesGenerate 按某个调用方式的 body 生成示例（不落库）。
func AdminDocV7PresetSamplesGenerate(c *gin.Context) {
	var in struct {
		Body []docdocs.KVItem `json:"body"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	sv, err := docdocs.GeneratePresetSamples(c.Param("key"), c.Param("ep"), in.Body)
	if err != nil {
		respondDocV7Err(c, err)
		return
	}
	response.OK(c, gin.H{"samples": sv})
}

// AdminDocV7Check 全站体检（启动期同一函数，管理台手动刷新）。
func AdminDocV7Check(c *gin.Context) {
	probs := docdocs.CheckAll()
	response.OK(c, gin.H{"problems": probs, "blocking": len(docdocs.BlockingProblems(probs))})
}

// respondDocV7Err 把服务层错误映射成可分辨的响应。
//
// 三类必须分开，因为下一步动作完全不同：
//   - 404：文档被删了（刷新目录）
//   - 409：别人先保存了（刷新这篇再改）
//   - 422 + problems：内容不合格（按清单逐条改）
func respondDocV7Err(c *gin.Context, err error) {
	var blocked *docdocs.BlockedError
	switch {
	case errors.As(err, &blocked):
		response.FailDefData(c, errs.DocPublishBlocked, gin.H{
			"problems": blocked.Problems,
			"blocking": docdocs.BlockingProblems(blocked.Problems),
		})
	case errors.Is(err, docdocs.ErrDocNotFound), errors.Is(err, docdocs.ErrDocEndpointNotFound):
		response.NotFound(c)
	case errors.Is(err, docdocs.ErrDocVersion):
		response.FailDef(c, errs.DocVersionConflict)
	case errors.Is(err, docdocs.ErrDocKeyTaken):
		response.FailDef(c, errs.DocKeyExists)
	default:
		// 其余都是「人话」的输入错误（key 非法、超长、模型被占用）。
		if isUserInputErr(err) {
			response.FailDefMsg(c, errs.InvalidParam, err.Error())
			return
		}
		response.FailDef(c, errs.Internal)
	}
}

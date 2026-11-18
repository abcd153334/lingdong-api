package controller

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	specdsl "lingdong-api/internal/relay/spec"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/spec"
)

// adapterSpecReq 是规格的创建/编辑请求体。
//
// spec 允许省略 slug/label/vendor（由表单字段补齐）；显式写了却与表单不一致会报错，
// 不做静默覆盖——否则运营改了表单却发现列表显示旧值，只能靠猜。
type adapterSpecReq struct {
	Slug   string `json:"slug"`
	Label  string `json:"label"`
	Vendor string `json:"vendor"`
	// Status 缺省（null）= 新建按启用、更新按不改。
	Status *int `json:"status"`
	// Version 仅更新时用于乐观锁；null/0 = 不校验。
	Version *int            `json:"version"`
	Spec    json.RawMessage `json:"spec"`
}

func (r adapterSpecReq) toInput() spec.Input {
	in := spec.Input{Slug: r.Slug, Label: r.Label, Vendor: r.Vendor, Spec: r.Spec}
	if r.Status != nil {
		in.Status = *r.Status
	}
	if r.Version != nil {
		in.Version = *r.Version
	}
	return in
}

// specLimitsView 是规格编辑器的限额提示（与 relay/spec 的常量同源，不手抄数字）。
type specLimitsView struct {
	MaxEndpoints   int    `json:"max_endpoints"`
	MaxConstKeys   int    `json:"max_const_keys"`
	MaxCopyKeys    int    `json:"max_copy_keys"`
	MaxArrays      int    `json:"max_arrays"`
	MaxArrayItems  int    `json:"max_array_items"`
	MaxStaticQuery int    `json:"max_static_query"`
	MaxPathLen     int    `json:"max_path_len"`
	MaxSpecBytes   int    `json:"max_spec_bytes"`
	MaxSlugLen     int    `json:"max_slug_len"`
	MaxLabelRunes  int    `json:"max_label_runes"`
	PollKeySuffix  string `json:"poll_key_suffix"`
}

func specLimits() specLimitsView {
	return specLimitsView{
		MaxEndpoints:   specdsl.MaxEndpoints,
		MaxConstKeys:   specdsl.MaxConstKeys,
		MaxCopyKeys:    specdsl.MaxCopyKeys,
		MaxArrays:      specdsl.MaxArrays,
		MaxArrayItems:  specdsl.MaxArrayItems,
		MaxStaticQuery: specdsl.MaxStaticQuery,
		MaxPathLen:     specdsl.MaxPathLen,
		MaxSpecBytes:   specdsl.MaxSpecBytes,
		MaxSlugLen:     specdsl.MaxSlugLen,
		MaxLabelRunes:  specdsl.MaxLabelRunes,
		PollKeySuffix:  specdsl.PollKeySuffix,
	}
}

// ListAdminAdapterSpecs 处理 GET /api/admin/adapter-specs。
//
// 同时下发 `endpoints`（可用端点键与模态）与 `limits`：
// 规格编辑器必须"只能选平台认得的端点"，让前端硬编码端点清单必然会漂移——
// 后端新增端点后前端下拉里没有，运营以为不支持。
func ListAdminAdapterSpecs(c *gin.Context) {
	items, err := spec.List(c.Request.Context())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"items": items, "endpoints": apidocSpecEndpointViews(), "limits": specLimits()})
}

// GetAdminAdapterSpec 处理 GET /api/admin/adapter-specs/{id}（含 DSL 原文）。
func GetAdminAdapterSpec(c *gin.Context) {
	id, ok := specID(c)
	if !ok {
		return
	}
	item, err := spec.Get(c.Request.Context(), id)
	if err != nil {
		respondSpecErr(c, err)
		return
	}
	response.OK(c, gin.H{"item": item, "endpoints": apidocSpecEndpointViews(), "limits": specLimits()})
}

// CreateAdminAdapterSpec 处理 POST /api/admin/adapter-specs。
func CreateAdminAdapterSpec(c *gin.Context) {
	var req adapterSpecReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	in := req.toInput()
	// owner 只从登录态取，不接受请求体：否则任何管理员都能把归属改成别人。
	in.OwnerID = c.GetUint64("user_id")
	item, err := spec.Create(c.Request.Context(), in)
	if err != nil {
		respondSpecErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAdapterSpecCreate, "adapter_spec",
		strconv.FormatUint(item.ID, 10), gin.H{"slug": item.Slug, "endpoints": item.Endpoints})
	afterChange(c.Request.Context(), "adapter_spec.create")
	response.OK(c, gin.H{"item": item})
}

// UpdateAdminAdapterSpec 处理 PUT /api/admin/adapter-specs/{id}。
func UpdateAdminAdapterSpec(c *gin.Context) {
	id, ok := specID(c)
	if !ok {
		return
	}
	var req adapterSpecReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	item, err := spec.Update(c.Request.Context(), id, req.toInput())
	if err != nil {
		respondSpecErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAdapterSpecUpdate, "adapter_spec",
		strconv.FormatUint(id, 10), gin.H{"slug": item.Slug, "version": item.Version})
	// 规格内容决定「这个模型对外有哪些扩展字段」，改完必须重算文档投影。
	afterChange(c.Request.Context(), "adapter_spec.update")
	response.OK(c, gin.H{"item": item})
}

// SetAdapterSpecStatus 处理 PATCH /api/admin/adapter-specs/{id}/status。
//
// 单独一条 PATCH 而不是塞进 PUT：停用会让引用它的渠道立刻失效，
// 是一个需要独立留痕的动作（与 packages 的启停同规矩）。
func SetAdapterSpecStatus(c *gin.Context) {
	id, ok := specID(c)
	if !ok {
		return
	}
	var req struct {
		Status *int `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Status == nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if err := spec.SetStatus(c.Request.Context(), id, *req.Status); err != nil {
		respondSpecErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAdapterSpecStatus, "adapter_spec",
		strconv.FormatUint(id, 10), gin.H{"status": *req.Status})
	afterChange(c.Request.Context(), "adapter_spec.status")
	response.OK(c, nil)
}

// GetAdapterSpecImpact 处理 GET /api/admin/adapter-specs/{id}/impact。
//
// 返回「删这份规格会影响谁」：引用渠道、绑定模型、模型声明的端点。
// 删除确认框必须先读它 —— 后端仍会二次拒绝（ErrInUse），前端只是提前告知。
func GetAdapterSpecImpact(c *gin.Context) {
	id, ok := specID(c)
	if !ok {
		return
	}
	imp, err := spec.ImpactOf(c.Request.Context(), id, siteurl.Base(c))
	if err != nil {
		respondSpecErr(c, err)
		return
	}
	response.OK(c, gin.H{"impact": imp})
}

// DeleteAdminAdapterSpec 处理 DELETE /api/admin/adapter-specs/{id}（仍被渠道引用时拒绝）。
func DeleteAdminAdapterSpec(c *gin.Context) {
	id, ok := specID(c)
	if !ok {
		return
	}
	if err := spec.Delete(c.Request.Context(), id); err != nil {
		respondSpecErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAdapterSpecDelete, "adapter_spec",
		strconv.FormatUint(id, 10), nil)
	afterChange(c.Request.Context(), "adapter_spec.delete")
	response.OK(c, nil)
}

func specID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return 0, false
	}
	return id, true
}

// respondSpecErr 把服务层错误映射成对外错误码。
//
// 字段级校验失败必须带回字段名与原因：规格是手写/由 AI 生成的 JSON，
// 只说"参数不合法"等于让运营从 200 行 JSON 里猜哪一行错了。
func respondSpecErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, spec.ErrNotFound):
		response.FailDef(c, errs.NotFound)
	case errors.Is(err, spec.ErrSlugTaken):
		response.FailDef(c, errs.AdapterSpecExists)
	case errors.Is(err, spec.ErrInUse):
		response.FailDef(c, errs.AdapterSpecInUse)
	case errors.Is(err, spec.ErrConflict):
		response.FailDef(c, errs.AdapterSpecConflict)
	case errors.Is(err, spec.ErrSlugImmutable):
		response.FailDef(c, errs.AdapterSpecInvalid.WithMessage(err.Error()))
	default:
		// 字段名必须一起返回：规格是手写/由 AI 生成的 JSON，只说"参数不合法"
		// 等于让运营从两百行 JSON 里猜哪一行错了。
		if field, msg := specdsl.AsFieldError(err); msg != "" {
			if field != "" {
				msg = field + "：" + msg
			}
			response.FailDef(c, errs.AdapterSpecInvalid.WithMessage(msg))
			return
		}
		response.FailDefMsg(c, errs.AdapterSpecInvalid, err.Error())
	}
}

// apidocSpecEndpointViews 供详情页复用（列表与详情下发的端点视图必须一致）。
func apidocSpecEndpointViews() []gin.H {
	eps := apidoc.SpecEndpoints()
	views := make([]gin.H, 0, len(eps))
	for _, ep := range eps {
		views = append(views, gin.H{
			"key":      ep.Key,
			"kind":     string(ep.Kind),
			"modality": ep.Modality,
			"async":    ep.Async,
		})
	}
	return views
}

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/appplugins"
	"lingdong-api/internal/service/appplugins/devdoc"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/iconref"
	"lingdong-api/internal/service/logs"
)

// 应用插件（声明式）的管理面。
//
// 全部挂在 /api/admin 组（AdminAuth + CSRF）。刻意**不复用**已下线插件系统的
// /api/admin/plugins：那套是"执行第三方代码"，这里是"解释一份声明"，
// 共用命名空间会让人以为执行型插件复活了（router/legacy_plugin_namespace_test.go 守着）。

// ---- 目录 ----

func AdminAppPluginList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	items, total, err := appplugins.ListCatalog(c.Request.Context(), db.DB, c.Query("q"), p.Offset(), p.Limit())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	base := siteurl.Base(c)
	for i := range items {
		items[i].IconURL = iconref.URL(base, items[i].Icon)
	}
	response.OK(c, pagination.New(items, total, p))
}

// AdminAppPluginDevDoc 下发**由代码生成**的插件开发手册（markdown）。
//
// 与页面同源：正文与表格都来自 devdoc（常量 + 类型矩阵 + 上限 + 错误码），
// 不是仓库里另存的一份 md —— 否则"下载到的手册"会和当前部署能力脱节，
// 而这正是手写文档已经发生过的问题。
//
// 文件名用 ASCII 兜底 + RFC 5987 的中文名：safeFilename 只保留 ASCII 字符，
// 直接写中文会被部分浏览器截成空文件名。
func AdminAppPluginDevDoc(c *gin.Context) {
	body := []byte(devdoc.Markdown())
	const ct = "text/markdown; charset=utf-8"
	c.Header("Content-Type", ct)
	c.Header("Content-Disposition",
		`attachment; filename="plugin-dev-manual.md"; filename*=UTF-8''`+url.PathEscape(devdoc.FileName))
	c.Header("X-Content-Type-Options", "nosniff")
	// 弱 ETag + no-cache：同一份构建下内容恒定（反复下载走 304），
	// 换构建后 ETag 自然变化，不会把旧手册当新手册。
	if writeETag(c, body, 0, false, true) {
		return
	}
	c.Data(http.StatusOK, ct, body)
}

// AdminAppPluginSample 下发内置示例模板（不落库，见 appplugins.SampleManifest 注释）。
func AdminAppPluginSample(c *gin.Context) {
	response.OK(c, gin.H{"manifest": json.RawMessage(appplugins.SampleManifest)})
}

func AdminAppPluginGet(c *gin.Context) {
	key := c.Param("key")
	row, m, err := appplugins.Get(c.Request.Context(), db.DB, key)
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	d := m.Describe()
	d.IconURL = iconref.URL(siteurl.Base(c), m.Icon)
	var installs int64
	db.DB.Model(&model.AppPluginInstall{}).
		Where("plugin_key = ? AND status <> ?", key, model.AppPluginInstallRemoved).
		Count(&installs)
	response.OK(c, gin.H{
		"plugin":       d,
		"manifest":     json.RawMessage(row.Manifest),
		"status":       row.Status,
		"source":       row.Source,
		"lock_version": row.LockVersion,
		"installs":     installs,
		"updated_at":   row.UpdatedAt.Format("2006-01-02 15:04:05"),
	})
}

type pluginImportReq struct {
	ExpectedVersion int             `json:"expected_version"`
	Force           bool            `json:"force"`
	Manifest        json.RawMessage `json:"manifest"`
}

// AdminAppPluginImport 导入/升级目录。?dry_run=1 只体检、不落库。
func AdminAppPluginImport(c *gin.Context) {
	var req pluginImportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if len(req.Manifest) == 0 {
		response.FailDefMsg(c, errs.InvalidParam, "缺少插件定义")
		return
	}
	dry := c.Query("dry_run") == "1"
	force := req.Force || c.Query("force") == "1"
	res, err := appplugins.Import(c.Request.Context(), db.DB, appplugins.ImportInput{
		Key:             c.Param("key"),
		Raw:             req.Manifest,
		ExpectedVersion: req.ExpectedVersion,
		Force:           force,
		DryRun:          dry,
		AdminID:         c.GetUint64("user_id"),
	})
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	if res.Applied {
		audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginImport,
			"app_plugin", res.Key, gin.H{
				"version": res.Version, "lock_version": res.LockVersion, "created": res.Created,
			})
	}
	response.OK(c, res)
}

type pluginStatusReq struct {
	Status int16 `json:"status"`
}

func AdminAppPluginStatus(c *gin.Context) {
	var req pluginStatusReq
	if err := c.ShouldBindJSON(&req); err != nil || (req.Status != 1 && req.Status != 2) {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	key := c.Param("key")
	if err := appplugins.SetStatus(c.Request.Context(), db.DB, key, req.Status); err != nil {
		respondPluginErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginStatus,
		"app_plugin", key, gin.H{"status": req.Status})
	response.OK(c, gin.H{"key": key, "status": req.Status})
}

func AdminAppPluginDelete(c *gin.Context) {
	key := c.Param("key")
	if err := appplugins.DeleteCatalog(c.Request.Context(), db.DB, key); err != nil {
		respondPluginErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginDelete,
		"app_plugin", key, nil)
	response.OK(c, gin.H{"key": key})
}

// ---- 安装 ----

func AdminAppPluginInstalls(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	items, err := appplugins.ListInstalls(c.Request.Context(), db.DB, app.ID)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	base := siteurl.Base(c)
	for i := range items {
		items[i].Icon = iconref.URL(base, items[i].Icon)
	}
	response.OK(c, gin.H{"items": items})
}

type pluginInstallReq struct {
	PluginKey string          `json:"plugin_key"`
	Config    json.RawMessage `json:"config"`
}

func AdminAppPluginInstall(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	var req pluginInstallReq
	if err := c.ShouldBindJSON(&req); err != nil || req.PluginKey == "" {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	cfg, err := appplugins.DecodeObject(req.Config)
	if err != nil {
		response.FailDefMsg(c, errs.InvalidParam, "配置不是合法的 JSON 对象")
		return
	}
	row, err := appplugins.Install(c.Request.Context(), db.DB, app.ID, req.PluginKey, cfg)
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginInstall,
		"app_plugin_install", fmt.Sprintf("%d/%s", app.ID, req.PluginKey), gin.H{
			"app_id": app.ID, "plugin_key": req.PluginKey, "plugin_version": row.PluginVersion,
		})
	response.OK(c, gin.H{"plugin_key": row.PluginKey, "status": row.Status, "lock_version": row.LockVersion})
}

// AdminAppPluginDetail 是泛化路由页面的唯一数据源。
//
// 已卸载也返回 200（带 record_count）：否则"重新启用 / 清除数据"的入口
// 会随 409 一起消失，而数据其实还在库里。
func AdminAppPluginDetail(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	d, err := appplugins.GetDetail(c.Request.Context(), db.DB, app.ID, c.Param("key"))
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	d.Plugin.IconURL = iconref.URL(siteurl.Base(c), d.Plugin.Icon)
	d.Install.Icon = d.Plugin.IconURL
	// 页面标题要写清"哪个应用的哪个插件"：只给 id 会让人靠面包屑猜。
	name := app.DisplayName
	if name == "" {
		name = app.Name
	}
	response.OK(c, gin.H{
		"install":          d.Install,
		"plugin":           d.Plugin,
		"drift":            d.Drift,
		"record_count":     d.RecordCount,
		"catalog_disabled": d.CatalogDisabled,
		"app":              gin.H{"id": app.ID, "slug": app.Slug, "name": name},
	})
}

type pluginPatchReq struct {
	Config          json.RawMessage `json:"config"`
	Status          *int16          `json:"status"`
	PublicEnabled   *bool           `json:"public_enabled"`
	ExpectedVersion int             `json:"expected_version"`
}

func AdminAppPluginPatch(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	var req pluginPatchReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	in := appplugins.PatchInput{Status: req.Status, PublicEnabled: req.PublicEnabled, ExpectedVersion: req.ExpectedVersion}
	if len(req.Config) > 0 {
		cfg, err := appplugins.DecodeObject(req.Config)
		if err != nil {
			response.FailDefMsg(c, errs.InvalidParam, "配置不是合法的 JSON 对象")
			return
		}
		in.Config = &cfg
	}
	if in.Config == nil && in.Status == nil && in.PublicEnabled == nil {
		response.FailDefMsg(c, errs.InvalidParam, "没有要修改的内容")
		return
	}
	row, dropped, err := appplugins.Patch(c.Request.Context(), db.DB, app.ID, c.Param("key"), in)
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	// 配置与启停分开记账：一个是"改行为参数"，一个是"开关服务"，追责等级不同。
	//
	// dropped_config_keys：这次保存顺带清掉了当前 manifest 不承认的旧键。
	// 清理是对的（否则任何保存都 422），但会真的删数据，必须留痕。
	if in.Config != nil || len(dropped) > 0 {
		audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginConfig,
			"app_plugin_install", fmt.Sprintf("%d/%s", app.ID, c.Param("key")),
			gin.H{"app_id": app.ID, "plugin_key": c.Param("key"), "dropped_config_keys": dropped})
	}
	if in.PublicEnabled != nil {
		audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginPublic,
			"app_plugin_install", fmt.Sprintf("%d/%s", app.ID, c.Param("key")),
			gin.H{"app_id": app.ID, "plugin_key": c.Param("key"), "public_enabled": *in.PublicEnabled})
	}
	if in.Status != nil {
		audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginStatus,
			"app_plugin_install", fmt.Sprintf("%d/%s", app.ID, c.Param("key")),
			gin.H{"app_id": app.ID, "plugin_key": c.Param("key"), "status": row.Status})
	}
	response.OK(c, gin.H{
		"plugin_key": row.PluginKey, "status": row.Status,
		"public_enabled": row.PublicEnabled, "lock_version": row.LockVersion,
	})
}

func AdminAppPluginUninstall(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	if err := appplugins.Uninstall(c.Request.Context(), db.DB, app.ID, c.Param("key")); err != nil {
		respondPluginErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginUninstall,
		"app_plugin_install", fmt.Sprintf("%d/%s", app.ID, c.Param("key")),
		gin.H{"app_id": app.ID, "plugin_key": c.Param("key")})
	response.OK(c, gin.H{"plugin_key": c.Param("key"), "status": 3})
}

func AdminAppPluginPurge(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	n, err := appplugins.Purge(c.Request.Context(), db.DB, app.ID, c.Param("key"))
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginPurge,
		"app_plugin_install", fmt.Sprintf("%d/%s", app.ID, c.Param("key")),
		gin.H{"app_id": app.ID, "plugin_key": c.Param("key"), "deleted": n})
	response.OK(c, gin.H{"deleted": n})
}

// ---- 记录 ----

func AdminAppPluginRecordList(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	entity := c.Query("entity")
	if entity == "" {
		response.FailDefMsg(c, errs.InvalidParam, "缺少数据对象")
		return
	}
	items, total, err := appplugins.ListRecords(c.Request.Context(), db.DB, app.ID,
		c.Param("key"), entity, p, c.Request.URL.Query())
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	response.OK(c, pagination.New(items, total, p))
}

type pluginRecordReq struct {
	Data    json.RawMessage `json:"data"`
	Entity  string          `json:"entity"`
	Version int             `json:"version"`
}

func AdminAppPluginRecordCreate(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	var req pluginRecordReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Entity == "" {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	item, err := appplugins.Create(c.Request.Context(), db.DB, app.ID, c.Param("key"), req.Entity, req.Data)
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginRecordCreate,
		"app_plugin_record", item.RecordKey, gin.H{
			"app_id": app.ID, "plugin_key": c.Param("key"), "entity": req.Entity,
			"record_key": item.RecordKey,
		})
	response.OK(c, item)
}

func AdminAppPluginRecordUpdate(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	var req pluginRecordReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Entity == "" {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	item, err := appplugins.Update(c.Request.Context(), db.DB, app.ID, c.Param("key"),
		req.Entity, c.Param("rk"), req.Data, req.Version)
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	// detail 只记字段名不记值：审计列表会 SELECT 全量 detail，塞整行数据会让
	// 审计页与导出随插件数据一起膨胀；具体值需要时按 record_key 去业务表查。
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginRecordUpdate,
		"app_plugin_record", item.RecordKey, gin.H{
			"app_id": app.ID, "plugin_key": c.Param("key"), "entity": req.Entity,
			"record_key": item.RecordKey, "from_version": req.Version, "to_version": item.Version,
		})
	response.OK(c, item)
}

func AdminAppPluginRecordDelete(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	entity := c.Query("entity")
	if entity == "" {
		response.FailDefMsg(c, errs.InvalidParam, "缺少数据对象")
		return
	}
	snapshot, err := appplugins.Delete(c.Request.Context(), db.DB, app.ID, c.Param("key"), entity, c.Param("rk"))
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginRecordDelete,
		"app_plugin_record", c.Param("rk"), gin.H{
			"app_id": app.ID, "plugin_key": c.Param("key"), "entity": entity,
			"record_key": c.Param("rk"), "snapshot": truncateSnapshot(snapshot),
		})
	response.OK(c, gin.H{"record_key": c.Param("rk")})
}

type pluginRecordBatchDeleteReq struct {
	Entity     string   `json:"entity"`
	RecordKeys []string `json:"record_keys"`
}

// AdminAppPluginRecordBatchDelete 批量删除（单事务）。
//
// 审计的 target_id 只有 64 字符，装不下 100 个 record_key：记"哪个插件的哪一批"（batch:<key>），
// 明细留前 20 个并标 keys_truncated —— 够定位，不会把审计撑爆。
func AdminAppPluginRecordBatchDelete(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	var req pluginRecordBatchDeleteReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Entity == "" || len(req.RecordKeys) == 0 {
		response.FailDefMsg(c, errs.InvalidParam, "缺少数据对象或待删除记录")
		return
	}
	n, err := appplugins.BatchDelete(c.Request.Context(), db.DB, app.ID, c.Param("key"), req.Entity, req.RecordKeys)
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	keys := req.RecordKeys
	if len(keys) > 20 {
		keys = keys[:20]
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppPluginRecordBatchDelete,
		"app_plugin_record", "batch:"+c.Param("key"), gin.H{
			"app_id": app.ID, "plugin_key": c.Param("key"), "entity": req.Entity,
			"deleted": n, "requested": len(req.RecordKeys),
			"record_keys": keys, "keys_truncated": len(req.RecordKeys) > len(keys),
		})
	response.OK(c, gin.H{"deleted": n})
}

// AdminAppPluginRecordExport 流式导出 CSV。
//
// 不复用 admin_log.go 的 writeCSV：那个是全量拼串 + 一次写出，5 万行 JSONB
// 会先吃满内存；而插件列名来自 manifest，也必须过 Esc（只转义数据格会让
// 列名里的逗号直接破坏结构）。
func AdminAppPluginRecordExport(c *gin.Context) {
	app, ok := appOf(c)
	if !ok {
		return
	}
	key := c.Param("key")
	entity := c.Query("entity")
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 导出吃与列表**同一份** Query：屏幕上筛过什么，导出的就是同一批行。
	e, q, _, err := appplugins.ExportPlan(c.Request.Context(), db.DB, app.ID, key, entity, p, c.Request.URL.Query())
	if err != nil {
		respondPluginErr(c, err)
		return
	}
	cols := make([]string, 0, len(e.List.Columns))
	for _, ck := range e.List.Columns {
		if f, ok := e.FieldByKey(ck); ok {
			cols = append(cols, f.Key)
		}
	}
	filename := fmt.Sprintf("%s-%s.csv", key, time.Now().Format("20060102-150405"))
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Header("Content-Type", "text/csv; charset=utf-8")

	// 先写 BOM 与表头，再分批拉数据：表头也过 Esc（见函数注释）。
	header := make([]string, 0, len(cols))
	for _, ck := range cols {
		f, _ := e.FieldByKey(ck)
		header = append(header, logs.Esc(f.Label))
	}
	first := true
	var afterID uint64
	const batch = 2000
	c.Stream(func(w io.Writer) bool {
		if first {
			first = false
			c.Writer.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
			_, _ = io.WriteString(w, joinCSV(header)+"\r\n")
			return true
		}
		rows, err := appplugins.ExportBatch(c.Request.Context(), db.DB, app.ID, key, entity, e, q, afterID, batch)
		if err != nil || len(rows) == 0 {
			return false
		}
		for _, r := range rows {
			cells := make([]string, 0, len(cols))
			for _, ck := range cols {
				f, _ := e.FieldByKey(ck)
				cells = append(cells, logs.Esc(appplugins.CellText(f, r.Data[ck])))
			}
			_, _ = io.WriteString(w, joinCSV(cells)+"\r\n")
			afterID = r.ID
		}
		return len(rows) == batch
	})
}

func joinCSV(cells []string) string { return strings.Join(cells, ",") }

// truncateSnapshot 把删除快照限制在 2KB：审计是"事后能看出删了什么"，
// 不是备份系统（备份该走导出）。
func truncateSnapshot(v map[string]any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if len(b) <= 2048 {
		return json.RawMessage(b)
	}
	cut := b[:2048]
	return gin.H{"truncated": true, "size": len(b), "preview": string(cut)}
}

// respondPluginErr 是插件域错误映射的唯一入口。
//
// 校验类错误必须带 problems（前端要逐条定位到字段），所以走 FailDefData；
// 其余交给 DefOr，避免这里再写一遍 switch。
func respondPluginErr(c *gin.Context, err error) {
	// 查询参数不合法是 400（不是 422 的 problems）：这类错误的读者是地址栏，
	// 消息必须直接说清"哪一项、能取什么"，否则管理员只能猜。
	var qe *appplugins.QueryError
	if errors.As(err, &qe) {
		response.FailDefMsg(c, errs.InvalidParam, qe.Msg)
		return
	}
	var invalid *appplugins.InvalidError
	if errors.As(err, &invalid) {
		def := errs.PluginManifestInvalid
		if invalid.Breaking {
			def = def.WithMessage("这次升级会影响存量数据，确认后再导入")
		}
		response.FailDefData(c, def, gin.H{
			"problems": invalid.Problems,
			"breaking": invalid.Breaking,
		})
		return
	}
	response.FailDef(c, appplugins.DefOr(err, errs.Internal))
}

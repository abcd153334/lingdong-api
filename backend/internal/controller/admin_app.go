package controller

// 本文件是管理后台的**接入应用**管理：列表 / 新建 / 改名 / 启停 / 查看其下密钥。
//
// 「应用」是这套系统支持"多应用统一后台"的落点：模型、渠道、价目全平台一套，
// 按应用隔离的是密钥、订单，以及由密钥派生的调用记录（归因链
// usages.key_id -> api_keys.app_id）。
//
// 刻意**没有删除**：应用被 api_keys.app_id / orders.app_id 引用，删掉会留下
// join 不到名字的孤儿。下线手段是停用（status=2）—— 停用只拦"新签发"，
// 已签发的密钥仍可用，要彻底断流需逐个禁用密钥。两步操作是刻意的：
// 停应用影响新接入，停密钥影响正在跑的流量，不该一次点击同时完成。

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/apikey"
	appsvc "lingdong-api/internal/service/app"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/iconref"
)

// adminAppItem 是应用列表的一行（含聚合口径与卡片所需的展示字段）。
type adminAppItem struct {
	ID          uint64 `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      int    `json:"status"`
	// AllowSelfIssue=false 表示该应用关闭了自助签发（只有管理员/运营能发凭证）。
	AllowSelfIssue bool `json:"allow_self_issue"`
	// AccessMode 是 allow_self_issue 的派生展示值（self_service / approval）。
	// 派生而不是落库：两个字段一定会漂移。
	AccessMode string `json:"access_mode"`
	// 品牌字段：卡片要显示图标与一句话描述，弹窗要显示展示名。
	DisplayName string `json:"display_name"`
	Tagline     string `json:"tagline"`
	LogoURL     string `json:"logo_url"`
	// 维护模式与 Status 正交：可以"启用但维护中"，也可以"停用且维护中"。
	MaintenanceMode    bool   `json:"maintenance_mode"`
	MaintenanceMessage string `json:"maintenance_message"`
	CreatedAt          string `json:"created_at"`
	// KeyCount / ActiveKeyCount 是托管密钥数量（后者只数 status=1）。
	KeyCount       int64 `json:"key_count"`
	ActiveKeyCount int64 `json:"active_key_count"`
	// UserCount 是持有该应用托管密钥的去重用户数。
	UserCount int64 `json:"user_count"`
	// Used 是该应用累计消费（**微**），口径 = SUM(api_keys.used)。
	//
	// 为什么不用 usages 求和：api_keys.used 是**同一份**数字，KEY 级金额限额、
	// 用户看到的"已用"都读它。用 usages 另算一份会出现"账户页 100、后台 98"
	// 这种无法解释的差（usages 有 pending/released 状态，口径随结算时机变化）。
	Used int64 `json:"used"`
	// PaidCent 是该应用下已支付订单总额（**分**）。金额单位与 used 不同是刻意的：
	// 订单本身以分计价（支付渠道只认分），转成微再展示会让运营对不上支付流水。
	PaidCent int64 `json:"paid_cent"`
	PaidCnt  int64 `json:"paid_count"`
	// Calls7d 是近 7 日调用次数（卡片第三格指标）。
	// 这是列表里唯一扫 usages 的字段，走 (key_id, created_at) 索引。
	Calls7d int64 `json:"calls_7d"`
	// NoticeCount 是**此刻生效**的公告数（enabled 且落在时间窗内）。
	NoticeCount int64 `json:"notice_count"`
	// LatestRelease 是最近发布的版本；从未发布时为 null。
	LatestRelease *adminAppLatestRelease `json:"latest_release"`
}

// adminAppLatestRelease 是卡片角标用的极简版本信息。
type adminAppLatestRelease struct {
	Version     string `json:"version"`
	PublishedAt string `json:"published_at"`
	ForceUpdate bool   `json:"force_update"`
}

// AdminAppList 处理 GET /api/admin/apps：卡片列表（聚合 + 品牌摘要 + 维护态）。
//
// 聚合全部**按页内 ids 批量**：密钥/订单、近 7 日调用、生效公告、最近发布
// 各一次查询。逐行查会让 20 条卡片的列表变成 80 次往返。
func AdminAppList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	orderBy := "id DESC"
	switch strings.TrimSpace(c.Query("sort")) {
	case "", "id":
		orderBy = "id DESC"
	case "created_at":
		orderBy = "created_at DESC"
	case "name":
		orderBy = "name ASC"
	default:
		// 只允许真能走 SQL 的列。聚合值（消费/调用数）不提供排序：
		// 给了也会被忽略，那比没有更糟。
		response.FailDef(c, errs.InvalidParam)
		return
	}

	q := db.DB.WithContext(c.Request.Context()).Model(&model.App{})
	if kw := strings.TrimSpace(c.Query("keyword")); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("slug ILIKE ? OR name ILIKE ?", like, like)
	}
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		st, perr := strconv.Atoi(raw)
		if perr != nil {
			response.FailDef(c, errs.InvalidParam)
			return
		}
		q = q.Where("status = ?", st)
	}
	if raw := strings.TrimSpace(c.Query("maintenance")); raw != "" {
		switch raw {
		case "1":
			q = q.Where("maintenance_mode = ?", true)
		case "0":
			q = q.Where("maintenance_mode = ?", false)
		default:
			response.FailDef(c, errs.InvalidParam)
			return
		}
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	var apps []model.App
	if err := q.Order(orderBy).Offset(p.Offset()).Limit(p.Limit()).Find(&apps).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}

	ids := make([]uint64, 0, len(apps))
	for i := range apps {
		ids = append(ids, apps[i].ID)
	}
	ctx := c.Request.Context()
	agg, err := appsvc.StatsByApps(ctx, db.DB, ids)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	calls, err := appCalls7d(ctx, ids)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 生效公告计数与公开面共用同一份判据（service/app 的 noticeActiveWhere），
	// 区别只在条数上限：公开面有 LIMIT，计数没有。
	notices, err := appsvc.ActiveCountsByApp(ctx, db.DB, ids, time.Now())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	releases, err := appsvc.LatestPublished(ctx, db.DB, ids)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	base := siteurl.Base(c)

	items := make([]adminAppItem, 0, len(apps))
	for i := range apps {
		a := apps[i]
		s := agg[a.ID]
		item := adminAppItem{
			ID: a.ID, Slug: a.Slug, Name: a.Name, Description: a.Description,
			Status: int(a.Status), AllowSelfIssue: a.AllowSelfIssue,
			AccessMode:      appsvc.AccessModeOf(a.AllowSelfIssue),
			DisplayName:     appsvc.DisplayNameOf(&a),
			Tagline:         a.Tagline,
			LogoURL:         iconref.URL(base, a.LogoIcon),
			MaintenanceMode: a.MaintenanceMode, MaintenanceMessage: a.MaintenanceMessage,
			CreatedAt: rfc3339(a.CreatedAt),
			KeyCount:  s.KeyCount, ActiveKeyCount: s.ActiveKeyCount,
			UserCount: s.UserCount, Used: s.Used,
			PaidCent: s.PaidCent, PaidCnt: s.PaidCount,
			Calls7d: calls[a.ID], NoticeCount: notices[a.ID],
		}
		if r, ok := releases[a.ID]; ok {
			item.LatestRelease = &adminAppLatestRelease{
				Version:     r.Version,
				PublishedAt: rfc3339OrEmpty(r.PublishedAt),
				ForceUpdate: r.ForceUpdate,
			}
		}
		items = append(items, item)
	}
	response.OK(c, gin.H{"items": items, "total": total, "page": p.Page, "page_size": p.PageSize})
}

// appCalls7d 统计每个应用近 7 日的调用次数。
//
// 走 usages JOIN api_keys（usages 没有 app_id），索引是 idx_usages_key_created；
// JOIN 不过滤 api_keys.deleted_at —— 密钥软删后历史调用仍属于该应用。
func appCalls7d(ctx context.Context, appIDs []uint64) (map[uint64]int64, error) {
	out := make(map[uint64]int64, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		AppID uint64
		Cnt   int64
	}
	err := db.DB.WithContext(ctx).
		Table("usages AS u").
		Select("k.app_id AS app_id, COUNT(*) AS cnt").
		Joins("JOIN api_keys AS k ON k.id = u.key_id").
		Where("k.app_id IN ? AND k.kind = ? AND u.created_at >= ?",
			appIDs, model.KeyKindHosted, time.Now().AddDate(0, 0, -7)).
		Group("k.app_id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.AppID] = r.Cnt
	}
	return out, nil
}

// adminAppReq 是新建/改名的请求体。
type adminAppReq struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// AllowSelfIssue 用指针区分「没传」与「显式传 false」。
	// 缺省（nil）= 保持原值（更新）或取默认 true（新建）。
	AllowSelfIssue *bool `json:"allow_self_issue"`
}

// AdminAppCreate 处理 POST /api/admin/apps。
func AdminAppCreate(c *gin.Context) {
	var req adminAppReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	slug := strings.TrimSpace(req.Slug)
	name := strings.TrimSpace(req.Name)
	if !appSlugPattern.MatchString(slug) {
		response.FailDef(c, errs.InvalidParam.WithMessage("应用标识只能包含小写字母、数字与连字符，长度 1-32"))
		return
	}
	if name == "" || !maxRunes(name, 64) {
		response.FailDef(c, errs.InvalidParam.WithMessage("应用名称必填，且不超过 64 个字符"))
		return
	}
	desc := strings.TrimSpace(req.Description)
	if !maxRunes(desc, 255) {
		response.FailDef(c, errs.InvalidParam.WithMessage("应用说明不超过 255 个字符"))
		return
	}
	allowSelf := req.AllowSelfIssue == nil || *req.AllowSelfIssue
	// 用 map 插入而不是"结构体 Create + 补写 allow_self_issue"：
	// 该列带 `default:true`，GORM 对「零值 + default」的列会用列默认值替换，
	// 于是"关闭自助签发"必须再发一条 UPDATE。那条 UPDATE 失败时接口回 500、
	// 库里却留着一个人人可自助签发的应用 —— 安全开关静默 fail-open。
	//
	// 注意 map 分支**不会**自动填 created_at/updated_at（时间填充只在结构体
	// 分支），必须显式给；漏写会让 sort=created_at 排序反转、后台显示 0001-01-01。
	now := db.DB.NowFunc()
	row := map[string]any{
		"slug": slug, "name": name, "description": desc,
		"status":           model.AppStatusEnabled,
		"allow_self_issue": allowSelf,
		"created_at":       now,
		"updated_at":       now,
	}
	if err := db.DB.WithContext(c.Request.Context()).Model(&model.App{}).Create(row).Error; err != nil {
		// 唯一冲突就是 slug 被占：回一个可行动的原因，而不是 500。
		if pgerr.IsUnique(err) {
			response.FailDef(c, errs.AppSlugExists)
			return
		}
		response.FailDef(c, errs.Internal)
		return
	}
	appID := insertedID(row)
	if appID == 0 {
		// 插入成功但拿不到自增 id：审计与响应都依赖它，不能静默继续。
		response.FailDef(c, errs.Internal)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppCreate, "app",
		strconv.FormatUint(appID, 10), map[string]any{"slug": slug, "name": name})
	// 公开面的应用行快照是整表加载的，新建后要让本实例立刻看到它。
	appsvc.InvalidateAppPublic()
	response.OK(c, gin.H{"id": appID, "slug": slug})
}

// AdminAppUpdate 处理 PUT /api/admin/apps/{id}：改名称、描述与自助签发开关。
//
// slug 不允许改：它已经写进接入方的部署配置（签发路径 /apps/{slug}/hosted-key），
// 改名等于让全部接入方失效。要换标识只能新建一个应用并迁移密钥。
func AdminAppUpdate(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req adminAppReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || !maxRunes(name, 64) {
		response.FailDef(c, errs.InvalidParam.WithMessage("应用名称必填，且不超过 64 个字符"))
		return
	}
	desc := strings.TrimSpace(req.Description)
	if !maxRunes(desc, 255) {
		response.FailDef(c, errs.InvalidParam.WithMessage("应用说明不超过 255 个字符"))
		return
	}
	updates := map[string]any{
		"name":        name,
		"description": desc,
	}
	if req.AllowSelfIssue != nil {
		updates["allow_self_issue"] = *req.AllowSelfIssue
	}
	res := db.DB.WithContext(c.Request.Context()).Model(&model.App{}).Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if res.RowsAffected == 0 {
		response.FailDef(c, errs.AppNotFound)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppUpdate, "app",
		strconv.FormatUint(id, 10), map[string]any{"name": name, "allow_self_issue": req.AllowSelfIssue})
	// 改名会影响公开面的展示名回落（display_name 为空时用 name），必须失效。
	appsvc.InvalidateAppPublic()
	response.OK(c, nil)
}

// adminAppStatusReq 是启停请求体。
type adminAppStatusReq struct {
	// Status：1 启用 / 2 停用。用指针是为了拒绝"缺字段"被当成 0（0 不是合法值）。
	Status *int `json:"status"`
}

// AdminAppStatus 处理 PATCH /api/admin/apps/{id}/status。
func AdminAppStatus(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req adminAppStatusReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Status == nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if *req.Status != int(model.AppStatusEnabled) && *req.Status != int(model.AppStatusDisabled) {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	res := db.DB.WithContext(c.Request.Context()).Model(&model.App{}).
		Where("id = ?", id).Update("status", *req.Status)
	if res.Error != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if res.RowsAffected == 0 {
		response.FailDef(c, errs.AppNotFound)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppStatus, "app",
		strconv.FormatUint(id, 10), map[string]any{"status": *req.Status})
	response.OK(c, nil)
}

// adminAppKeyItem 是应用下的一把托管密钥（含持有者用户名）。
type adminAppKeyItem struct {
	ID        uint64 `json:"id"`
	UserID    uint64 `json:"user_id"`
	Username  string `json:"username"`
	KeyPrefix string `json:"key_prefix"`
	Status    int    `json:"status"`
	// DisabledReason 让管理员一眼区分"我封的"与"系统判的闲置超期"：
	// 前者要人工解锁或保持封禁，后者应用自己轮换一次就恢复了。
	// 不给的话，管理台只能看到一个笼统的"已禁用"，处置方式无从判断。
	DisabledReason string   `json:"disabled_reason"`
	Used           int64    `json:"used"`
	Scopes         []string `json:"scopes"`
	CreatedAt      string   `json:"created_at"`
	RotatedAt      string   `json:"rotated_at"`
}

// AdminAppKeys 处理 GET /api/admin/apps/{id}/keys：该应用下的托管密钥。
//
// 不返回明文、不返回密文：后台只需要"谁在用这个应用、用了多少"。
// 管理员要帮接入方恢复凭证时，正确动作是让接入方自己轮换（rotate=true），
// 而不是由后台导出别人的应用凭证。
func AdminAppKeys(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	q := db.DB.WithContext(c.Request.Context()).Model(&model.APIKey{}).
		Where("app_id = ? AND kind = ?", id, model.KeyKindHosted)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	var keys []model.APIKey
	if err := q.Order("id DESC").Offset(p.Offset()).Limit(p.Limit()).Find(&keys).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	names, err := usernamesOf(c, keys)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	items := make([]adminAppKeyItem, 0, len(keys))
	for i := range keys {
		k := keys[i]
		it := adminAppKeyItem{
			ID: k.ID, UserID: k.UserID, Username: names[k.UserID],
			KeyPrefix: k.KeyPrefix, Status: int(k.Status), Used: k.Used,
			DisabledReason: k.DisabledReason,
			Scopes:         apikey.DecodeStrings(k.Scopes),
			CreatedAt:      rfc3339(k.CreatedAt),
		}
		if k.ResetSecretAt != nil {
			it.RotatedAt = rfc3339OrEmpty(k.ResetSecretAt)
		}
		items = append(items, it)
	}
	response.OK(c, gin.H{"items": items, "total": total, "page": p.Page, "page_size": p.PageSize})
}

// usernamesOf 批量取用户名（避免每行一次查询）。
func usernamesOf(c *gin.Context, keys []model.APIKey) (map[uint64]string, error) {
	out := make(map[uint64]string, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	ids := make([]uint64, 0, len(keys))
	seen := map[uint64]struct{}{}
	for i := range keys {
		if _, ok := seen[keys[i].UserID]; ok {
			continue
		}
		seen[keys[i].UserID] = struct{}{}
		ids = append(ids, keys[i].UserID)
	}
	var users []model.User
	if err := db.DB.WithContext(c.Request.Context()).
		Select("id, username").Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, err
	}
	for i := range users {
		out[users[i].ID] = users[i].Username
	}
	return out, nil
}

// 唯一约束冲突判据见 pkg/pgerr（slug 撞车必须映射成 409，不能被兜成 500）。

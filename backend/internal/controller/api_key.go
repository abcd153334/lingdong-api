package controller

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/authevent"
	"lingdong-api/internal/service/schema"
)

// 允许的模型模态枚举：来自 service/schema 的目录（唯一真相源）。
//
// 不再在本文件内联字面量：模态现在是「schema 目录 + 计费方式矩阵 + 规格可承载性」
// 三件事的共同主语，散着写就会出现「KEY 能选某模态、模型却建不出来」。
//
// 其中 music 与 audio 的区别值得记住：music 是异步按任务计费的音乐生成（SUNO 等），
// audio 是同步 TTS/ASR —— 计费口径与产物形态都不同，不能合并。
var allowedModalities = schema.ModalitySet()

// ipCIRDPattern 对齐原型 keys.html 的 IP 段校验。
var ipCIRDPattern = regexp.MustCompile(`^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}(/(3[0-2]|[12]?\d))?$`)

// keyReq 是创建/编辑 API KEY 的请求体（编辑为整体替换）。
type keyReq struct {
	Name string `json:"name"`
	// Group 只为**显式拒绝**而保留。
	//
	// 计费分组是账号属性（users.group），不是密钥属性：它同时决定折扣倍率
	// （groupDiscount）与可用渠道池（groupMatches）。曾经这里直接落库，于是
	// 任何登录用户 PUT 一次就能把自己 KEY 的分组改成 `vip` —— 拿折扣 + 进 VIP
	// 渠道池，而默认种子就把 `vip` 配成 9 折并挂进了渠道分组。
	//
	// 现在：非空一律 400。静默忽略更糟 —— 调用方会以为设置生效了。
	Group               string   `json:"group"`
	AmountLimit         int64    `json:"amount_limit"`
	ExpiredAt           *string  `json:"expired_at"`
	ModelsAllowed       []string `json:"models_allowed"`
	ModalitiesAllowed   []string `json:"modalities_allowed"`
	AllowIPs            []string `json:"allow_ips"`
	RPMLimit            int      `json:"rpm_limit"`
	ConcurrencyLimit    int      `json:"concurrency_limit"`
	AllowUserQuotaQuery *bool    `json:"allow_user_quota_query"`
}

// keyItem 是 API KEY 的对外视图（不含 key_cipher / key_hash）。
type keyItem struct {
	ID        uint64 `json:"id"`
	KeyPrefix string `json:"key_prefix"`
	// Kind 区分"用户自建"与"应用托管"。前端据此隐藏编辑/重置/回显按钮 ——
	// 按钮点了也会被 ownEditableKey 拦成 409，但让用户先看到"由应用托管"
	// 比让他点一次失败更好。
	Kind string `json:"kind"`
	// AppID 是归属应用（0 = 平台直连）。门店户账单需要，也让用户明白这把密钥
	// 的消费记在哪个应用下。
	AppID uint64 `json:"app_id"`
	// AppSlug / AppName 是归属应用的展示信息（自建密钥恒为空串）。
	//
	// 为什么要下发而不是让前端另调 /portal/apps：那个调用是**跨请求时序依赖** ——
	// 前端必须先拿到应用清单才能把 app_id 映射成 slug，进而决定轮换按钮是否可点。
	// 一旦清单请求失败或密钥落在列表第二页，轮换入口就消失了，而且看不出原因。
	AppSlug string `json:"app_slug"`
	AppName string `json:"app_name"`
	// DisabledReason 是 status=2 的成因，决定用户看到什么文案、走哪条恢复路径：
	//   admin_block = 平台处置（轮换不会解除，只能联系平台）
	//   expired     = 闲置超期（轮换/门户操作即可恢复）
	//   ""          = 启用中
	DisabledReason string `json:"disabled_reason"`
	// Scopes 只对托管密钥有意义（用户自建恒为空数组）。
	Scopes              []string   `json:"scopes"`
	Name                string     `json:"name"`
	Group               string     `json:"group"`
	Status              int        `json:"status"`
	AmountLimit         int64      `json:"amount_limit"`
	Used                int64      `json:"used"`
	ModelsAllowed       []string   `json:"models_allowed"`
	ModalitiesAllowed   []string   `json:"modalities_allowed"`
	AllowIPs            []string   `json:"allow_ips"`
	RPMLimit            int        `json:"rpm_limit"`
	ConcurrencyLimit    int        `json:"concurrency_limit"`
	ExpiredAt           *time.Time `json:"expired_at"`
	AllowUserQuotaQuery bool       `json:"allow_user_quota_query"`
	CreatedAt           time.Time  `json:"created_at"`
	ResetSecretAt       *time.Time `json:"reset_secret_at"`
}

// appRef 是密钥归属应用的展示信息；自建密钥（app_id=0）用零值。
type appRef struct {
	Slug string
	Name string
}

// toKeyItem 投影密钥视图。
//
// group 参数是**生效分组**（与计费同一口径），不是 k.Group ——
// 后者永远是空串（见 keyReq.Group 的注释）。
func toKeyItem(k *model.APIKey, group string, ref appRef) keyItem {
	return keyItem{
		ID:                  k.ID,
		KeyPrefix:           k.KeyPrefix,
		Kind:                k.Kind,
		AppID:               k.AppID,
		AppSlug:             ref.Slug,
		AppName:             ref.Name,
		DisabledReason:      k.DisabledReason,
		Scopes:              apikey.DecodeStrings(k.Scopes),
		Name:                k.Name,
		Group:               group,
		Status:              int(k.Status),
		AmountLimit:         k.AmountLimit,
		Used:                k.Used,
		ModelsAllowed:       apikey.DecodeStrings(k.ModelsAllowed),
		ModalitiesAllowed:   apikey.DecodeStrings(k.ModalitiesAllowed),
		AllowIPs:            apikey.DecodeStrings(k.AllowIPs),
		RPMLimit:            k.RPMLimit,
		ConcurrencyLimit:    k.ConcurrencyLimit,
		ExpiredAt:           k.ExpiredAt,
		AllowUserQuotaQuery: k.AllowUserQuotaQuery,
		CreatedAt:           k.CreatedAt,
		ResetSecretAt:       k.ResetSecretAt,
	}
}

// recordKeyEvent 记录 API KEY 的签发/吊销。
//
// 写 authevent 而不是 admin_audit：门户自助操作按 v2.0 §4.9.8 不进管理审计，
// 但"凭证何时被建/被删/被重置"是排查额度异常的唯一起点，必须留痕。
// 失败只记日志不阻断业务 —— 审计写不进去不该让用户建不了 KEY。
func recordKeyEvent(c *gin.Context, event string, keyID uint64, detail map[string]any) {
	authevent.Record(c.Request.Context(), authevent.Event{
		UID:    c.GetUint64("user_id"),
		Event:  event,
		Result: authevent.ResultSuccess,
		IP:     c.ClientIP(),
		Detail: mergeDetail(map[string]any{"key_id": keyID}, detail),
	})
}

func mergeDetail(base, extra map[string]any) map[string]any {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// ListKeys 处理 GET /api/portal/keys（仅本人密钥）。
//
// 分页：单条 KEY 带 4 个 JSONB 数组（scopes/models/modalities/allow_ips），
// 上百模型时一条就 3–5KB；没有上限时「先刷几千把再拉列表」是现成的放大器。
// 上限 20（authcfg.KeyLimitPerUser）保证绝大多数用户永远只有一页，
// 响应里的 `items` 字段保持不变，前端不必改。
func ListKeys(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam.WithMessage(err.Error()))
		return
	}
	uid := c.GetUint64("user_id")

	var total int64
	q := db.DB.Model(&model.APIKey{}).Where("user_id = ?", uid)
	if err := q.Count(&total).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	var keys []model.APIKey
	if err := q.Order("id DESC").Offset(p.Offset()).Limit(p.Limit()).Find(&keys).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	group, ok := keyGroupOf(c)
	if !ok {
		response.FailDef(c, errs.Internal)
		return
	}
	// 归属应用的 slug/name 一次批量取回，供前端直接渲染与判断。
	// 单独查而不是 JOIN：分页 SQL 已经够长，JOIN 会让"这一页有多少条"更难读，
	// 而且自建密钥（app_id=0）在 LEFT JOIN 里全是空值，收益为零。
	refs := keyAppRefs(c.Request.Context(), keys)
	items := make([]keyItem, 0, len(keys))
	for i := range keys {
		items = append(items, toKeyItem(&keys[i], group, refs[keys[i].AppID]))
	}
	response.OK(c, pagination.New(items, total, p))
}

// keyAppRefs 批量取密钥归属应用的展示信息。
//
// 查失败只记日志并返回空 map：应用名是**展示性**信息，不该让它把整个密钥列表
// 变成 500（用户就看不到自己的密钥了）。代价是托管密钥卡片的轮换按钮暂时不可点，
// 而那是"少一个功能"，不是"页面打不开"。
func keyAppRefs(ctx context.Context, keys []model.APIKey) map[uint64]appRef {
	ids := make([]uint64, 0, 4)
	seen := map[uint64]bool{}
	for i := range keys {
		id := keys[i].AppID
		if id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	refs := make(map[uint64]appRef, len(ids))
	if len(ids) == 0 {
		return refs
	}
	var rows []struct {
		ID   uint64
		Slug string
		Name string
	}
	if err := db.DB.WithContext(ctx).Table("apps").
		Select("id, slug, name").Where("id IN ?", ids).Scan(&rows).Error; err != nil {
		logger.L().Warn("读取密钥归属应用信息失败（列表仍返回，托管密钥暂不可轮换）",
			zap.Any("app_ids", ids), zap.Error(err))
		return refs
	}
	for _, r := range rows {
		refs[r.ID] = appRef{Slug: r.Slug, Name: r.Name}
	}
	return refs
}

// keyGroupOf 读取当前账号的计费分组（KEY 分组的唯一来源）。
func keyGroupOf(c *gin.Context) (string, bool) {
	var u model.User
	if err := db.DB.WithContext(c.Request.Context()).
		Select("id", "group").First(&u, c.GetUint64("user_id")).Error; err != nil {
		return "", false
	}
	// 与计费完全同口径：账号为空时回落到 default（而不是空串），
	// 否则前端会显示一个计费侧并不存在的分组名。
	if u.Group == "" {
		return "default", true
	}
	return u.Group, true
}

// newKeyMaterial 生成一组「库里尚未被占用」的密钥材料：明文 / 密文 / hash / 前缀。
//
// 抽取理由：CreateKey 与 ResetKey 原先各写了一份一模一样的 5 次碰撞重试循环
// （生成 → 查 key_hash 是否已存在 → 冲突就换一组），改一侧漏另一侧是迟早的事。
// 碰撞是极低概率事件（key_hash 带随机盐），5 次仍撞说明有系统性问题，
// 此时返回 409 而不是 500：让用户点重试，而不是以为服务挂了。
func newKeyMaterial() (plain, cipher, hash, prefix string, fail *errs.Def) {
	for attempt := 0; attempt < 5; attempt++ {
		p, h, pre, err := apikey.Generate()
		if err != nil {
			d := errs.Internal
			return "", "", "", "", &d
		}
		var n int64
		if err := db.DB.Model(&model.APIKey{}).Where("key_hash = ?", h).Count(&n).Error; err != nil {
			d := errs.Internal
			return "", "", "", "", &d
		}
		if n > 0 {
			continue
		}
		c, err := apikey.Encrypt(p)
		if err != nil {
			d := errs.Internal
			return "", "", "", "", &d
		}
		return p, c, h, pre, nil
	}
	d := errs.KeyConflict
	return "", "", "", "", &d
}

// CreateKey 处理 POST /api/portal/keys：返回一次性明文（此后仅可经 plaintext 端点回显）。
func CreateKey(c *gin.Context) {
	req, ok := parseKeyReq(c)
	if !ok {
		return
	}
	uid := c.GetUint64("user_id")

	// 数量上限：先算后建。
	//
	// 并发下两个请求可能同时读到 count=19 然后各建一把（变成 21）—— 这里接受
	// 这个误差：超一把不会造成资损或越权，而加行锁会让"建 KEY"这种低频操作
	// 去争用户行的锁，影响面反而更大。
	if limit := authcfg.KeyLimitPerUser(); limit > 0 {
		var n int64
		if err := db.DB.Model(&model.APIKey{}).
			Where("user_id = ? AND kind = ? AND deleted_at IS NULL", uid, model.KeyKindUser).
			Count(&n).Error; err != nil {
			response.FailDef(c, errs.Internal)
			return
		}
		if n >= int64(limit) {
			response.FailDef(c, errs.KeyLimitReached.WithMessage(
				fmt.Sprintf("最多保留 %d 个 API KEY，请先删除不再使用的密钥", limit)))
			return
		}
	}

	plain, cipher, hash, prefix, fail := newKeyMaterial()
	if fail != nil {
		response.FailDef(c, *fail)
		return
	}

	k := model.APIKey{
		UserID:            uid,
		KeyCipher:         cipher,
		KeyHash:           hash,
		KeyPrefix:         prefix,
		Name:              req.Name,
		Status:            1,
		AmountLimit:       req.AmountLimit,
		ModelsAllowed:     jsonList(req.ModelsAllowed),
		ModalitiesAllowed: jsonList(req.ModalitiesAllowed),
		AllowIPs:          jsonList(req.AllowIPs),
		// 恒空：分组由账号决定（effectiveGroup 会在 k.Group 为空时回落到 users.group）。
		Group:               "",
		AllowUserQuotaQuery: req.allowQuery(),
		RPMLimit:            req.RPMLimit,
		ConcurrencyLimit:    req.ConcurrencyLimit,
		ExpiredAt:           req.expiredAt(),
	}
	if err := db.DB.Create(&k).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 显式覆盖 allow_user_quota_query：GORM 对「零值 + default 标签」的字段
	// 会用**默认值**替换后再插入（callbacks/create.go 的 DefaultValueInterface 分支），
	// 于是 `allow_user_quota_query=false` 会被写成 true —— 用户以为自己关掉了
	// 「允许 KEY 查询账户额度」，实际完全没关（这个开关只在 /v1/account 与
	// /api/user/self 上生效，正好是最容易被当成"安全边界"的两处）。
	// UPDATE 走 map，零值不会被吞；两件事必须同一事务，否则失败会留下
	// 一把「权限比用户要求更大」的 KEY。
	if want := req.allowQuery(); k.AllowUserQuotaQuery != want {
		if err := db.DB.Model(&model.APIKey{}).Where("id = ?", k.ID).
			Update("allow_user_quota_query", want).Error; err != nil {
			// 删掉刚建的 KEY：宁可返回失败，也不能把一把权限被放大的 KEY 交出去。
			db.DB.Delete(&model.APIKey{}, k.ID)
			response.FailDef(c, errs.Internal)
			return
		}
		k.AllowUserQuotaQuery = want
	}
	// 门户自助操作（key.*）按 v2.0 §4.9.8 归属不写 admin_audit（仅管理员写操作入管理审计）。
	group, ok := keyGroupOf(c)
	if !ok {
		response.FailDef(c, errs.Internal)
		return
	}
	recordKeyEvent(c, authevent.EventKeyIssue, k.ID, map[string]any{"kind": k.Kind})
	response.OK(c, gin.H{"key": toKeyItem(&k, group, appRef{}), "plaintext": plain})
}

// UpdateKey 处理 PUT /api/portal/keys/{id}：整体替换可编辑字段（不含密钥本身）。
func UpdateKey(c *gin.Context) {
	k, ok := ownEditableKey(c)
	if !ok {
		return
	}
	req, ok := parseKeyReq(c)
	if !ok {
		return
	}
	updates := map[string]any{
		"name":                   req.Name,
		"amount_limit":           req.AmountLimit,
		"models_allowed":         jsonList(req.ModelsAllowed),
		"modalities_allowed":     jsonList(req.ModalitiesAllowed),
		"allow_ips":              jsonList(req.AllowIPs),
		"rpm_limit":              req.RPMLimit,
		"concurrency_limit":      req.ConcurrencyLimit,
		"allow_user_quota_query": req.allowQuery(),
		"expired_at":             req.expiredAt(),
	}
	if err := db.DB.Model(&model.APIKey{}).Where("id = ?", k.ID).Updates(updates).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, nil)
}

// UpdateKeyStatus 处理 PATCH /api/portal/keys/{id}/status（启用/禁用）。
func UpdateKeyStatus(c *gin.Context) {
	k, ok := ownEditableKey(c)
	if !ok {
		return
	}
	var req struct {
		Status *int `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Status == nil || (*req.Status != 1 && *req.Status != 2) {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if err := db.DB.Model(&model.APIKey{}).Where("id = ?", k.ID).Update("status", *req.Status).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 只有"禁用"才是吊销；重新启用记签发（否则审计里会出现一条没有对应吊销的启用）。
	if *req.Status == int(apikey.KeyStatusDisabled) {
		recordKeyEvent(c, authevent.EventKeyRevoke, k.ID, map[string]any{"reason": "disabled"})
	} else {
		recordKeyEvent(c, authevent.EventKeyIssue, k.ID, map[string]any{"reason": "enabled"})
	}
	response.OK(c, nil)
}

// ResetKey 处理 POST /api/portal/keys/{id}/reset：旧密钥立即失效，返回新明文。
func ResetKey(c *gin.Context) {
	k, ok := ownEditableKey(c)
	if !ok {
		return
	}
	plain, cipher, hash, prefix, fail := newKeyMaterial()
	if fail != nil {
		response.FailDef(c, *fail)
		return
	}
	now := time.Now()
	if err := db.DB.Model(&model.APIKey{}).Where("id = ?", k.ID).Updates(map[string]any{
		"key_cipher": cipher, "key_hash": hash, "key_prefix": prefix, "reset_secret_at": now,
	}).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	recordKeyEvent(c, authevent.EventKeyIssue, k.ID, map[string]any{"reset": true})
	response.OK(c, gin.H{"plaintext": plain})
}

// DeleteKey 处理 DELETE /api/portal/keys/{id}（软删除）。
func DeleteKey(c *gin.Context) {
	k, ok := ownEditableKey(c)
	if !ok {
		return
	}
	if err := db.DB.Delete(&model.APIKey{}, k.ID).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	recordKeyEvent(c, authevent.EventKeyRevoke, k.ID, map[string]any{"reason": "deleted"})
	response.OK(c, nil)
}

// RevealKey 处理 GET /api/portal/keys/{id}/plaintext（鉴权后回显）。
// 明文回显属敏感操作：按 v2.0 §4.9.8 / §4.14.1，**每次回显必写** admin_audit(action='key.reveal')（GV4 验收项）。
// 注：key.* 自助动作（create/update/status/reset/delete）不写审计，与本行的 key.reveal 是两回事。
func RevealKey(c *gin.Context) {
	k, ok := ownEditableKey(c)
	if !ok {
		return
	}
	plain, err := apikey.Decrypt(k.KeyCipher)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionKeyReveal, "key", strconv.FormatUint(k.ID, 10), gin.H{"key_prefix": k.KeyPrefix})
	response.OK(c, gin.H{"plaintext": plain})
}

// ownKey 取路径 {id} 且属当前用户的密钥；不存在/越权一律 404（不泄露存在性）。
// ownEditableKey 是 ownKey + 「托管密钥不可在门户操作」的判定。
//
// 为什么必须在门户侧拦（而不是只靠不展示）：托管密钥是**应用侧**持有的凭证。
// 用户在门户点"重置"，应用手上那把立即失效 —— 而应用对此毫无感知，
// 它会在下一个请求上拿到 401，运维只能从"突然全部鉴权失败"开始排查。
// 同理"回显"会把应用凭证明文暴露到用户浏览器里，"删除"会让应用永久断流。
//
// 409 而不是 403：用户的身份与请求都合法，只是这个对象的状态不允许该操作。
func ownEditableKey(c *gin.Context) (*model.APIKey, bool) {
	k, ok := ownKey(c)
	if !ok {
		return nil, false
	}
	if k.IsHosted() {
		response.FailDef(c, errs.HostedKeyManaged)
		return nil, false
	}
	return k, true
}

func ownKey(c *gin.Context) (*model.APIKey, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return nil, false
	}
	var k model.APIKey
	if err := db.DB.Where("id = ? AND user_id = ?", id, c.GetUint64("user_id")).First(&k).Error; err != nil {
		response.FailDef(c, errs.NotFound)
		return nil, false
	}
	return &k, true
}

// parseKeyReq 解析并校验请求体；失败时已写响应。
func parseKeyReq(c *gin.Context) (keyReq, bool) {
	var req keyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return req, false
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 30 {
		response.FailDef(c, errs.InvalidParam)
		return req, false
	}
	if strings.TrimSpace(req.Group) != "" {
		// 见 keyReq.Group：分组是账号属性。给一句能照着改的文案，
		// 而不是让接入方去猜"我传了但没生效"。
		response.FailDef(c, errs.InvalidParam.WithMessage("group 由账号决定，不能在密钥上设置"))
		return req, false
	}
	if req.AmountLimit < 0 || req.RPMLimit < 0 || req.ConcurrencyLimit < 0 {
		response.FailDef(c, errs.InvalidParam)
		return req, false
	}
	for _, m := range req.ModalitiesAllowed {
		if !allowedModalities[m] {
			response.FailDef(c, errs.InvalidParam)
			return req, false
		}
	}
	for _, ip := range req.AllowIPs {
		if !ipCIRDPattern.MatchString(ip) {
			response.FailDef(c, errs.InvalidParam)
			return req, false
		}
	}
	if _, err := parseExpiredAt(req.ExpiredAt); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return req, false
	}
	return req, true
}

func (r keyReq) allowQuery() bool { return r.AllowUserQuotaQuery == nil || *r.AllowUserQuotaQuery }

func (r keyReq) expiredAt() *time.Time {
	t, err := parseExpiredAt(r.ExpiredAt)
	if err != nil {
		return nil
	}
	return t
}

// parseExpiredAt 解析 "YYYY-MM-DD"，落到该日 23:59:59 UTC；空串/缺省视为不过期。
func parseExpiredAt(s *string) (*time.Time, error) {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", strings.TrimSpace(*s))
	if err != nil {
		return nil, err
	}
	end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
	return &end, nil
}

// jsonList 把字符串切片序列化为 JSONB 列（空切片落 NULL，避免存空数组语义歧义）。
func jsonList(list []string) model.JSON {
	if len(list) == 0 {
		return nil
	}
	j, err := model.NewJSON(list)
	if err != nil {
		return nil
	}
	return j
}

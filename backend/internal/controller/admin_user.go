package controller

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/auth"
	"lingdong-api/internal/service/authevent"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/order"
)

// adminUserSortCols 是 GET /api/admin/users 的排序白名单：**参数名 → 实际列名**。
//
// 必须是映射而不是 `map[string]bool`：排序值直接拼进 ORDER BY，裸写参数名等于把
// 外部输入当标识符。`group` 是 PostgreSQL 保留字（与 `:63` 的过滤分支同因），
// 不加引号就是 42601 → 整个列表 500；`created_at` 之类的普通列这里也统一走映射，
// 避免以后新增列时又出现「过滤加了引号、排序忘了加」的半修。
var adminUserSortCols = map[string]string{
	"id":            "id",
	"created_at":    "created_at",
	"username":      "username",
	"balance":       "balance",
	"used":          "used",
	"request_count": "request_count",
	"status":        "status",
	"group":         `"group"`,
}

// adminUserOrder 翻译排序参数（缺省 created_at DESC，与列表默认顺序一致）。
func adminUserOrder(field string, desc bool) (string, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return "created_at DESC", nil
	}
	col, ok := adminUserSortCols[field]
	if !ok {
		return "", fmt.Errorf("unsupported sort: %s", field)
	}
	if desc {
		return col + " DESC", nil
	}
	return col + " ASC", nil
}

type adminUserItem struct {
	ID           uint64 `json:"id"`
	Username     string `json:"username"`
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	Group        string `json:"group"`
	Status       int    `json:"status"`
	Role         int    `json:"role"`
	Balance      int64  `json:"balance"`
	Used         int64  `json:"used"`
	RequestCount int64  `json:"request_count"`
	KeyCount     int64  `json:"key_count"`
	// BalanceLimit 是**用户级**余额上限原值（0 = 未单独设置，跟随全局配置）。
	// 生效值必须用 conf.BalanceLimitFor 现算：把「存储值」当「生效值」下发，
	// 管理台会在用户跟随全局时显示「上限 0」，运营据此判定该用户不能充值。
	BalanceLimit int64     `json:"balance_limit"`
	CreatedAt    time.Time `json:"created_at"`
}

// AdminListUsers 处理 GET /api/admin/users（T21 分页 + keyword/group/status 过滤）。
func AdminListUsers(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	q := db.DB.Model(&model.User{})
	if kw := c.Query("keyword"); kw != "" {
		q = q.Where("username ILIKE ?", "%"+kw+"%")
	}
	if g := c.Query("group"); g != "" {
		// group 是 PostgreSQL 保留字：裸写 `group = ?` 会 42601，整个用户列表直接 500。
		q = q.Where("\"group\" = ?", g)
	}
	if s := c.Query("status"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			response.FailDef(c, errs.InvalidParam)
			return
		}
		q = q.Where("status = ?", n)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}

	order, oerr := adminUserOrder(p.Field, p.Desc)
	if oerr != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}

	var users []model.User
	// 列表不展示头像（管理台坚持「只有自己的头像」），一次 20 行不背几 KB × 20 的 base64。
	if err := q.Omit("avatar").Order(order).Offset(p.Offset()).Limit(p.Limit()).Find(&users).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}

	counts := make(map[uint64]int64, len(users))
	if len(users) > 0 {
		ids := make([]uint64, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		var rows []struct {
			UserID uint64
			Cnt    int64
		}
		if err := db.DB.Model(&model.APIKey{}).Select("user_id, count(*) AS cnt").
			Where("user_id IN ?", ids).Group("user_id").Scan(&rows).Error; err != nil {
			response.FailDef(c, errs.Internal)
			return
		}
		for _, r := range rows {
			counts[r.UserID] = r.Cnt
		}
	}

	items := make([]adminUserItem, 0, len(users))
	for _, u := range users {
		items = append(items, adminUserItem{
			ID:           u.ID,
			Username:     u.Username,
			Email:        u.Email,
			Phone:        u.Phone,
			Group:        u.Group,
			Status:       int(u.Status),
			Role:         int(u.Role),
			Balance:      u.Balance,
			Used:         u.Used,
			RequestCount: u.RequestCount,
			KeyCount:     counts[u.ID],
			BalanceLimit: u.BalanceLimit,
			CreatedAt:    u.CreatedAt,
		})
	}
	response.OK(c, pagination.New(items, total, p))
}

// AdminAdjustQuota 处理 POST /api/admin/users/{id}/quota（人工调账）。
func AdminAdjustQuota(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req struct {
		// Amount 是调账金额（微，可正可负）。用金额而不是"额度"：
		// 库里余额就是金额，调多少就是多少，不需要第二套单位。
		Amount int64  `json:"amount"`
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Reason) == "" {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 0 元调账：不改余额，却会在流水里留一条 0 金额记录（对账时无法与"真的调过账"区分）。
	if req.Amount == 0 {
		response.FailDef(c, errs.InvalidParam.WithMessage("调账金额不能为 0"))
		return
	}

	if err := billing.New(db.DB).Adjust(c.Request.Context(), id, req.Amount, req.Reason); err != nil {
		switch {
		case errors.Is(err, billing.ErrNegativeBalance):
			response.FailDefMsg(c, errs.InvalidParam, "调账后余额不能为负")
		// 下面两条原先落进 default → 500 internal_error：
		// 运营填了一个越界金额（如 2^63-1），看到的却是"服务器内部错误"，
		// 会去查服务状态而不是改输入。两者都是**调用方能自己改对**的 400。
		case errors.Is(err, billing.ErrAmountTooLarge):
			response.FailDef(c, errs.AmountTooLarge)
		case errors.Is(err, billing.ErrBalanceLimitExceeded):
			response.FailDef(c, errs.BalanceLimitExceeded)
		case errors.Is(err, gorm.ErrRecordNotFound):
			response.FailDef(c, errs.NotFound)
		default:
			response.FailDef(c, errs.Internal)
		}
		return
	}

	var u model.User
	if err := db.DB.First(&u, id).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	detail := gin.H{"amount": req.Amount, "reason": req.Reason, "balance_after": u.Balance}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionUserQuotaAdjust, "user", strconv.FormatUint(id, 10), detail)
	response.OK(c, gin.H{"balance": u.Balance})
}

// AdminUpdateUser 处理 PATCH /api/admin/users/{id}（修改 group/status/role）。
func AdminUpdateUser(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req struct {
		Group  *string `json:"group"`
		Status *int    `json:"status"`
		Role   *int    `json:"role"`
		// BalanceLimit 是用户级余额上限（0 = 清空覆盖、跟随全局配置）。
		//
		// 为什么必须能改：这个列是「个别用户加严/放宽」的唯一出口，
		// 而 order/redeem/billing 三处入账都按「用户级 > 0 优先，否则回落全局」判定。
		// 没有写入口时它永远是 0，运营想给单个大客户放宽只能改全局，
		// 而改全局等于对所有用户放宽 —— 闸门形同虚设。
		BalanceLimit *int64 `json:"balance_limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}

	var u model.User
	if err := db.DB.First(&u, id).Error; err != nil {
		response.FailDef(c, errs.NotFound)
		return
	}

	updates := map[string]any{}
	detail := map[string]any{}
	if req.Group != nil {
		if *req.Group == "" {
			response.FailDef(c, errs.InvalidParam)
			return
		}
		updates["group"] = *req.Group
		detail["group"] = gin.H{"old": u.Group, "new": *req.Group}
	}
	if req.Status != nil {
		if *req.Status != 1 && *req.Status != 2 {
			response.FailDef(c, errs.InvalidParam)
			return
		}
		updates["status"] = *req.Status
		detail["status"] = gin.H{"old": int(u.Status), "new": *req.Status}
	}
	if req.Role != nil {
		if *req.Role != 0 && *req.Role != 1 {
			response.FailDef(c, errs.InvalidParam)
			return
		}
		updates["role"] = *req.Role
		detail["role"] = gin.H{"old": int(u.Role), "new": *req.Role}
	}
	if req.BalanceLimit != nil {
		// 负数没有语义（上限不能为负），0 表示「清空覆盖、跟随全局」——
		// 这两者必须区分，所以用指针接收而不是「0 视为未传」。
		if *req.BalanceLimit < 0 {
			response.FailDef(c, errs.InvalidParam)
			return
		}
		updates["balance_limit"] = *req.BalanceLimit
		detail["balance_limit"] = gin.H{"old": u.BalanceLimit, "new": *req.BalanceLimit}
	}
	// 角色/状态实际变更属权限敏感操作：撤销该用户全部会话，使其旧凭证立即失效。
	// （管理面判权已读 DB 实时角色，但 UI 与应用态需要重新登录才能一致。）
	// 同值提交不撤销（不做无意义登出）。
	revokeSessions := (req.Role != nil && *req.Role != int(u.Role)) ||
		(req.Status != nil && *req.Status != int(u.Status))
	if revokeSessions {
		detail["session_revoked"] = true
	}
	if len(updates) == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}

	// 最后管理员保护：把自己（或唯一的管理员）降级/禁用后，管理台将永久无人可进。
	// 必须在事务内带行锁判定：两个管理员同时互相降级时，双方都会看到"对方还在"，
	// 结果全部降级 —— 这类竞态只有在同一事务里锁住行才排得掉。
	demoteAdmin := int(u.Role) == 0 && ((req.Role != nil && *req.Role != 0) ||
		(req.Status != nil && *req.Status == 2))
	err = db.DB.Transaction(func(tx *gorm.DB) error {
		if demoteAdmin {
			// 锁“全部在任管理员”而不是“除我之外的管理员”：
			//
			// ① 不能用 `Count` + `FOR UPDATE` —— PostgreSQL 明确禁止
			//    `SELECT count(*) ... FOR UPDATE`，报错会让整个操作 500。
			// ② 必须按固定顺序（id）加锁：两个管理员同时互相降级时，
			//    “锁 A 再查 B” 与 “锁 B 再查 A” 会互相等待成死锁；
			//    统一 ORDER BY id 后两者抢同一把行锁，串行通过。
			var adminIDs []uint64
			if err := tx.Model(&model.User{}).
				Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("role = 0 AND status = 1 AND deleted_at IS NULL").
				Order("id").
				Pluck("id", &adminIDs).Error; err != nil {
				return err
			}
			others := 0
			for _, aid := range adminIDs {
				if aid != id {
					others++
				}
			}
			if others == 0 {
				return errs.LastAdminProtected
			}
		}
		if err := tx.Model(&model.User{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		if revokeSessions {
			return tx.Model(&model.UserSession{}).
				Where("uid = ? AND revoked_at IS NULL", id).
				Updates(map[string]any{
					"revoked_at":    time.Now(),
					"revoke_reason": model.SessionRevokeAdminRevoked,
				}).Error
		}
		return nil
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	// 动作按实际变更域收敛（role > status > group），与设计 §4.9.8 的 user.role/user.status/user.update 对齐。
	action := audit.ActionUserUpdate
	switch {
	case req.Role != nil && *req.Role != int(u.Role):
		action = audit.ActionUserRole
	case req.Status != nil && *req.Status != int(u.Status):
		action = audit.ActionUserStatus
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), action, "user", strconv.FormatUint(id, 10), detail)
	response.OK(c, nil)
}

// AdminResetUserPassword 处理 POST /api/admin/users/{id}/password-reset。
//
// 管理员重置密码是「救人」通道（用户邮箱不可用、无法自助找回），
// 但它同时是一次完整的账号接管：因此必须
//   - 用与自助注册同一套强度规则（唯一来源 auth.ValidatePassword）；
//   - 在同一事务里撤销该用户全部会话；
//   - 审计只记 target_uid，**绝不记密码**。
func AdminResetUserPassword(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req struct {
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var target model.User
	if err := db.DB.First(&target, id).Error; err != nil {
		response.FailDef(c, errs.NotFound)
		return
	}
	if err := auth.New(db.DB).AdminResetPassword(c.Request.Context(), id, req.NewPassword); err != nil {
		respondErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionUserPasswordReset,
		"user", strconv.FormatUint(id, 10), map[string]any{"target_uid": id})
	authevent.Record(c.Request.Context(), authevent.Event{
		UID: id, Account: target.Username, Event: authevent.EventPasswordResetAdmin,
		Result: authevent.ResultSuccess, IP: c.ClientIP(),
		Detail: map[string]any{"admin_id": c.GetUint64("user_id")},
	})
	response.OK(c, nil)
}

// AdminUserTransactions 处理 GET /api/admin/users/{id}/transactions：按用户查资金流水（只读）。
// 与门户 /api/portal/transactions 复用同一 service 实现，口径一致。
func AdminUserTransactions(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 管理台流水页不对外声明 sort（apicontract 里没有这条端点），传空串走 service 默认顺序。
	items, total, err := order.New(db.DB).Transactions(c.Request.Context(), id,
		strings.TrimSpace(c.Query("type")), p.Offset(), p.Limit(), "")
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, pagination.New(items, total, p))
}

// adminUserKeyItem 是管理台查看用户密钥的视图：仅展示索引前缀，**永不含明文/密文**。
type adminUserKeyItem struct {
	ID                uint64     `json:"id"`
	KeyPrefix         string     `json:"key_prefix"`
	Name              string     `json:"name"`
	Group             string     `json:"group"`
	Status            int        `json:"status"`
	AmountLimit       int64      `json:"amount_limit"`
	Used              int64      `json:"used"`
	ModelsAllowed     []string   `json:"models_allowed"`
	ModalitiesAllowed []string   `json:"modalities_allowed"`
	RPMLimit          int        `json:"rpm_limit"`
	ConcurrencyLimit  int        `json:"concurrency_limit"`
	ExpiredAt         *time.Time `json:"expired_at"`
	CreatedAt         time.Time  `json:"created_at"`
}

// AdminUserKeys 处理 GET /api/admin/users/{id}/keys：按用户查密钥（只读，禁回显明文）。
func AdminUserKeys(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var keys []model.APIKey
	if err := db.DB.Where("user_id = ?", id).Order("id DESC").Find(&keys).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	items := make([]adminUserKeyItem, 0, len(keys))
	for i := range keys {
		k := &keys[i]
		items = append(items, adminUserKeyItem{
			ID: k.ID, KeyPrefix: k.KeyPrefix, Name: k.Name, Group: k.Group, Status: int(k.Status),
			AmountLimit: k.AmountLimit, Used: k.Used,
			ModelsAllowed:     apikey.DecodeStrings(k.ModelsAllowed),
			ModalitiesAllowed: apikey.DecodeStrings(k.ModalitiesAllowed),
			RPMLimit:          k.RPMLimit, ConcurrencyLimit: k.ConcurrencyLimit,
			ExpiredAt: k.ExpiredAt, CreatedAt: k.CreatedAt,
		})
	}
	response.OK(c, gin.H{"items": items})
}

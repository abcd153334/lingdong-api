// Package app 是「接入应用」的业务语义：品牌、公告、版本、维护与概览。
//
// 边界（改之前先读）：应用**不是租户**。
//   - 模型/渠道/价目/套餐全平台一套；
//   - 按应用隔离的只有 api_keys.app_id、orders.app_id，以及由 key 派生的
//     usages/tasks/files（归因链 key_id -> api_keys.app_id）。
//
// 所以本包只做"应用自己的配置与统计"，不碰选路、限额与计费。
package app

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
)

// 领域哨兵。transport 层用 DefOr 映射成对外错误码，
// 避免每个 controller 各写一份 switch。
var (
	ErrNotFound        = errors.New("app: 应用不存在")
	ErrNoticeNotFound  = errors.New("app: 公告不存在")
	ErrReleaseNotFound = errors.New("app: 版本不存在")
	// ErrVersionExists 与 ErrReleaseState 分开：前者是"号码被占"（换个号），
	// 后者是"顺序错了"（先撤回/先改状态），处置动作不同。
	ErrVersionExists = errors.New("app: 版本号已存在")
	ErrReleaseState  = errors.New("app: 版本状态不允许该操作")
)

// DefOr 把领域哨兵映射成对外错误定义；未知错误交给调用方兜底（通常是 Internal）。
func DefOr(err error, fallback errs.Def) errs.Def {
	switch {
	case errors.Is(err, ErrNotFound):
		return errs.AppNotFound
	case errors.Is(err, ErrNoticeNotFound):
		return errs.AppNoticeNotFound
	case errors.Is(err, ErrReleaseNotFound):
		return errs.AppReleaseNotFound
	case errors.Is(err, ErrVersionExists):
		return errs.AppReleaseVersionExists
	case errors.Is(err, ErrReleaseState):
		return errs.AppReleaseStateInvalid
	}
	return fallback
}

// Get 按 ID 取应用。不返回 gorm.ErrRecordNotFound：那会让每个 handler
// 都要先判一次 gorm 的哨兵，最终总有人漏掉，表现为 500。
func Get(ctx context.Context, gdb *gorm.DB, id uint64) (*model.App, error) {
	return findOne(ctx, gdb, "id = ?", id)
}

// findOne 是"按唯一列取一行"的唯一实现。
//
// 此前 Get 与 BySlug 逐行同构（只差列名与入参类型），两份代码的下一步
// 一定是分叉 —— 一边补了过滤条件另一边没补。where 只接受调用方的常量字面量，
// 不拼任何外部输入。
func findOne(ctx context.Context, gdb *gorm.DB, where string, arg any) (*model.App, error) {
	var a model.App
	if err := gdb.WithContext(ctx).Where(where, arg).Limit(1).Find(&a).Error; err != nil {
		return nil, err
	}
	if a.ID == 0 {
		return nil, ErrNotFound
	}
	return &a, nil
}

// Stats 是应用维度的聚合口径。
type Stats struct {
	// KeyCount / ActiveKeyCount 只数托管密钥（kind='hosted'）。
	KeyCount       int64
	ActiveKeyCount int64
	// UserCount 是持有该应用托管密钥的**去重用户数**。
	UserCount int64
	// Used 是累计消费（**微**），口径 = SUM(api_keys.used)。
	//
	// 不用 usages 求和：api_keys.used 是同一份数字，KEY 级金额限额与用户看到的
	// "已用"都读它；另算一份会出现"账户页 100、后台 98"这种无法解释的差。
	Used int64
	// PaidCent 是已支付订单总额（**分**）——订单本身以分计价，转微会让运营
	// 对不上支付流水。
	PaidCent  int64
	PaidCount int64
}

// StatsByApps 批量取多个应用的聚合（列表页一次查询，禁止逐行 N+1）。
func StatsByApps(ctx context.Context, gdb *gorm.DB, appIDs []uint64) (map[uint64]Stats, error) {
	out := make(map[uint64]Stats, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	type keyRow struct {
		AppID  uint64
		Cnt    int64
		Active int64
		Users  int64
		Used   int64
	}
	var keys []keyRow
	if err := gdb.WithContext(ctx).
		Table("api_keys").
		Select(`app_id,
			COUNT(*) AS cnt,
			SUM(CASE WHEN status = 1 THEN 1 ELSE 0 END) AS active,
			COUNT(DISTINCT user_id) AS users,
			COALESCE(SUM(used), 0) AS used`).
		Where("app_id IN ? AND kind = ? AND deleted_at IS NULL", appIDs, model.KeyKindHosted).
		Group("app_id").Scan(&keys).Error; err != nil {
		return nil, err
	}
	for _, r := range keys {
		s := out[r.AppID]
		s.KeyCount, s.ActiveKeyCount, s.UserCount, s.Used = r.Cnt, r.Active, r.Users, r.Used
		out[r.AppID] = s
	}
	type orderRow struct {
		AppID    uint64
		PaidCent int64
		Cnt      int64
	}
	var orders []orderRow
	if err := gdb.WithContext(ctx).
		Table("orders").
		Select("app_id, COALESCE(SUM(amount_cent), 0) AS paid_cent, COUNT(*) AS cnt").
		Where("app_id IN ? AND status = ? AND deleted_at IS NULL", appIDs, model.OrderPaid).
		Group("app_id").Scan(&orders).Error; err != nil {
		return nil, err
	}
	for _, r := range orders {
		s := out[r.AppID]
		s.PaidCent, s.PaidCount = r.PaidCent, r.Cnt
		out[r.AppID] = s
	}
	return out, nil
}

// ---- 校验工具 ----

// maxRunes 按**字符**而不是字节计长：中文在 varchar(n) 里按字符算，
// 用 len() 会让"64 个汉字"被误判成超长（UTF-8 下是 192 字节）。
func maxRunes(s string, n int) bool { return utf8.RuneCountInString(s) <= n }

// validHTTPURL 只接受 http(s) 绝对地址。
//
// 不接受相对路径：这些 URL 会下发给终端应用，终端的 base 与平台不同源，
// 相对地址必然 404；也不接受 javascript:/data: —— 它们会被直接渲染。
func validHTTPURL(s string) bool {
	if s == "" {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}

// AccessModeOf 把 allow_self_issue 派生成对外可读的枚举。
//
// 为什么不新增一列 access_mode：派生值一旦落库就会与源头漂移
// （改了开关忘了改列），而这里只有一个源头。
func AccessModeOf(allowSelf bool) string {
	if allowSelf {
		return "self_service"
	}
	return "approval"
}

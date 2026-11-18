package app

// 公开面（免凭证）的三个返回体与组装函数。
//
// 设计边界（改之前先读）：这是唯一不必持有任何凭证就能读到应用信息的面。
// slug 是人类可读串、可枚举，所以返回体必须是**白名单**，只含终端用户本来
// 就该看到的东西：品牌、有效公告、最近发布、维护状态。
// **不得**出现用户数、消费、密钥数、应用启用状态、在线状态或任何错误原文 ——
// 那些属于经营/拓扑数据。
//
// 三条端点而不是一条聚合，判据是「变更频率 × 鉴权等级 × 缓存策略」三者是否一致；
// 理由写在 controller/app_public.go 的包注释里。

import (
	"context"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// ProfileView 是 GET /api/apps/{slug}/profile 的返回体。
//
// 品牌与维护态合在一条是**正确聚合**：同出 apps 一行、同属人工运营动作、
// 同一缓存档，且终端冷启动时两样都要。
type ProfileView struct {
	Slug        string          `json:"slug"`
	Brand       BrandView       `json:"brand"`
	Maintenance MaintenanceView `json:"maintenance"`
}

// MaintenanceView 是对外的维护状态。
//
// 判定统一走 MaintenanceOf（与 /v1 的 503 闸共用同一份 10 秒快照）：
// 两处各算一份的后果是"终端提示一切正常，调用却一直 503"。
type MaintenanceView struct {
	Enabled bool   `json:"enabled"`
	Message string `json:"message"`
}

// NoticesData 是 GET /api/apps/{slug}/notices 的返回体。
//
// 恒为数组（无生效公告时是 `[]` 而不是 `null`）：终端直接 .map 不会炸。
type NoticesData struct {
	Notices []NoticeView `json:"notices"`
}

// ReleaseData 是 GET /api/apps/{slug}/release 的返回体。
//
// 无已发布版本时 Release 为 null，终端据此隐藏"检查更新"。
type ReleaseData struct {
	Release *ReleaseView `json:"release"`
}

// 公开接口一次最多下发多少条公告。取 20 而不是全量：
// 公告是"最近通知"不是档案；无限下发会让终端首屏渲染被历史文案拖垮。
const publicNoticeLimit = 20

// ProfileOf 组装 profile。logoURL 由 controller 用 iconref 派生后传入
// （本包不依赖 gin，也不该知道站点基址从哪来）。
func ProfileOf(a *model.App, logoURL, maintMsg string, maintEnabled bool) ProfileView {
	return ProfileView{
		Slug:        a.Slug,
		Brand:       BrandOf(a, logoURL),
		Maintenance: MaintenanceView{Enabled: maintEnabled, Message: maintMsg},
	}
}

// NoticesOf 取"此刻应当下发"的公告。
//
// 每次实时查库、不做缓存：判据是精确时间窗（EffectiveNotices），
// 缓存会把 `now` 冻住，表现为"到点不下发"，而这是一条运营会立刻发现、
// 开发很难复现的问题。一条走索引的点查不值得为它引入这个偏差。
func NoticesOf(ctx context.Context, gdb *gorm.DB, appID uint64, now time.Time) (NoticesData, error) {
	rows, err := EffectiveNotices(ctx, gdb, appID, now, publicNoticeLimit)
	if err != nil {
		return NoticesData{}, err
	}
	return NoticesData{Notices: ViewsOf(rows)}, nil
}

// ReleaseOf 取最近一次**发布**的版本（按 published_at，不按 semver）。
func ReleaseOf(ctx context.Context, gdb *gorm.DB, appID uint64) (ReleaseData, error) {
	row, err := LatestPublishedOf(ctx, gdb, appID)
	if err != nil {
		return ReleaseData{}, err
	}
	return ReleaseData{Release: ViewOf(row)}, nil
}

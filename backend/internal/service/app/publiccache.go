package app

// 公开面的应用行快照。
//
// 为什么需要它：公开面三条端点（profile / notices / release）每次请求都要先按
// slug 解析出应用。这是固定开销，而这条面是**匿名**的 —— 终端集群在开机高峰
// 会同时打进来，同一张表的重复点查没有任何收益。
//
// 为什么是「整表快照」而不是「按 slug 缓存 + 按 id 失效」：应用数量是平台拓扑
// （个位数到几十），一次 SELECT 能全部装下；整表的失效不需要记 appID、不需要
// 遍历删除，`InvalidateAppPublic()` 无参即全量。少一层 "slug → appID" 的映射，
// 就少一处能写错的地方（漏了它，失效会退化成空操作，表现为"改了品牌不生效"）。
//
// 为什么缓存行而不缓存序列化后的字节：`logo_url` 由 iconref 按**请求站点**派生，
// 缓存字节会把第一个请求的域名烧进去，多域名部署直接串。
//
// TTL / 双检锁 / 失败沿用旧值 / 失败退避都在 pkg/snapshot；本文件只剩
// 「查什么」与「拿不到时算 500 还是 404」。

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/snapshot"
)

// publicAppsTTL 与维护态快照取同一个数量级：品牌改动是人工动作，
// 10 秒的传播窗口足够，而它把匿名请求的数据库开销降到 0。
const publicAppsTTL = 10 * time.Second

// publicAppsLimit 防止把整张表读进内存（与 maintenance.go 同一考量）。
//
// 超限必须按 id 升序（结果确定、可复现）：不排序时被挤出的应用会稳定 404，
// 而终端无法把它与"应用被删了"区分开。
const publicAppsLimit = 1000

// errPublicSnapshot 表示快照不可用（加载失败且没有任何旧值）。
//
// 必须与 ErrNotFound 区分：前者是 500（稍后重试），后者是 404（换个 slug）。
// 合成一个的后果是"数据库抖动时所有终端都以为应用被删了"。
var errPublicSnapshot = errors.New("app: 公开面快照不可用")

// loadPublicApps 是可替换的加载器（单测用它注入失败与调用次数）。
var loadPublicApps = func(ctx context.Context) (map[string]*model.App, error) {
	rows := make([]model.App, 0, 32)
	if err := db.DB.WithContext(ctx).
		Order("id").
		Limit(publicAppsLimit).Find(&rows).Error; err != nil {
		return nil, err
	}
	// 超限是静默的可用性事故（被挤出的应用对外"消失"），必须留痕。
	if len(rows) >= publicAppsLimit {
		logger.L().Error("公开面应用快照已达上限，超出的应用将无法通过 slug 解析",
			zap.Int("limit", publicAppsLimit))
	}
	out := make(map[string]*model.App, len(rows))
	for i := range rows {
		out[rows[i].Slug] = &rows[i]
	}
	return out, nil
}

var publicSnap = snapshot.New("app_public", publicAppsTTL, func(ctx context.Context) (map[string]*model.App, error) {
	return loadPublicApps(ctx)
})

// AppBySlug 按标识取应用（公开面专用）。
//
// 失败策略（与维护态**相反**，见 pkg/snapshot 的说明）：
//   - 有旧快照 ⇒ 沿用。宁可多给 10 秒旧品牌，也不要因为一次抖动让所有终端
//     拿不到品牌；
//   - 无快照且加载失败 ⇒ 返回错误，由 handler 回 500。**不能**编造一个空应用：
//     那会把"服务挂了"显示成"这个应用还没配置品牌"，是最难排查的一类故障。
func AppBySlug(ctx context.Context, slug string) (*model.App, error) {
	cache, err := publicSnap.Get(ctx)
	if err != nil {
		return nil, errPublicSnapshot
	}
	a, ok := cache[slug]
	if !ok {
		return nil, ErrNotFound
	}
	return a, nil
}

// InvalidateAppPublic 让本实例立刻重新加载。
//
// 多实例不需要广播：其余实例最多 publicAppsTTL 后自然收敛，而"改了品牌要几秒
// 才生效"本来就受客户端缓存（profile 的 max-age=60）支配。
func InvalidateAppPublic() { publicSnap.Invalidate() }

// ResetPublicCacheForTest 清空缓存（仅供测试）。
func ResetPublicCacheForTest() { publicSnap.ResetForTest() }

package app

import (
	"context"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pgerr"
)

// semverPattern 只做**格式**校验，不解析、不比较大小。
//
// 为什么不比较：hotfix 回填（发 1.1.9 去补 1.2.0 的漏洞）是合法操作，
// 按版本号取最大会让线上静默降级；"最近发布"一律按 published_at 取。
var semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)

// ReleaseInput 是版本的写入形态。
type ReleaseInput struct {
	Version      string
	ReleaseNotes string
	DownloadURL  string
	ForceUpdate  bool
}

// NormalizeRelease 去空白并校验。versionReadOnly=true 时不校验版本号格式
// （已发布的版本不允许改号，入参里的 version 会被忽略）。
func NormalizeRelease(in ReleaseInput) (ReleaseInput, string) {
	in.Version = strings.TrimSpace(in.Version)
	in.ReleaseNotes = strings.TrimSpace(in.ReleaseNotes)
	in.DownloadURL = strings.TrimSpace(in.DownloadURL)

	switch {
	case in.Version == "":
		return in, "版本号必填"
	case !semverPattern.MatchString(in.Version):
		return in, "版本号必须形如 1.2.3（可带 -beta.1 后缀）"
	case !maxRunes(in.Version, 32):
		return in, "版本号不超过 32 个字符"
	case in.ReleaseNotes == "":
		return in, "更新说明必填"
	case !maxRunes(in.ReleaseNotes, 4000):
		return in, "更新说明不超过 4000 个字符"
	case in.DownloadURL == "":
		return in, "下载地址必填"
	case !maxRunes(in.DownloadURL, 1024):
		return in, "下载地址不超过 1024 个字符"
	case !validHTTPURL(in.DownloadURL):
		return in, "下载地址必须是 http(s) 地址"
	}
	return in, ""
}

// ListReleases 分页列出某应用的版本（含草稿与已撤回），按 id 倒序。
func ListReleases(ctx context.Context, gdb *gorm.DB, appID uint64, status int16, offset, limit int) ([]model.AppRelease, int64, error) {
	q := gdb.WithContext(ctx).Model(&model.AppRelease{}).Where("app_id = ?", appID)
	if status != 0 {
		q = q.Where("status = ?", status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	rows := make([]model.AppRelease, 0, limit)
	if err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// CreateRelease 新建版本，**一律是草稿**：先建后发，避免"填错地址直接推给全量"。
func CreateRelease(ctx context.Context, gdb *gorm.DB, appID uint64, in ReleaseInput) (*model.AppRelease, error) {
	// 先查一次再建：唯一索引才是并发下的硬保证（见 db/indexes.go 的
	// uq_app_releases_version），但没有这条预检时，"填了个已存在的版本号"
	// 会走 23505 → 需要 pgerr 才认得出来；预检让常规路径立刻给出可读错误，
	// 索引则兜住并发。
	var dup int64
	if err := gdb.WithContext(ctx).Model(&model.AppRelease{}).
		Where("app_id = ? AND version = ?", appID, in.Version).Count(&dup).Error; err != nil {
		return nil, err
	}
	if dup > 0 {
		return nil, ErrVersionExists
	}
	row := model.AppRelease{
		AppID: appID, Version: in.Version, ReleaseNotes: in.ReleaseNotes,
		DownloadURL: in.DownloadURL, ForceUpdate: in.ForceUpdate,
		Status: model.AppReleaseDraft,
	}
	if err := gdb.WithContext(ctx).Create(&row).Error; err != nil {
		if pgerr.IsUnique(err) {
			// 版本号在应用内唯一（uq_app_releases_version）。
			return nil, ErrVersionExists
		}
		return nil, err
	}
	return &row, nil
}

// UpdateRelease 修改版本。
//
// 已发布/已撤回的版本**不允许改版本号**：对外发过的号一旦改掉，
// 终端上报的"我装的是 2.3.0"与库里的记录就对不上了。
// 更新说明、下载地址、强制更新仍可改 —— 发错下载链接是真实事故，必须能救。
func UpdateRelease(ctx context.Context, gdb *gorm.DB, appID, id uint64, in ReleaseInput) (*model.AppRelease, error) {
	var row model.AppRelease
	if err := gdb.WithContext(ctx).Where("id = ? AND app_id = ?", id, appID).Limit(1).Find(&row).Error; err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, ErrReleaseNotFound
	}
	version := row.Version
	if row.Status == model.AppReleaseDraft {
		if in.Version != row.Version {
			// 草稿改号要重新走唯一性校验：否则会撞索引变成 500。
			var n int64
			if err := gdb.WithContext(ctx).Model(&model.AppRelease{}).
				Where("app_id = ? AND version = ? AND id <> ?", appID, in.Version, id).Count(&n).Error; err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, ErrVersionExists
			}
			version = in.Version
		}
	} else if in.Version != row.Version {
		return nil, ErrReleaseState
	}

	if err := gdb.WithContext(ctx).Model(&model.AppRelease{}).
		Where("id = ? AND app_id = ?", id, appID).
		Updates(map[string]any{
			"version": version, "release_notes": in.ReleaseNotes,
			"download_url": in.DownloadURL, "force_update": in.ForceUpdate,
		}).Error; err != nil {
		// 并发改号撞唯一索引：预检只能挡住常规路径，索引才是硬保证。
		// 不映射的话这里回 500，而同一个事实在 CreateRelease 里回 409。
		if pgerr.IsUnique(err) {
			return nil, ErrVersionExists
		}
		return nil, err
	}
	row.Version, row.ReleaseNotes, row.DownloadURL, row.ForceUpdate = version, in.ReleaseNotes, in.DownloadURL, in.ForceUpdate
	return &row, nil
}

// SetReleaseStatus 执行发布(2)/撤回(3) 的状态迁移。
//
// 允许：草稿→发布、已撤回→重发、已发布→撤回。
// 拒绝：已发布/已撤回 → 草稿 —— 对外发过的版本不能退回草稿态，
// 那会让"这个版本到底有没有上线过"变成无法回答的问题。
func SetReleaseStatus(ctx context.Context, gdb *gorm.DB, appID, id uint64, next int16) error {
	if next != model.AppReleasePublished && next != model.AppReleaseRevoked {
		return ErrReleaseState
	}
	var row model.AppRelease
	if err := gdb.WithContext(ctx).Where("id = ? AND app_id = ?", id, appID).Limit(1).Find(&row).Error; err != nil {
		return err
	}
	if row.ID == 0 {
		return ErrReleaseNotFound
	}

	now := time.Now()
	// CAS：谓词带**读取时的状态**。不带的话两个并发请求都读到 draft、
	// 都判定合法，后到的那个会覆盖前一个的结果（重复发布 / 发布与撤回交错）。
	switch next {
	case model.AppReleasePublished:
		if row.Status == model.AppReleasePublished {
			return ErrReleaseState
		}
		// 发布与重发都刷新 published_at：对外"最近发布"按它排序。
		res := gdb.WithContext(ctx).Model(&model.AppRelease{}).
			Where("id = ? AND app_id = ? AND status = ?", id, appID, row.Status).
			Updates(map[string]any{"status": model.AppReleasePublished, "published_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrReleaseState
		}
	case model.AppReleaseRevoked:
		if row.Status != model.AppReleasePublished {
			return ErrReleaseState
		}
		// 保留 published_at：审计要能回答"它何时上过线"。
		res := gdb.WithContext(ctx).Model(&model.AppRelease{}).
			Where("id = ? AND app_id = ? AND status = ?", id, appID, model.AppReleasePublished).
			Update("status", model.AppReleaseRevoked)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrReleaseState
		}
	}
	return nil
}

// DeleteRelease 只允许删草稿；返回被删的行（调用方用它写审计，不必再查一次）。
func DeleteRelease(ctx context.Context, gdb *gorm.DB, appID, id uint64) (*model.AppRelease, error) {
	var row model.AppRelease
	if err := gdb.WithContext(ctx).Where("id = ? AND app_id = ?", id, appID).Limit(1).Find(&row).Error; err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, ErrReleaseNotFound
	}
	if row.Status != model.AppReleaseDraft {
		return nil, ErrReleaseState
	}
	// 删除语句再带一次状态谓词：读判与删除之间被并发发布时，无谓词会物理删掉
	// 一个"已经对外发布过"的版本 —— 版本历史是发布记录的唯一依据，不能丢。
	res := gdb.WithContext(ctx).Where("id = ? AND app_id = ? AND status = ?", id, appID, model.AppReleaseDraft).
		Delete(&model.AppRelease{})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrReleaseState
	}
	return &row, nil
}

// LatestPublished 批量取每个应用"最近发布"的版本（列表页用，一次查询）。
//
// DISTINCT ON 是 PG 方言，但项目只支持 PG（其余查询也用了 FILTER / ILIKE）。
func LatestPublished(ctx context.Context, gdb *gorm.DB, appIDs []uint64) (map[uint64]model.AppRelease, error) {
	out := make(map[uint64]model.AppRelease, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	rows := make([]model.AppRelease, 0, len(appIDs))
	// NULLS LAST 必须显式写：PG 的 DESC 默认 NULLS FIRST，一条
	// status=2 且 published_at 为空的脏数据会被当成"最新发布"下发。
	if err := gdb.WithContext(ctx).Raw(
		`SELECT DISTINCT ON (app_id) * FROM app_releases
		 WHERE status = ? AND app_id IN ?
		 ORDER BY app_id, published_at DESC NULLS LAST, id DESC`,
		model.AppReleasePublished, appIDs).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.AppID] = r
	}
	return out, nil
}

// LatestPublishedOf 取单个应用"最近发布"的版本；没有则返回 nil（不是错误）。
func LatestPublishedOf(ctx context.Context, gdb *gorm.DB, appID uint64) (*model.AppRelease, error) {
	var row model.AppRelease
	err := gdb.WithContext(ctx).Where("app_id = ? AND status = ?", appID, model.AppReleasePublished).
		Order("published_at DESC NULLS LAST, id DESC").Limit(1).Find(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, nil
	}
	return &row, nil
}

// ReleaseView 是对外版本视图。
type ReleaseView struct {
	Version      string     `json:"version"`
	ReleaseNotes string     `json:"release_notes"`
	DownloadURL  string     `json:"download_url"`
	ForceUpdate  bool       `json:"force_update"`
	PublishedAt  *time.Time `json:"published_at"`
}

// ViewOf 把版本行投影成对外视图。
func ViewOf(r *model.AppRelease) *ReleaseView {
	if r == nil {
		return nil
	}
	return &ReleaseView{
		Version: r.Version, ReleaseNotes: r.ReleaseNotes, DownloadURL: r.DownloadURL,
		ForceUpdate: r.ForceUpdate, PublishedAt: r.PublishedAt,
	}
}

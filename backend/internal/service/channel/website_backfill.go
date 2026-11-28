package channel

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// websiteBackfillMarker 是「存量渠道官网回填」的一次性标记。
//
// 为什么在 service/channel 而不是 db/migrate.go：回填要读适配器默认官网，
// 而 relay/adapters → service/billing → db；把这段放进 db 会形成导入环。
// 调用点与 MigrateChannelOptions 同级（main.go 的迁移锁内）。
const websiteBackfillMarker = "m65_channels_website_backfill"

// BackfillWebsites 给 website 为空的存量渠道补上「有效官网」。
//
// 三条语义：
//  1. **只填空值**：运营改过的一律不动；
//  2. 软删行也填：恢复出厂会把它们拉回来，行内字段必须已经是完整的；
//  3. 匹配不到规则的行不是错误，逐条 warn（自定义规格、私有域名）——回填失败
//     不该让服务起不来，但必须能看见"哪些渠道没有官网可用"。
func BackfillWebsites(gdb *gorm.DB) error {
	var applied model.DataMigration
	err := gdb.Where("name = ?", websiteBackfillMarker).First(&applied).Error
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return fmt.Errorf("读取官网回填标记失败: %w", err)
	}

	return gdb.Transaction(func(tx *gorm.DB) error {
		var rows []model.Channel
		// Unscoped：软删行也要补（恢复出厂时直接可见）。
		if err := tx.Unscoped().Where("website = ?", "").Find(&rows).Error; err != nil {
			return fmt.Errorf("读取待回填渠道失败: %w", err)
		}
		filled := 0
		var noRule []string
		for i := range rows {
			ch := &rows[i]
			website := WebsiteFor(ch.AdapterID, ch.BaseURL)
			if website == "" {
				noRule = append(noRule, fmt.Sprintf("#%d %s（%s）", ch.ID, ch.Name, ch.AdapterID))
				continue
			}
			// UpdateColumn 而不是 Update：回填不是"运营编辑"，不该刷新 updated_at。
			// Unscoped 让软删行也能被写。
			if err := tx.Unscoped().Model(&model.Channel{}).Where("id = ?", ch.ID).
				UpdateColumn("website", website).Error; err != nil {
				return fmt.Errorf("回填渠道 #%d 官网失败: %w", ch.ID, err)
			}
			filled++
		}
		if err := tx.Create(&model.DataMigration{
			Name:      websiteBackfillMarker,
			AppliedAt: time.Now().UTC(),
		}).Error; err != nil {
			return fmt.Errorf("写入官网回填标记失败: %w", err)
		}
		if len(noRule) > 0 {
			logger.L().Warn("部分渠道没有可用的官网（未回填）",
				zap.Int("fillable", filled),
				zap.Int("no_rule", len(noRule)),
				zap.String("samples", strings.Join(firstN(noRule, 5), "; ")))
		}
		if filled > 0 {
			logger.L().Info("渠道官网回填完成", zap.Int("filled", filled), zap.Int("no_rule", len(noRule)))
		}
		return nil
	})
}

// firstN 截断样本，避免一条日志把启动日志刷满。
func firstN(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/service/billing"
)

// specPriceGuardInterval 是计价配置体检的节拍。
//
// 6 小时而不是每分钟：坏行只可能由迁移脚本或直连 SQL 产生（正常保存路径
// 已被 ValidateSpecPrices 拦住），而每次扫描都要把在售模型全读一遍。
const specPriceGuardInterval = 6 * time.Hour

// SpecPriceGuardJob 周期体检「档位价无法解析」的模型。
//
// 为什么不能只在启动时查一次：坏行入库后，进程往往几个月不重启，于是
// 「这些模型的文档不显示档位、下单命中不到价格」这件事只存在于那次启动日志里。
// 运行期把行数做成指标，才能被看板发现并追到是哪次操作写坏的。
type SpecPriceGuardJob struct{}

// NewSpecPriceGuardJob 构造计价体检任务。
func NewSpecPriceGuardJob() *SpecPriceGuardJob { return &SpecPriceGuardJob{} }

// Name 返回任务标识。
func (*SpecPriceGuardJob) Name() string { return "spec_price_guard" }

// Interval 返回体检节拍。
func (*SpecPriceGuardJob) Interval() time.Duration { return specPriceGuardInterval }

// Run 执行一轮体检：有坏行就 ERROR + 指标，没坏行不写任何日志（避免噪声）。
func (*SpecPriceGuardJob) Run(ctx context.Context) error {
	if db.DB == nil {
		return nil
	}
	bad, err := billing.UnparsableSpecModels(db.DB)
	if err != nil {
		logger.L().Warn("计价配置体检失败", zap.Error(err))
		return nil
	}
	if len(bad) == 0 {
		return nil
	}
	metrics.Add("spec_prices_unparsable", float64(len(bad)))
	// 只打前 10 个：坏行多时全量打印会把日志打爆，而定位只需要几个样本。
	sample := bad
	if len(sample) > 10 {
		sample = sample[:10]
	}
	logger.L().Error("存在档位价无法解析的模型（价格必须是整数微；这些模型无法按档位计价）",
		zap.Bool("spec_prices_unparsable", true),
		zap.Int("count", len(bad)),
		zap.Strings("sample", sample))
	return nil
}

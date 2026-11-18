package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/order"
)

// orderExpireInterval 是过期关单节拍。
//
// 判据在 order.ExpirePending 里（已从 created_at 改为 expire_at）：
// 普通渠道到期即关；虎皮椒订单等查单 worker 确认未付，超过补查窗口（30 分钟）才强制关，
// 且已有到账流水的绝不关。
const orderExpireInterval = 5 * time.Minute

// OrderExpireJob 关闭超时未支付订单，保证「过期订单续付」入口有确定的终态。
type OrderExpireJob struct{}

// NewOrderExpireJob 构造过期关单任务。
func NewOrderExpireJob() *OrderExpireJob { return &OrderExpireJob{} }

// Name 返回任务标识。
func (*OrderExpireJob) Name() string { return "order_expire" }

// Interval 返回关单节拍。
func (*OrderExpireJob) Interval() time.Duration { return orderExpireInterval }

// Run 执行一轮关单。
func (*OrderExpireJob) Run(ctx context.Context) error {
	n, err := order.New(db.DB).ExpirePending(ctx)
	if err != nil {
		logger.L().Warn("过期订单关单失败", zap.Error(err))
		return nil
	}
	if n > 0 {
		logger.L().Info("过期订单已关单", zap.Int64("expired", n))
	}
	return nil
}

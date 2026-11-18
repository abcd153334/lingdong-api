// Package redeem 实现兑换码校验与入账（v2.0 §4.3.10 / §4.8）。
package redeem

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/logs"
)

// CodeMaxLen 是兑换码最大长度（生成与核销共用，避免长度口径分叉）。
const CodeMaxLen = 32

// 兑换码状态（派生结果，列表/导出/前端唯一来源）。
const (
	StatusAvailable = "available"
	StatusUsed      = "used"
	StatusExpired   = "expired"
)

// 生成参数与上限。
const (
	codePrefix       = "LD-"
	codeBodyLen      = 24 // 总长 = len("LD-") + 24 = 27 ≤ CodeMaxLen
	codeAlphabet     = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	maxCodeAttempts  = 5
	MaxGenerateCount = 1000
)

// Service 是兑换码域服务。
type Service struct{ db *gorm.DB }

// New 构造兑换码服务。
func New(gdb *gorm.DB) *Service { return &Service{db: gdb} }

// StatusOf 派生兑换码状态（已用 > 已过期 > 可用），是状态判定的唯一实现。
func StatusOf(rc model.RedeemCode, now time.Time) string {
	if rc.UsedBy != nil {
		return StatusUsed
	}
	if rc.ExpiresAt != nil && now.After(*rc.ExpiresAt) {
		return StatusExpired
	}
	return StatusAvailable
}

// Item 是管理面列表/导出的兑换码视图（Status 为派生值，前端不得二次推导）。
type Item struct {
	ID        uint64     `json:"id"`
	Code      string     `json:"code"`
	Batch     string     `json:"batch"`
	Amount    int64      `json:"amount"`
	Status    string     `json:"status"`
	UsedBy    *uint64    `json:"used_by"`
	UsedAt    *time.Time `json:"used_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// ViewOf 把表行投影为视图（含派生状态）。
func ViewOf(rc model.RedeemCode, now time.Time) Item {
	return Item{
		ID: rc.ID, Code: rc.Code, Batch: rc.Batch, Amount: rc.Amount,
		Status: StatusOf(rc, now), UsedBy: rc.UsedBy, UsedAt: rc.UsedAt,
		ExpiresAt: rc.ExpiresAt, CreatedAt: rc.CreatedAt,
	}
}

// GenerateInput 是批量生成入参（校验在 controller 层完成，service 只做业务约束）。
type GenerateInput struct {
	Batch     string
	Amount    int64
	Count     int
	ExpiresAt *time.Time
}

// ListInput 是列表/导出筛选入参。
type ListInput struct {
	Batch  string
	Status string
	Offset int
	Limit  int
}

// newCode 生成一个兑换码：`LD-` + 24 位大写字母数字（去除易混字符 I/O/0/1）。
func newCode() (string, error) {
	buf := make([]byte, codeBodyLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("redeem: 生成随机字节失败: %w", err)
	}
	b := make([]byte, codeBodyLen)
	for i, v := range buf {
		b[i] = codeAlphabet[int(v)%len(codeAlphabet)]
	}
	return codePrefix + string(b), nil
}

// Generate 批量生成兑换码：单事务逐条插入；码冲突时重新生成该条（最多 maxCodeAttempts 次）。
// 冲突检测用 Unscoped 计数——唯一索引对软删除行同样生效，已删除的码不可复用。
//
// 返回**明文码列表**（按插入顺序），而不是条数：
// 生成后运营要立刻把码发出去，只回 `created` 等于逼他们再去列表页按批次筛一遍，
// 而列表是分页的（一屏 20 条），生成 500 条时第一批就看不到 —— 2026-10-29 审计 B-02。
// 明文码本来就能从列表与 CSV 全量读到，回码不新增任何敏感面。
func (s *Service) Generate(ctx context.Context, in GenerateInput) ([]string, error) {
	now := time.Now()
	codes := make([]string, 0, in.Count)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := 0; i < in.Count; i++ {
			var lastErr error
			inserted := false
			for attempt := 0; attempt < maxCodeAttempts; attempt++ {
				code, err := newCode()
				if err != nil {
					return err
				}
				var n int64
				if err := tx.Unscoped().Model(&model.RedeemCode{}).Where("code = ?", code).Count(&n).Error; err != nil {
					return err
				}
				if n > 0 {
					continue
				}
				row := model.RedeemCode{
					Code: code, Amount: in.Amount, Batch: in.Batch,
					ExpiresAt: in.ExpiresAt, CreatedAt: now, UpdatedAt: now,
				}
				if err := tx.Create(&row).Error; err != nil {
					lastErr = err
					continue
				}
				codes = append(codes, code)
				inserted = true
				break
			}
			if !inserted {
				if lastErr == nil {
					lastErr = errs.RedeemCodeConflict
				}
				return lastErr
			}
		}
		return nil
	})
	if err != nil {
		// 失败时返回 nil：事务已回滚，部分码并不存在，回半个列表会误导调用方。
		return nil, err
	}
	return codes, nil
}

// List 分页查询兑换码（batch/status 可选筛选）；状态筛选在 Go 侧按派生规则施加。
func (s *Service) List(ctx context.Context, in ListInput) ([]Item, int64, error) {
	q := s.db.WithContext(ctx).Model(&model.RedeemCode{})
	if in.Batch != "" {
		q = q.Where("batch = ?", in.Batch)
	}
	now := time.Now()
	switch in.Status {
	case StatusUsed:
		q = q.Where("used_by IS NOT NULL")
	case StatusExpired:
		q = q.Where("used_by IS NULL AND expires_at IS NOT NULL AND expires_at < ?", now)
	case StatusAvailable:
		q = q.Where("used_by IS NULL AND (expires_at IS NULL OR expires_at >= ?)", now)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.RedeemCode
	if err := q.Order("id DESC").Offset(in.Offset).Limit(in.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	items := make([]Item, 0, len(rows))
	for _, r := range rows {
		items = append(items, ViewOf(r, now))
	}
	return items, total, nil
}

// ExportRows 按筛选条件取导出数据（不超过 limit 行，导出不参与分页）。
func (s *Service) ExportRows(ctx context.Context, in ListInput, limit int) ([]Item, error) {
	var rows []model.RedeemCode
	q := s.db.WithContext(ctx).Model(&model.RedeemCode{})
	if in.Batch != "" {
		q = q.Where("batch = ?", in.Batch)
	}
	now := time.Now()
	switch in.Status {
	case StatusUsed:
		q = q.Where("used_by IS NOT NULL")
	case StatusExpired:
		q = q.Where("used_by IS NULL AND expires_at IS NOT NULL AND expires_at < ?", now)
	case StatusAvailable:
		q = q.Where("used_by IS NULL AND (expires_at IS NULL OR expires_at >= ?)", now)
	}
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(rows))
	for _, r := range rows {
		items = append(items, ViewOf(r, now))
	}
	return items, nil
}

// CSV 生成兑换码导出表（UTF-8 无 BOM，BOM 由 controller 的 writeCSV 统一补）。
func CSV(items []Item) string {
	var b strings.Builder
	b.WriteString("Code,Batch,Amount,Status,UsedBy,UsedAt,ExpiresAt,CreatedAt\n")
	for _, it := range items {
		b.WriteString(it.Code + ",")
		b.WriteString(logs.Esc(it.Batch) + ",")
		b.WriteString(strconv.FormatInt(it.Amount, 10) + ",")
		b.WriteString(it.Status + ",")
		b.WriteString(uint64PtrText(it.UsedBy) + ",")
		b.WriteString(timePtrText(it.UsedAt) + ",")
		b.WriteString(timePtrText(it.ExpiresAt) + ",")
		b.WriteString(it.CreatedAt.Format(time.RFC3339) + "\n")
	}
	return b.String()
}

func uint64PtrText(p *uint64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatUint(*p, 10)
}

func timePtrText(p *time.Time) string {
	if p == nil {
		return ""
	}
	return p.Format(time.RFC3339)
}

// DeleteBatch 软删除指定兑换码：仅允许删除未使用（used_by IS NULL）的码；
// 已使用或不存在的 id 一律跳过并计数返回（幂等，不整批失败）。
func (s *Service) DeleteBatch(ctx context.Context, ids []uint64) (deleted int, skipped int, err error) {
	if len(ids) == 0 {
		return 0, 0, nil
	}
	now := time.Now()
	res := s.db.WithContext(ctx).Model(&model.RedeemCode{}).
		Where("id IN ? AND used_by IS NULL", ids).
		Updates(map[string]any{"deleted_at": now})
	if res.Error != nil {
		return 0, 0, res.Error
	}
	deleted = int(res.RowsAffected)
	return deleted, len(ids) - deleted, nil
}

// Redeem 校验并核销兑换码：不存在/已用/已过期分别返回明确错误码；成功后经 billing 入账并写 redeem 流水。
func (s *Service) Redeem(ctx context.Context, userID uint64, code string) (int64, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" || len(code) > CodeMaxLen {
		return 0, errs.RedeemInvalid
	}

	var credited int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rc model.RedeemCode
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("code = ?", code).First(&rc).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errs.RedeemInvalid
			}
			return err
		}
		if rc.UsedBy != nil {
			return errs.RedeemUsed
		}
		if rc.ExpiresAt != nil && time.Now().After(*rc.ExpiresAt) {
			return errs.RedeemExpired
		}
		if err := billing.CreditTx(tx, billing.CreditInput{
			UserID: userID, Amount: rc.Amount, Type: model.TxnRedeem,
			RefType: "redeem_code", RefID: strconv.FormatUint(rc.ID, 10), Reason: "兑换码兑换",
			Cap: quotaUpperLimitFor(ctx, tx, userID),
		}); err != nil {
			if errors.Is(err, billing.ErrBalanceLimitExceeded) {
				return errs.BalanceLimitExceeded
			}
			return err
		}
		credited = rc.Amount
		now := time.Now()
		return tx.Model(&model.RedeemCode{}).Where("id = ?", rc.ID).
			Updates(map[string]any{"used_by": userID, "used_at": now}).Error
	})
	if err != nil {
		return 0, err
	}
	return credited, nil
}

// quotaUpperLimitFor 返回某用户的充值上限：用户级 > 0 优先，否则回落全局。
//
// 兑换码入账也要受用户级上限约束，否则「给某用户设了更严上限」的运营意图
// 会被一张兑换码绕过。查不到用户回落全局（上限是防超发兜底，不是主流程）。
// 回落规则统一走 conf.BalanceLimitFor，与下单/调账同一实现。
func quotaUpperLimitFor(ctx context.Context, tx *gorm.DB, userID uint64) int64 {
	var u model.User
	if err := tx.WithContext(ctx).Select("balance_limit").First(&u, userID).Error; err != nil {
		return conf.BalanceLimitFor(0)
	}
	return conf.BalanceLimitFor(u.BalanceLimit)
}

// OwnedItem 是「我的兑换记录」的对外投影。
//
// 与 Item 分开而不是复用：Item 里带 used_by（内部 uid）与 status（管理台要看
// 「这张码现在什么状态」）。对客场景只需要「我什么时候用哪张码充了多少」，
// 多给的字段都是将来不能改的包袱。
type OwnedItem struct {
	ID        uint64     `json:"id"`
	Code      string     `json:"code"`
	Amount    int64      `json:"amount"`
	Batch     string     `json:"batch"`
	UsedAt    *time.Time `json:"used_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// ListByUser 返回该用户已兑换的码（按 used_at 倒序，分页）。
//
// 为什么按 used_at 而不是 created_at：用户来这里是为了核对"我什么时候充的"，
// 而码的创建时间（批次生成时）对他来说没有意义。
func (s *Service) ListByUser(ctx context.Context, uid uint64, offset, limit int) ([]OwnedItem, int64, error) {
	base := s.db.WithContext(ctx).Model(&model.RedeemCode{}).Where("used_by = ?", uid)

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.RedeemCode
	if err := base.Order("used_at DESC, id DESC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	items := make([]OwnedItem, 0, len(rows))
	for _, rc := range rows {
		items = append(items, OwnedItem{
			ID: rc.ID, Code: rc.Code, Amount: rc.Amount, Batch: rc.Batch,
			UsedAt: rc.UsedAt, ExpiresAt: rc.ExpiresAt,
		})
	}
	return items, total, nil
}

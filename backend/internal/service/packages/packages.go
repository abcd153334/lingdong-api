package packages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/service/payment"
)

// Service 是充值套餐的配置域（管理台 CRUD）。
//
// 它**不依赖** service/order：order 承载下单/回调/退款/查单四条生命线，
// 配置域的 bug 不该有机会碰到支付主链路。两边共享的只有叶子包 pkg/money。
type Service struct{ db *gorm.DB }

func New(gdb *gorm.DB) *Service { return &Service{db: gdb} }

// Input 是写接口入参。三个可选字段用指针区分「未传」与「显式零值」：
// sort_order=0 是合法值（置顶），badge="" 是合法值（无角标），用值类型无法表达。
type Input struct {
	Name string
	// PriceCent 是售价（**分**）；Bonus 是赠送金额（**微**）。
	PriceCent int64
	Bonus     int64
	Enabled   *bool
	SortOrder *int
	Badge     *string
}

// Item 是管理台列表/详情的返回结构（已含服务端复算的额度拆分）。
type Item struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
	// PriceCent 是售价（分）；Base / Bonus / Credited 单位是**微**，由服务端复算，
	// 前端只做展示格式化，不做算术。
	PriceCent  int64     `json:"price_cent"`
	Base       int64     `json:"base"`
	Bonus      int64     `json:"bonus"`
	Credited   int64     `json:"credited"`
	Enabled    bool      `json:"enabled"`
	SortOrder  int       `json:"sort_order"`
	Badge      string    `json:"badge"`
	OrderCount int64     `json:"order_count"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Warning 是「保存成功但有运营风险」的提示，随 200 响应一起下发。
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// 角标枚举。三种共用同一视觉（.pkg-ribbon），只换文案 —— 刻意不做差异化配色，
// 否则每加一种枚举就要同步改 CSS，收益不抵维护成本。
var badgeSet = map[string]bool{"": true, "recommend": true, "hot": true, "value": true}

// 排序白名单：只允许映射后的列名进 ORDER BY，杜绝拼接注入。
var sortFields = map[string]string{
	"sort_order": "sort_order",
	"price_cent": "price_cent",
	"bonus":      "bonus",
	"created_at": "created_at",
	"id":         "id",
}

// ListInput 是列表筛选条件。
type ListInput struct {
	Keyword   string
	Enabled   string // "1" 启用 / "0" 停用 / 其他 = 不过滤
	Offset    int
	Limit     int
	SortField string
	Desc      bool
}

func (s *Service) item(p *model.Package, orderCount int64) Item {
	base, credit, err := packageAmounts(p.PriceCent, p.Bonus)
	if err != nil {
		// 只可能来自手工改坏的 DB 行（负价/超大值）。这里显示 0 而不是猜一个数：
		// 列表上直接可见「这行不对」，写回时会被 normalize 拦下。
		logger.L().Warn("套餐金额非法", zap.Uint64("package_id", p.ID),
			zap.Int64("price_cent", p.PriceCent), zap.Int64("bonus", p.Bonus), zap.Error(err))
	}
	return Item{
		ID: p.ID, Name: p.Name, PriceCent: p.PriceCent,
		Base: base, Bonus: p.Bonus, Credited: credit,
		Enabled: p.Enabled, SortOrder: p.SortOrder, Badge: p.Badge,
		OrderCount: orderCount, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// packageAmounts 是「售价（分）+ 赠送（微）→ 到账（微）」的**唯一**换算点。
//
// 纯加法：base = 分 × 1e4，credit = base + bonus。没有倍率、没有刻度。
func packageAmounts(priceCent, bonus int64) (base, credit int64, err error) {
	base, err = money.FromCents(priceCent)
	if err != nil {
		return 0, 0, err
	}
	credit, err = money.Sum(base, bonus)
	if err != nil {
		return base, 0, err
	}
	return base, credit, nil
}

// warning 只对「到账额度 > 充值上限」告警，不阻断保存。
//
// 为什么只告警：上限是随时可调的运营闸门，套餐是长期商品。硬拒会把「上限暂时偏低」
// 升级成「套餐完全无法维护」（连改名、改排序都被拒），而且会与默认种子冲突。
// 但列表与表单必须把这条红标出来，否则就是一个「静默失败的商品」。
func (s *Service) warning(priceCent, bonus int64) *Warning {
	capAmount := conf.BalanceLimitFor(0)
	if capAmount <= 0 {
		return nil
	}
	_, credit, err := packageAmounts(priceCent, bonus)
	if err != nil {
		// 换算不出来比「超上限」更严重：拿 0 去比上限会得到「没问题」的假结论。
		return &Warning{
			Code:    "credit_invalid",
			Message: "该套餐的到账金额超出可表示范围（售价或赠送金额过大），请调整后再保存",
		}
	}
	if credit <= capAmount {
		return nil
	}
	sym := conf.SiteCurrencySymbol()
	return &Warning{
		Code: "credit_over_upper_limit",
		Message: fmt.Sprintf("该套餐到账 %s%s 超过当前余额上限 %s%s，客户将无法购买；可保存后调高「余额上限」",
			sym, money.Format(credit, 2), sym, money.Format(capAmount, 2)),
	}
}

// List 返回分页结果与**含已删除订单**的引用计数。
//
// 口径必须与 Delete 的删除保护一致（都算软删订单）：否则列表显示「3 笔」而删除报
// 「被 7 笔订单引用」，客服会以为是 bug。
func (s *Service) List(ctx context.Context, in ListInput) ([]Item, int64, error) {
	scope := func() *gorm.DB {
		q := s.db.WithContext(ctx).Model(&model.Package{})
		if kw := strings.TrimSpace(in.Keyword); kw != "" {
			q = q.Where("name ILIKE ?", "%"+kw+"%")
		}
		switch in.Enabled {
		case "1":
			q = q.Where("enabled = ?", true)
		case "0":
			q = q.Where("enabled = ?", false)
		}
		return q
	}

	var total int64
	if err := scope().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	// Count 与 Find 各建一条链：复用同一个 *gorm.DB 会把 Count 的 statement 带进 Find。
	q := scope()
	if col, ok := sortFields[in.SortField]; ok {
		dir := "ASC"
		if in.Desc {
			dir = "DESC"
		}
		// 末尾恒加 id ASC：并列值翻页时不抖动。
		q = q.Order(col + " " + dir).Order("id ASC")
	} else {
		q = q.Order("sort_order ASC, price_cent ASC, id ASC")
	}
	var rows []model.Package
	if err := q.Offset(in.Offset).Limit(in.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}

	ids := make([]uint64, 0, len(rows))
	for _, p := range rows {
		ids = append(ids, p.ID)
	}
	counts, err := s.OrderCounts(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	items := make([]Item, 0, len(rows))
	for i := range rows {
		items = append(items, s.item(&rows[i], counts[rows[i].ID]))
	}
	return items, total, nil
}

// OrderCounts 批量补齐引用计数（一条 SQL，不做逐行 COUNT）。
//
// Unscoped：软删订单也算引用 —— 订单没有套餐名快照列，套餐行一旦消失，
// 那些历史订单就再也没法解释「这笔钱买的是什么档位」。
func (s *Service) OrderCounts(ctx context.Context, ids []uint64) (map[uint64]int64, error) {
	out := make(map[uint64]int64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		PackageID uint64
		N         int64
	}
	if err := s.db.WithContext(ctx).Unscoped().Model(&model.Order{}).
		Select("package_id, count(*) AS n").
		Where("package_id IN ?", ids).Group("package_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.PackageID] = r.N
	}
	return out, nil
}

// Get 读取单个套餐（含引用计数）。
func (s *Service) Get(ctx context.Context, id uint64) (*Item, error) {
	var p model.Package
	if err := s.db.WithContext(ctx).First(&p, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFound
		}
		return nil, err
	}
	counts, err := s.OrderCounts(ctx, []uint64{id})
	if err != nil {
		return nil, err
	}
	it := s.item(&p, counts[id])
	return &it, nil
}

// Create 新建套餐。
//
// **GORM 零值坑（必须按这个写法）**：Enabled 列声明了 default:true，
// 结构体 Create 会把 false 当零值忽略并回填 true —— 运营「先停用、下周上架」的套餐
// 会直接出现在门户收银台。sort_order=0（显式置顶）同样会被 default 吞掉。
// 因此：Create 前捕获目标值 → Create → 同一事务内用 map 把三列写回。
func (s *Service) Create(ctx context.Context, in Input) (*Item, *Warning, error) {
	name, err := s.normalize(in)
	if err != nil {
		return nil, nil, err
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	badge := ""
	if in.Badge != nil {
		badge = *in.Badge
	}

	var p model.Package
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := nameFree(tx, name, 0); err != nil {
			return err
		}
		sortOrder := 10
		if in.SortOrder != nil {
			sortOrder = *in.SortOrder
		} else {
			// 未传排序 → 追加到末尾（空表则从 10 开始）。
			// 这样「新建即置顶」不会发生，运营想置顶就显式填 0。
			var row struct{ Max *int }
			if err := tx.Model(&model.Package{}).Select("MAX(sort_order) AS max").Scan(&row).Error; err != nil {
				return err
			}
			if row.Max != nil {
				sortOrder = *row.Max + 10
			}
		}
		p = model.Package{
			Name: name, PriceCent: in.PriceCent, Bonus: in.Bonus,
			Enabled: enabled, SortOrder: sortOrder, Badge: badge,
		}
		if err := tx.Create(&p).Error; err != nil {
			if pgerr.IsUnique(err) {
				return errs.PackageNameExists
			}
			return err
		}
		if err := tx.Model(&model.Package{}).Where("id = ?", p.ID).Updates(map[string]any{
			"enabled": enabled, "sort_order": sortOrder, "badge": badge,
			"updated_at": time.Now(),
		}).Error; err != nil {
			return err
		}
		// 回读：Create 已把列默认值（enabled=true / sort_order=1000）回填进 p，
		// 直接拿 p 组装返回值会把「刚写进去的零值」又显示成默认值。
		return tx.First(&p, p.ID).Error
	})
	if err != nil {
		return nil, nil, err
	}
	it := s.item(&p, 0)
	return &it, s.warning(p.PriceCent, p.Bonus), nil
}

// Update 修改套餐。未传的可选字段保持原值；传了零值就写零值（sort_order=0 置顶、badge="" 去掉角标）。
//
// enabled 刻意不在这里：启停只走 SetEnabled 一条路径，否则同一个字段有两种写法、
// 两种审计动作，出问题时无法判断是哪条路径改的。
func (s *Service) Update(ctx context.Context, id uint64, in Input) (*Item, *Warning, error) {
	name, err := s.normalize(in)
	if err != nil {
		return nil, nil, err
	}
	var p model.Package
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&p, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errs.NotFound
			}
			return err
		}
		if err := nameFree(tx, name, id); err != nil {
			return err
		}
		// 一律用 map：struct 会跳过 false/0/"" 三个零值。
		upd := map[string]any{
			"name": name, "price_cent": in.PriceCent, "bonus": in.Bonus,
			"updated_at": time.Now(),
		}
		if in.SortOrder != nil {
			upd["sort_order"] = *in.SortOrder
		}
		if in.Badge != nil {
			upd["badge"] = *in.Badge
		}
		if err := tx.Model(&model.Package{}).Where("id = ?", id).Updates(upd).Error; err != nil {
			if pgerr.IsUnique(err) {
				return errs.PackageNameExists
			}
			return err
		}
		return tx.First(&p, id).Error
	})
	if err != nil {
		return nil, nil, err
	}
	counts, err := s.OrderCounts(ctx, []uint64{id})
	if err != nil {
		return nil, nil, err
	}
	it := s.item(&p, counts[id])
	return &it, s.warning(p.PriceCent, p.Bonus), nil
}

// SetEnabled 启停套餐（幂等：重复设置同一值不报错）。
func (s *Service) SetEnabled(ctx context.Context, id uint64, enabled bool) (*Item, error) {
	res := s.db.WithContext(ctx).Model(&model.Package{}).Where("id = ?", id).
		Updates(map[string]any{"enabled": enabled, "updated_at": time.Now()})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		// 行不存在，或值与原来相同且 updated_at 未变（极少见）：统一回读判定存在性。
		var exist int64
		if err := s.db.WithContext(ctx).Model(&model.Package{}).Where("id = ?", id).Count(&exist).Error; err != nil {
			return nil, err
		}
		if exist == 0 {
			return nil, errs.NotFound
		}
	}
	return s.Get(ctx, id)
}

// Delete 软删除套餐。有订单引用（含软删订单、任意状态）则拒绝。
//
// 与支付渠道「只查在途订单」不同：套餐的风险不是资金在途，而是订单没有套餐名快照列，
// 行被删掉后历史订单就查不到买的是哪一档。软删 + Unscoped 反查既能下架，又保得住追溯。
func (s *Service) Delete(ctx context.Context, id uint64) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var p model.Package
		if err := tx.First(&p, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errs.NotFound
			}
			return err
		}
		var n int64
		if err := tx.Unscoped().Model(&model.Order{}).Where("package_id = ?", id).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return errs.PackageInUse.WithMessage(
				fmt.Sprintf("该套餐已被 %d 笔订单引用，无法删除；请改为「停用」", n))
		}
		return tx.Delete(&model.Package{}, id).Error
	})
}

// normalize 校验并归一入参（名称去首尾空白、角标转小写）。
//
// b 由调用方读取后传入，不在本函数内再读一次：校验用的刻度/倍率必须与
// 同一请求里写库、复算返回值用的完全同一份。
func (s *Service) normalize(in Input) (string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", errs.InvalidParam.WithMessage("套餐名称必填")
	}
	if utf8.RuneCountInString(name) > 32 {
		return "", errs.InvalidParam.WithMessage("套餐名称不能超过 32 个字符")
	}
	if !payment.TitleSafe(name) {
		return "", errs.InvalidParam.WithMessage("套餐名称不能包含 % 或 emoji 等特殊符号，也不能以标点结尾（名称会进入支付标题）")
	}
	sym := conf.SiteCurrencySymbol()
	if in.PriceCent < MinPriceCent || in.PriceCent > MaxPriceCent {
		return "", errs.InvalidParam.WithMessage(fmt.Sprintf("售价需在 %s%s – %s%s 之间",
			sym, money.Format(mustCents(MinPriceCent), 2),
			sym, money.Format(mustCents(MaxPriceCent), 2)))
	}
	if in.Bonus < 0 || in.Bonus > MaxBonusAmount {
		return "", errs.InvalidParam.WithMessage(fmt.Sprintf("赠送金额需在 %s0 – %s%s 之间",
			sym, sym, money.Format(MaxBonusAmount, 2)))
	}
	// 换算自检：到账金额必须可表示（分 → 微 + 赠送，纯加法，可能溢出）。
	if _, _, err := packageAmounts(in.PriceCent, in.Bonus); err != nil {
		return "", errs.InvalidParam.WithMessage("售价与赠送金额相加后的到账金额超出可表示范围，请降低取值")
	}
	if in.Badge != nil {
		b := strings.ToLower(strings.TrimSpace(*in.Badge))
		if !badgeSet[b] {
			return "", errs.InvalidParam.WithMessage("角标取值不合法（仅支持 recommend/hot/value 或留空）")
		}
	}
	if in.SortOrder != nil && (*in.SortOrder < 0 || *in.SortOrder > MaxSortOrder) {
		return "", errs.InvalidParam.WithMessage(fmt.Sprintf("排序值需在 0 – %d 之间", MaxSortOrder))
	}
	return name, nil
}

// 取值边界（后端权威，前端只做同源提示，不重复定义业务规则）。
const (
	// MinPriceCent = 1 货币单位（分）：与自定义充值的 min_topup 默认值一致，避免「1 分钱包」。
	MinPriceCent = 100
	// MaxPriceCent = 100,000 货币单位（分）：全系统单笔金额硬上限（order.maxTopupCentHardCap）。
	// **刻意不用 balance_limit 派生的上限** —— 那个值随配置漂移，
	// 拿它当套餐价格上限会出现「运营调低余额上限后，连改个名字都保存失败」。
	MaxPriceCent = 10_000_000
	// MaxBonusAmount = 2000 货币单位（微）：防手滑多输几个 0；能否购买由余额上限告警兜底。
	MaxBonusAmount = 2000 * money.Scale
	MaxSortOrder   = 999_999
)

// mustCents 把分换成微（仅用于生成文案，不参与任何计费）。
func mustCents(c int64) int64 {
	v, err := money.FromCents(c)
	if err != nil {
		return 0
	}
	return v
}

// nameFree 检查同名（软删后允许复用，与 uq_packages_name 的部分索引口径一致）。
func nameFree(tx *gorm.DB, name string, excludeID uint64) error {
	q := tx.Model(&model.Package{}).Where("name = ?", name)
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return errs.PackageNameExists
	}
	return nil
}

// 并发建同名时应用层查重会漏，靠唯一索引兜底；判据见 pkg/pgerr。
//
// 必须映射成 409：未识别的 error 会被 respondErr 兜成 500，前端只能弹「服务器错误」。

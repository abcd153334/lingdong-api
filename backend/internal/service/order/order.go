// Package order 实现套餐/自定义充值下单、订单状态流转、支付回调入账、主动查单、
// 人工退款与过期关单（v2.0 §4.8）。
//
// 口径（唯一）：到账金额 = 实付金额（分 → 微，整数乘 1e4）+ 赠送金额（微）。纯加法，
// 没有倍率、没有额度刻度。金额单位与换算只在 pkg/money 里，本包不做任何算术。
//
// 支付相关的三条硬约束（改代码前必须先读）：
//  1. **先落库、再调上游**。反过来的话，上游建单成功而本地写库失败，用户拿到二维码并付款后
//     本地没有订单，无法入账也无从对账。
//  2. **解析只用 PayChannelID**，PayChannel 只是展示快照。按名字解析会让「管理台改个名」
//     把在途订单的回调全部打死。
//  3. **入账只有一条路径**（settlePaid）：回调与主动查单共用，绝不各写一套。
package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/pkg/sitecfg"
	"lingdong-api/internal/pkg/textx"
	"lingdong-api/internal/pkg/ulid"
	"lingdong-api/internal/service/affiliate"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/payment"
)

// pendingTTL 与 payment.PendingTTL 同源：前端倒计时、关单、查单窗口共用同一个值。
const pendingTTL = payment.PendingTTL

// pendingTTLFor 按协议返回订单存活时长（W1）。
//
// Stripe 的 Checkout Session hosted 页有效期为 24 小时，但收款场景不需要那么长；
// 30 分钟足够用户完成海外信用卡支付，同时不会让「本地已关单、用户还能付」的窗口
// 拖太长。其它协议维持 5 分钟（二维码天然 5 分钟有效）。
func pendingTTLFor(protocol string) time.Duration {
	if payment.ParseProtocol(protocol) == payment.ProtocolStripe {
		return 30 * time.Minute
	}
	return pendingTTL
}

// querySettleWindow 是「过期后继续补查」的窗口：二维码 5 分钟有效，
// 但用户可能在最后一秒付款、回调几分钟后才到，过期即刻放弃会真丢单。
const querySettleWindow = 30 * time.Minute

// maxTopupCentHardCap 是单笔金额硬上限（10 万元）。
//
// 为什么需要：Create 的入参是 float64，`1e300` 这种值 `math.Round(x*100)` 仍然是有限 float，
// 但 `int64(...)` 会溢出成负数 —— 于是订单金额为负、额度为负，
// 而 `u.Quota+credit > cap` 的判断还会因为负数被绕过。
//
// 它是**唯一**的金额硬闸，判定点在 placeOrder（所有下单分支的汇聚点）。
// 「入账后超账户上限」是另一条闸（balance_limit，错误码 BalanceLimitExceeded），
// 两者判据不同、错误码也不同，不许在 Create 里再复制一份。
const maxTopupCentHardCap int64 = 10_000_000

// maxPayAttempts 是同一订单最多发起几次上游下单。
// 超过后只允许查单，不允许继续 Create：重复 Create 会在上游留下垃圾单。
const maxPayAttempts = 3

// Service 是订单域服务。
type Service struct{ db *gorm.DB }

// New 构造订单服务。
func New(gdb *gorm.DB) *Service { return &Service{db: gdb} }

// View 是订单对外视图（门户/管理台共用）。
type View struct {
	ID       uint64 `json:"id"`
	UserID   uint64 `json:"user_id"`
	Username string `json:"username,omitempty"`
	// AppID 是订单归属的应用（0 = 平台直连）。**不下发省略**：0 有业务含义，
	// 前端要据此显示「平台直连」，用 omitempty 会让"没有应用"和"字段没下发"混为一谈。
	AppID uint64 `json:"app_id"`
	// AppName 是应用的展示名（display_name 优先），由 controller 批量补齐；
	// 0/查不到时为空串，前端自行渲染「平台直连」或「—」。
	AppName    string `json:"app_name,omitempty"`
	OutTradeNo string `json:"out_trade_no"`
	TradeNo    string `json:"trade_no,omitempty"`
	// AmountCent 是订单金额（**分**，支付渠道最小单位）。
	AmountCent int64 `json:"amount_cent"`
	// Currency 是订单币种（下单时固化）。分与微都是「该币种」的最小单位。
	Currency string `json:"currency"`
	// Credited 是到账金额（**微**），下单时冻结，回调不重算。
	Credited     int64  `json:"credited"`
	Status       string `json:"status"`
	PayChannel   string `json:"pay_channel"`
	PayChannelID string `json:"pay_channel_id"`
	PayMethod    string `json:"pay_method"`
	// PayMethodLabel 是支付方式中文（微信支付/支付宝）。历史订单存的是中文原文、
	// 新订单存枚举 code，展示一律用它，前端不再自己维护映射表。
	PayMethodLabel string `json:"pay_method_label"`
	PayProtocol    string `json:"pay_protocol"`
	Subject        string `json:"subject,omitempty"`
	// PackageID 只在管理台补名时用（0 = 自定义金额/续付），前端不展示。
	PackageID uint64 `json:"-"`
	// PackageName 是**实时**套餐名（按 package_id 反查，含软删套餐）；下单当时的原文在 Subject 里冻结。
	// 两者都给：改名后管理台看新名，对账看 Subject 才不会错乱。
	PackageName string     `json:"package_name,omitempty"`
	PayError    string     `json:"pay_error,omitempty"`
	ExpireAt    *time.Time `json:"expire_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	PaidAt      *time.Time `json:"paid_at"`

	// 退款信息。RefundMode 区分「原路退回」与「仅回退余额」，
	// 前端据此决定文案，对账据此判断是否应有上游退款单号。
	RefundStatus     string `json:"refund_status,omitempty"`
	RefundMode       string `json:"refund_mode,omitempty"`
	RefundNo         string `json:"refund_no,omitempty"`
	RefundAmountCent int64  `json:"refund_amount_cent,omitempty"`
	// RefundCredited 是实际扣回的平台额度（微）。balance 模式下 refund_amount_cent=0
	// 而这里非 0 —— 前端必须据此区分「钱没退、只是扣了额度」。
	RefundCredited int64      `json:"refund_credited,omitempty"`
	RefundReason   string     `json:"refund_reason,omitempty"`
	RefundError    string     `json:"refund_error,omitempty"`
	RefundAt       *time.Time `json:"refund_at,omitempty"`
	RefundDoneAt   *time.Time `json:"refund_done_at,omitempty"`
}

// PackageView 是套餐的对外视图：基础额度 / 赠送额度 / 到账总额均服务端复算，前端不做算术。
type PackageView struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
	// PriceCent 是售价（分）；Base / Bonus / Credited 单位是**微**，前端用 scale 格式化。
	PriceCent int64 `json:"price_cent"`
	Base      int64 `json:"base"`
	Bonus     int64 `json:"bonus"`
	Credited  int64 `json:"credited"`
	// Badge 是门户卡片角标（""=不渲染 DOM）。取值 recommend/hot/value，
	// 前端只做文案映射，不参与排序或过滤。
	Badge string `json:"badge"`
}

// ErrClientOrderNoTaken 表示 (user_id, app_id, client_order_no) 已被占用。
//
// 之所以要一个**专门**的哨兵而不是让调用方自己判 23505：唯一冲突可能来自
// out_trade_no（理论上不可能，那说明 ULID 撞了）或 client_order_no（并发重放，正常）。
// 只有后者可以被安全地"回查原单返回"，前者必须当作服务端故障暴露出来。
// 把判据收在服务层，调用方就不需要知道索引名或错误码。
var ErrClientOrderNoTaken = errors.New("order: 幂等号已被占用")

// CreateInput 是下单入参（套餐 / 自定义金额两条路径共用，PackageID 优先）。
//
// ChannelID + Method 取代了原来的 PayChannel 字符串：渠道改名不再影响历史订单，
// 也不会出现「名字里带 - 导致拆分错位」。
type CreateInput struct {
	UserID    uint64
	PackageID uint64
	// Amount 是自定义充值金额（**微**，已由 controller 用 money.Parse 从十进制字符串解析）。
	Amount    int64
	ChannelID string
	Method    string
	// AppID 是归属应用：0 = 平台直连（门户 / 会话票下单），>0 = 某接入应用代用户发起。
	//
	// 由调用方按**凭证**决定（托管密钥 -> 该应用；会话票 -> 0），不由请求体决定：
	// 让客户端自报 app_id 等于让它给自己贴归属，账单口径立刻失去意义。
	AppID uint64
	// ClientOrderNo 是客户端幂等号，"" = 不启用幂等。
	//
	// 它只解决"同一笔充值不要建两次"，**不**参与支付链路：落库后由
	// uq_orders_client_no_v2 兜并发，回查时按状态分流（见 UserCreateOrder）。
	ClientOrderNo string
}

// CreateResult 是下单结果。
//
// CashierURL 与 PayURL 是同一个值（移动端跳转地址），保留 CashierURL 是为了兼容既有前端字段；
// QRRef 是上游 url_qrcode 的**原值**（可能是图片地址，也可能是二维码内容），
// 真正可渲染的数据由 GET /portal/orders/:id/qrcode 提供（后端代理，避免防盗链与鉴权问题）。
type CreateResult struct {
	ID         uint64 `json:"id"`
	OutTradeNo string `json:"out_trade_no"`
	AmountCent int64  `json:"amount_cent"`
	// Status 是订单当前状态（pending/paid/expired/...）。
	//
	// 存在的理由只有一个：`POST /api/user/orders` 的**幂等重放**必须能回答
	// "上次那笔到底怎样了"。重放时如果只回支付链接，客户端无法区分
	// "还没付" 与 "已经付过了"，只能再去查一次单 —— 而它正是在"响应丢了"
	// 的情况下重放的，多一次往返就多一次丢包机会。
	Status string `json:"status"`
	// Currency 是订单币种：前端据此选符号（¥/$）。
	Currency       string `json:"currency"`
	Credited       int64  `json:"credited"`
	PayChannel     string `json:"pay_channel"`
	PayChannelID   string `json:"pay_channel_id"`
	PayMethod      string `json:"pay_method"`
	PayMethodLabel string `json:"pay_method_label"`
	PayProtocol    string `json:"pay_protocol"`
	CashierURL     string `json:"cashier_url"`
	PayURL         string `json:"pay_url"`
	QRRef          string `json:"qr_ref"`
	ExpiresIn      int    `json:"expires_in"`
	PayError       string `json:"pay_error,omitempty"`
}

// WalletConfig 是门户钱包页所需的配置快照（只读）。
type WalletConfig struct {
	CustomTopupEnabled bool `json:"custom_topup_enabled"`
	// MinTopup 是最低充值金额（**微**）。
	MinTopup int64 `json:"min_topup"`
	// BalanceLimit 是本次生效的余额上限（**微**，0 = 不限）。
	BalanceLimit int64 `json:"balance_limit"`
	// Scale 是「1 货币单位 = 多少微」，前端唯一需要的换算常量。
	Scale          int64   `json:"scale"`
	Currency       string  `json:"currency"`
	CurrencySymbol string  `json:"currency_symbol"`
	CNYPerUSD      float64 `json:"cny_per_usd"`
}

// Config 返回门户钱包页配置快照。
//
// userCap 传当前用户行的 balance_limit 原值（未设置时 0）。上限下发**生效值**
// 而不是全局值：前端用它预览「本次充值后是否超上限」，若下发全局值而该用户有
// 更严/更宽的个体覆盖，预览就会与后端校验分叉（明明能充却提示超限，或反过来）。
func Config(userCap int64) WalletConfig {
	return WalletConfig{
		CustomTopupEnabled: customTopupEnabled(),
		MinTopup:           minTopup(),
		BalanceLimit:       conf.BalanceLimitFor(userCap),
		Scale:              money.Scale,
		Currency:           conf.SiteCurrency(),
		CurrencySymbol:     conf.SiteCurrencySymbol(),
		CNYPerUSD:          conf.CnyPerUsd(),
	}
}

// ListFilter 是订单列表过滤条件（UserID=0 表示不过滤，管理台用）。
//
// PayChannelID 是新筛选口径；PayChannel 保留给旧前端按展示快照精确匹配。
// ClientOrderNoMaxLen 是客户端幂等号的最大长度，与 orders.client_order_no 的列宽一致。
//
// 必须在校验层先拒：超长值会被 PG 以 22001（value too long）拒绝，
// 那是个"看起来像服务端故障"的错误，最终映射成 500 —— 而它其实是参数问题。
const ClientOrderNoMaxLen = 64

type ListFilter struct {
	UserID uint64
	// AppID 非 0 时只返回该应用下的订单（托管密钥的隔离维度）。
	// 0 = 不按应用过滤（门户 / 会话票 / 自建 KEY）。
	AppID uint64
	// AppScoped 让「只看平台直连（app_id=0）」成为可表达的筛选。
	//
	// 为什么不能把上面的 AppID 语义改成「0 也过滤」：门户/会话票下单路径
	// 会把 AppID=0 传进来表达"不按应用过滤"（controller/order.go 的 appFilterOf），
	// 改语义会让它们突然只看平台直连，订单列表直接少一大半。
	// 因此保留 AppID 的旧语义，另加这个显式开关：AppScoped=true 时 AppID=0 也参与过滤。
	AppScoped    bool
	Status       string
	PayChannelID string
	PayChannel   string
	// ClientOrderNo 是**精确**匹配客户端幂等号（不是模糊/前缀）。
	//
	// 它的唯一用途是让接入方回答"我这次下单到底成没成"：客户端持有幂等号，
	// 网络超时后无法确认订单 id，只能按号反查。因此这里必须精确匹配，
	// 且不做"空串表示不过滤"以外的任何解释。
	ClientOrderNo string
	// OrderBy 是已经过白名单校验的 ORDER BY 片段（空 = 历史默认 `id DESC`）。
	//
	// 由 handler 用 pkg/sorting 解析后传入，service 不再看 `sort` 原文：
	// 列名映射与白名单属于对外契约，service 只认"能安全拼进 SQL 的片段"。
	OrderBy string
	Offset  int
	Limit   int
}

// Packages 返回启用中的套餐（含服务端复算的额度拆分）。
//
// 排序三列全 NOT NULL 且 id 唯一 → 全序，翻页与多实例一致，不会出现「同一页两次拉取顺序不同」。
// 不加缓存：单次是 idx_packages_portal 的覆盖索引扫描；加缓存就必须解决多实例失效广播，
// 而「停用后门户还展示」是事故级后果，收益为负。
//
// 套餐价是绝对值（分）且全库只有一个结算币种，因此不需要按币种过滤。
func (s *Service) Packages(ctx context.Context) ([]PackageView, error) {
	var rows []model.Package
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).
		Order("sort_order ASC, price_cent ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]PackageView, 0, len(rows))
	for _, p := range rows {
		base, err := money.FromCents(p.PriceCent)
		if err != nil {
			// 单条脏数据（负价、越界）不能打死整个钱包页：跳过该档并留痕。
			logger.L().Warn("套餐金额非法，已跳过该档",
				zap.Uint64("package_id", p.ID), zap.Int64("price_cent", p.PriceCent), zap.Error(err))
			continue
		}
		credit, err := money.Sum(base, p.Bonus)
		if err != nil {
			logger.L().Warn("套餐到账金额超出范围，已跳过该档",
				zap.Uint64("package_id", p.ID), zap.Int64("bonus", p.Bonus), zap.Error(err))
			continue
		}
		items = append(items, PackageView{
			ID: p.ID, Name: p.Name, PriceCent: p.PriceCent,
			Base: base, Bonus: p.Bonus, Credited: credit,
			Badge: p.Badge,
		})
	}
	return items, nil
}

// Create 下单：套餐或自定义金额 → 先落库 → 再调上游取支付信息。
func (s *Service) Create(ctx context.Context, in CreateInput) (*CreateResult, error) {
	var subject string
	var amountCent, credit int64
	var packageID uint64

	if in.PackageID != 0 {
		var p model.Package
		if err := s.db.WithContext(ctx).First(&p, in.PackageID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errs.NotFound
			}
			return nil, err
		}
		if !p.Enabled {
			// 停用 ≠ 不存在。回「资源不存在」会让点了旧卡片的用户以为站点坏了并反复重试；
			// 这条文案才是可执行的（刷新页面重新选一个）。
			return nil, errs.PackageUnavailable
		}
		// M4：套餐分支同样要有金额守卫。套餐价由运营配置，但 DB 手工改负价/超大价
		// 会让 credit 为负或 int64 溢出，绕过下单上限（自定义分支有守卫，套餐分支不能裸奔）。
		//
		// 「超过单笔硬上限」与「入账后超账户上限」**刻意不在这里判**：
		// 两条闸的唯一真相源都在 placeOrder（硬上限看 maxTopupCentHardCap，
		// 账户上限看 u.Quota+credit > balance_limit）。在 Create 里再判一遍
		// 会得到两种错误码指向同一个原因，前端无法据此给出可执行提示。
		if p.PriceCent <= 0 {
			return nil, errs.InvalidParam
		}
		amountCent = p.PriceCent
		base, convErr := money.FromCents(p.PriceCent)
		if convErr != nil {
			return nil, errs.InvalidParam
		}
		credit, convErr = money.Sum(base, p.Bonus)
		if convErr != nil {
			return nil, errs.AmountTooLarge
		}
		subject = payment.SanitizeTitle("充值-" + p.Name)
		packageID = p.ID
	} else {
		if !customTopupEnabled() {
			return nil, errs.CustomTopupDisabled
		}
		amt := in.Amount
		if amt <= 0 {
			return nil, errs.InvalidParam
		}
		if amt < minTopup() {
			return nil, errs.AmountBelowMin
		}
		// 微 → 分必须整除：支付渠道只认分，静默截断会让「订单金额」与「到账金额」
		// 出现无法解释的差额。用户输入 0.001 元这种就明确拒绝。
		cents, convErr := money.CentsOf(amt)
		if convErr != nil {
			return nil, errs.InvalidParam.WithMessage("充值金额最多保留两位小数")
		}
		amountCent = cents
		// 到账 = 实付（1:1，无倍率无刻度）。
		credit = amt
		subject = payment.SanitizeTitle("充值-自定义")
	}

	res, err := s.placeOrder(ctx, placeInput{
		userID: in.UserID, amountCent: amountCent, credit: credit,
		subject: subject, channelID: in.ChannelID, method: in.Method, packageID: packageID,
		appID: in.AppID, clientOrderNo: in.ClientOrderNo,
	})
	if err != nil && in.ClientOrderNo != "" && pgerr.IsUnique(err) {
		// 并发同号：uq_orders_client_no_v2 挡住了第二次插入。
		// 这不是错误 —— 调用方要回查原单，按它的状态返回（真幂等）。
		// 不转成哨兵的话，客户端会收到一个 500，然后**永远不知道该去查哪一单**。
		return nil, ErrClientOrderNoTaken
	}
	return res, err
}

// CreateForClient 是**对外开放面**的下单入口：Create + 幂等重放。
//
// 与 `Create` 的关系是"同一笔落单，两种响应契约"：首次调用与 `Create` 完全一致
// （同一套金额校验、同一套渠道路由、同一张订单表），只有 `ErrClientOrderNoTaken`
// 这一种失败被翻译成"回查原单"。
//
// 为什么必须在这一层做，而不是让 controller 接住 409 自己查：回查要按订单**状态**
// 分流（可付 / 已付 / 已过期 / 上游建单失败过），这套分流依赖
// `RetryPay`、`resultOf`、支付信息列语义 —— 全在 service 内部。放到 controller
// 就等于在 handler 里重新推导一遍"什么算可付"，两处判据必然漂移。
//
// 语义保证：只要 (user_id, client_order_no) 命中一行，本函数**一定**返回 200
// 与那一行的真实状态，不返回冲突错误。理由很实际 —— 调用方之所以重放，
// 是因为它没收到上一次的响应；此时再给它一个 409，它既不知道钱有没有出去，
// 也不知道该查哪一单。
func (s *Service) CreateForClient(ctx context.Context, in CreateInput) (*CreateResult, error) {
	res, err := s.Create(ctx, in)
	if err == nil {
		return res, nil
	}
	if in.ClientOrderNo == "" || !errors.Is(err, ErrClientOrderNoTaken) {
		return nil, err
	}
	return s.replayByClientOrderNo(ctx, in.UserID, in.AppID, in.ClientOrderNo)
}

// replayByClientOrderNo 回查幂等号对应的原单，并按状态给出可继续推进的响应。
//
// 三条分支，覆盖了 pending 的全部子状态：
//
//	已有支付信息   -> 原样返回（pay_url/qr_ref 都还在，客户端可直接跳转）
//	无支付信息     -> 走 RetryPay（**不能**直接返回 resultOf：那会给出空 pay_url，
//	                 客户端拿着空链接既不跳转也不会再试，这笔钱就永远收不到）
//	非 pending     -> 返回真实状态（paid 说明已到账，expired 说明要换新单号）
//
// RetryPay 失败时不再上抛：订单客观存在且不可付，把 resultOf 返回去
// （status=pending、pay_error 有值、expires_in 可能为 0）比抛错更有用 ——
// 客户端据此能判断"该换新单号重下了"，而错误码做不到这一点。
func (s *Service) replayByClientOrderNo(ctx context.Context, userID, appID uint64, clientOrderNo string) (*CreateResult, error) {
	var o model.Order
	// app_id 必须参与回查：唯一索引已改成 (user_id, app_id, client_order_no)。
	// 少这一条时，应用 A 用一个应用 B 用过的幂等号下单会命中 B 的订单，
	// 把 B 的金额、状态与支付链接返回给 A。
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND app_id = ? AND client_order_no = ?", userID, appID, clientOrderNo).
		First(&o).Error
	if err != nil {
		// 唯一冲突刚发生，行必然存在。走到这里说明它在本请求期间被物理删除
		// （本项目不提供删单），属于异常，按内部错误暴露而不是编一个"查无此单"。
		return nil, err
	}
	if o.Status != model.OrderPending {
		return resultOf(&o), nil
	}
	if o.PayURL == "" && o.QRRef == "" {
		if res, rerr := s.RetryPay(ctx, userID, o.ID); rerr == nil {
			return res, nil
		}
		// 重试失败：重读一次拿到本次写下的 pay_error / pay_attempts，
		// 否则返回的是重试**之前**的快照，客户端看到的失败原因会晚一轮。
		if derr := s.db.WithContext(ctx).First(&o, o.ID).Error; derr != nil {
			return nil, derr
		}
	}
	return resultOf(&o), nil
}

// Continue 过期订单续付：沿用原金额与到账额度新建订单（受 custom_topup_enabled 约束）。
func (s *Service) Continue(ctx context.Context, userID, orderID uint64) (*CreateResult, error) {
	var o model.Order
	if err := s.db.WithContext(ctx).First(&o, orderID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.OrderNotFound
		}
		return nil, err
	}
	if o.UserID != userID {
		return nil, errs.OrderNotFound
	}
	if o.Status != model.OrderExpired {
		return nil, errs.OrderNotPaid
	}
	if !customTopupEnabled() {
		return nil, errs.CustomTopupDisabled
	}
	// 续付必须沿用**原单**的到账金额：它是下单时冻结的值，重算会让用户实付与
	// 到账对不上（尤其在下单后调过价的情况下）。
	subject := payment.SanitizeTitle("充值-续付")
	return s.placeOrder(ctx, placeInput{
		userID: userID, amountCent: o.AmountCent, credit: o.Credited,
		subject: subject, packageID: o.PackageID,
		// 归属跟随原订单：续付是同一笔充值的延续，账必须记在同一个应用下。
		appID: o.AppID,
		// 幂等号**不继承**：uq_orders_client_no_v2 是唯一索引，沿用原号必然 23505。
		// 而"续付"本身就是"我接受这是一笔新订单"，不需要幂等号。
		clientOrderNo: "",
	})
}

// placeInput 是套餐/自定义/续付共用的落单入参。
type placeInput struct {
	userID     uint64
	amountCent int64
	// credit 是到账金额（微），落库即快照。
	credit    int64
	subject   string
	channelID string
	method    string
	packageID uint64
	// appID / clientOrderNo 见 CreateInput 的同名说明。
	appID         uint64
	clientOrderNo string
}

// pendingOccupyWindow 是「在途未支付订单」占额的统计窗口。
//
// 取 2 小时而不是按 expire_at 精确判断：expire_at 允许为 NULL（手工插单兜底），
// 而关单 worker 每 5 分钟跑一次，正常 pending 的寿命远小于 2 小时；
// 超过 2 小时还挂着的单说明 worker 出问题了，不该让它永久占据用户的余额上限。
const pendingOccupyWindow = 2 * time.Hour

// placeOrder 是唯一的下单路径：额度校验 → 渠道解析 → 回调地址校验 → 并发锁 →
// **先落库** → 调上游 → 幂等回写支付信息。
func (s *Service) placeOrder(ctx context.Context, in placeInput) (*CreateResult, error) {
	// 金额/额度硬闸是**唯一**一处：Create 的三个分支（套餐、自定义、续付）都汇到这里。
	// 放在读库之前，坏入参不产生任何 DB 往返。
	//
	// 为什么必须判 amountCent<=0：入参是 float64，`1e300` 的 math.Round(x*100) 仍是有限
	// float，但 int64(...) 溢出成负数 —— 订单金额为负，而 `u.Quota+credit > cap`
	// 也会因负 credit 被绕过（负负相消）。额度侧同样只接受正数。
	if in.amountCent <= 0 || in.amountCent > maxTopupCentHardCap {
		return nil, errs.AmountTooLarge
	}
	if in.credit <= 0 {
		return nil, errs.InvalidParam
	}

	var u model.User
	if err := s.db.WithContext(ctx).First(&u, in.userID).Error; err != nil {
		return nil, err
	}
	if cap := conf.BalanceLimitFor(u.BalanceLimit); cap > 0 && u.Balance+in.credit > cap {
		return nil, errs.BalanceLimitExceeded
	}

	ch, method, err := resolveChannel(in.channelID, in.method)
	if err != nil {
		return nil, err
	}
	notifyURL, returnURL, err := payURLs(ch)
	if err != nil {
		return nil, err
	}

	release, ok := lockTopup(ctx, in.userID)
	if !ok {
		return nil, errs.TopupInProgress
	}
	defer release()

	// R3：Redis 锁内再用 FOR UPDATE 串行化重判上限。
	//
	// 前面的前置检查是「快速失败」（省一次上游调用），但它读的是非锁快照；
	// 并发下两笔请求可能同时通过前置检查。这里对 users 行加行锁重判，
	// 把「可能双过闸」收窄成「后到的一笔在行锁上被串行化、读到最新余额后拒绝」。
	// Redis 锁不可用时（SETNX 返回 err 也放行），这里就是唯一的兜底。
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("balance", "balance_limit").First(&locked, in.userID).Error; err != nil {
			return err
		}
		cap := conf.BalanceLimitFor(locked.BalanceLimit)
		if cap <= 0 {
			return nil // 0 = 不限制，省掉下面那次聚合
		}
		// **在途订单必须占额**：只算「当前余额 + 本单」的话，用户可以连下 N 笔
		// 各接近上限的订单再一起支付 —— 每一笔下单时都通过校验，支付回调时
		// 后面的全部因「到账将超出余额上限」转 need_manual：钱收了、额度不到账，
		// 只能人工一单单退。这不是理论风险，一次性开 N 个收银台页面就能复现。
		//
		// 窗口取 2 小时而不是 expire_at：expire_at 可能为 NULL（手工插单兜底），
		// 而关单 worker 每 5 分钟跑一次，正常 pending 寿命远小于 2 小时。
		// 超过 2 小时还挂着的单属于异常（worker 停了），不该永久锁死用户额度。
		var pending int64
		if err := tx.Model(&model.Order{}).
			Where("user_id = ? AND status = ? AND created_at > ?",
				in.userID, model.OrderPending, time.Now().Add(-pendingOccupyWindow)).
			Select("COALESCE(SUM(credited), 0)").Scan(&pending).Error; err != nil {
			return err
		}
		if locked.Balance+pending+in.credit > cap {
			return errs.BalanceLimitExceeded
		}
		return nil
	}); err != nil {
		return nil, err
	}

	outTradeNo := "LD" + ulid.New()
	// 单号契约：虎皮椒要求 ≤32 且仅 [0-9A-Za-z_-*]。LD+ULID = 28 位，全部合法。
	if len(outTradeNo) > 32 {
		return nil, fmt.Errorf("订单号超长: %s", outTradeNo)
	}

	now := time.Now()
	expireAt := now.Add(pendingTTLFor(ch.Protocol))
	o := model.Order{
		UserID: in.userID, OutTradeNo: outTradeNo,
		AmountCent: in.amountCent, Credited: in.credit,
		Currency: conf.SiteCurrency(), AppID: in.appID, ClientOrderNo: in.clientOrderNo,
		Status: model.OrderPending, PayChannel: payment.DisplayName(ch, method),
		PayChannelID: ch.ID, PayMethod: string(method), PayProtocol: ch.Protocol,
		Subject: in.subject, PackageID: in.packageID,
		ExpireAt: &expireAt,
	}
	// 先落库：上游建单成功而本地写库失败 = 丢钱；本地先落库最坏只是一个可被 TTL 收尾的 pending。
	if err := s.db.WithContext(ctx).Create(&o).Error; err != nil {
		return nil, err
	}

	res, createErr := payment.Create(ctx, ch, payment.CreateRequest{
		OutTradeNo: outTradeNo, AmountCent: in.amountCent, Method: method,
		// 下单透传币种：与订单落库的 Currency 同源（conf.SiteCurrency），
		// 保证 Stripe 按订单币种收款，避免「¥72 被按 $72 建单」的 7.2 倍错位。
		Currency: o.Currency,
		Subject:  in.subject, NotifyURL: notifyURL, ReturnURL: returnURL,
		ClientIP: clientIPOf(ctx), Attach: strconv.FormatUint(o.ID, 10),
		// 传**绝对时间**而不是「now+TTL」：官方直连会把订单 TTL 直接写成上游的
		// time_expire / qr_code_timeout_express，用相对时间会让上游有效期与本地
		// expire_at 分叉（本地已过期、二维码还能付）。
		ExpireAt: expireAt,
	})
	if createErr != nil {
		s.markPayError(ctx, o.ID, createErr)
		logger.L().Warn("创建支付失败",
			zap.Uint64("order_id", o.ID), zap.String("channel", ch.ID),
			zap.Int64("amount_cent", in.amountCent), zap.Error(createErr))
		return nil, mapGatewayErr(createErr)
	}

	// 幂等回写：`pay_url = '' AND qr_ref = ''` 保证并发重试只有一次写入生效。
	// 必须同时判两列：官方直连的支付信息只在 qr_ref 里（pay_url 恒为空），
	// 只判 pay_url 等于没有守卫。
	if err := s.db.WithContext(ctx).Model(&model.Order{}).
		Where("id = ? AND pay_url = '' AND qr_ref = ''", o.ID).
		Updates(map[string]any{
			"pay_url":          res.PayURL,
			"qr_ref":           res.QRRef,
			"channel_order_id": res.ChannelOrderID,
			"pay_attempts":     gorm.Expr("pay_attempts + 1"),
			"pay_error":        "",
		}).Error; err != nil {
		// 回写失败不回滚订单：订单仍在 pending，用户可通过「重试」或查单恢复。
		logger.L().Error("回写支付信息失败",
			zap.Uint64("order_id", o.ID), zap.Error(err))
	}
	o.PayURL, o.QRRef, o.ChannelOrderID = res.PayURL, res.QRRef, res.ChannelOrderID
	logger.L().Info("创建支付成功",
		zap.Uint64("order_id", o.ID), zap.String("channel", ch.ID),
		zap.String("protocol", ch.Protocol), zap.Int64("amount_cent", in.amountCent))

	return resultOf(&o), nil
}

// markPayError 记录创建支付失败原因（截断后落库，避免超长 errmsg 触发 22001 回滚整个事务）。
func (s *Service) markPayError(ctx context.Context, orderID uint64, cause error) {
	msg := textx.TruncateRunes(cause.Error(), 180)
	if err := s.db.WithContext(ctx).Model(&model.Order{}).Where("id = ?", orderID).
		Updates(map[string]any{
			"pay_error":    msg,
			"pay_attempts": gorm.Expr("pay_attempts + 1"),
		}).Error; err != nil {
		logger.L().Warn("记录支付失败原因失败", zap.Uint64("order_id", orderID), zap.Error(err))
	}
}

// mapGatewayErr 把协议层错误翻译成对客错误码。
//
// 分类不是为了好看：参数/签名错误要告警给管理员（配置问题），
// 订单已存在要转查单（不是失败），一律 502 会让配置错误无声无息。
func mapGatewayErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, payment.ErrOrderExists):
		return errs.PayOrderExists
	case errors.Is(err, payment.ErrGatewayBusy):
		return errs.PayGatewayBusy
	case errors.Is(err, payment.ErrNotConfigured):
		return errs.PayNotConfigured
	case errors.Is(err, payment.ErrUnsupportedMethod):
		// 渠道还在但方式不匹配（EPay 配了 aggregate、或渠道方式被改后旧页面重试）。
		// 这不是网关故障，别让用户以为「系统坏了」。
		return errs.PayMethodUnavailable
	case errors.Is(err, payment.ErrInvalidAmount):
		return errs.InvalidParam
	case errors.Is(err, payment.ErrNoPayURL):
		return errs.PayGatewayError
	case errors.Is(err, payment.ErrSignInvalid):
		// 上游响应验签失败：对外仍是「支付网关错误」（不向用户泄露验签细节），
		// 但必须与「网关故障」分开 —— 前者意味着响应可能被篡改或公钥配错，
		// 后者是上游抖动，两者的排查方向完全不同。原始错误由调用方的 zap.Error 记录。
		return errs.PayGatewayError
	default:
		return errs.PayGatewayError
	}
}

// resolveChannel 解析下单渠道与支付方式，返回的 method 一定是归一化后的枚举。
//
// channelID 只是**优先尝试**：渠道被停用/删除时按方式回退到同方式的第一个可用渠道，
// 用户看到的仍然是「微信支付」，结果一致。但有两条硬边界：
//  1. 渠道存在且启用、只是不支持该方式 → 报错，**不静默改道**
//     （静默改道会掩盖前端传错，而且可能把钱导向另一个商户号）；
//  2. 重试支付**不得走本函数**（见 resolveRetryChannel）—— 订单的回调地址与
//     验签密钥绑死原渠道，换渠道建的单永远无法入账。
func resolveChannel(channelID, method string) (payment.Channel, payment.Method, error) {
	channelID = strings.TrimSpace(channelID)
	var m payment.Method
	if raw := strings.TrimSpace(method); raw != "" {
		parsed, ok := payment.ParseMethod(raw)
		if !ok {
			return payment.Channel{}, "", errs.PayMethodUnavailable
		}
		m = parsed
	}

	// 未指定渠道：有明确方式就按方式路由，否则取收银台第一个可用项。
	if channelID == "" {
		opts := payment.Options()
		if len(opts) == 0 {
			return payment.Channel{}, "", errs.PayMethodDisabled
		}
		if m != "" {
			// 只给了方式、没给渠道（例如旧前端缓存只回传了 method）：
			// 必须按方式去找承接渠道，不能盲取第一个 —— 用户选支付宝却拿到微信通道，
			// 是他点了支付宝之后被扣了微信的钱。
			opt, ok := payment.OptionByMethod(m)
			if !ok {
				return payment.Channel{}, "", errs.PayMethodUnavailable
			}
			ch, found := payment.FindChannel(opt.ChannelID)
			if !found || !ch.Enabled {
				return payment.Channel{}, "", errs.PayMethodDisabled
			}
			mm, _ := payment.ParseMethod(opt.Method)
			return ch, mm, nil
		}
		first := opts[0]
		ch, ok := payment.FindChannel(first.ChannelID)
		if !ok {
			return payment.Channel{}, "", errs.PayMethodDisabled
		}
		mm, _ := payment.ParseMethod(first.Method)
		return ch, mm, nil
	}

	ch, found := payment.FindChannel(channelID)
	// ChannelUsable 额外挡掉「启用但必填不全」：这种渠道的回调必然验签失败，
	// 让它建单就是把用户的钱送进一个永远到不了账的黑洞。
	if !found || !ch.Enabled || !payment.ParseProtocol(ch.Protocol).Valid() || !payment.ChannelUsable(ch) {
		// 渠道没了/停了：按方式回退。找不到同方式的渠道才报错。
		if m != "" {
			if opt, ok := payment.OptionByMethod(m); ok {
				if ch2, ok2 := payment.FindChannel(opt.ChannelID); ok2 {
					mm, _ := payment.ParseMethod(opt.Method)
					return ch2, mm, nil
				}
			}
		}
		if len(payment.Options()) == 0 {
			return payment.Channel{}, "", errs.PayMethodDisabled
		}
		return payment.Channel{}, "", errs.PayMethodUnavailable
	}
	// 币种校验：显式传 channel_id 绕过了 Options() 的币种过滤，
	// 必须在这里再拦一道 —— 否则「切了美元却还能用人民币渠道下单」。
	if !ch.SettlesIn(conf.SiteCurrency()) {
		return payment.Channel{}, "", errs.PayMethodUnavailable
	}

	if m != "" {
		// 两道关都要过：
		//  1. SupportsMethod —— 协议层能不能表达这种支付方式（易支付表达不了聚合）；
		//  2. 本渠道有没有勾选这种方式 —— 协议支持不代表运营开通了它。
		// 少了第 2 条，一条只勾了微信的易支付渠道也会接下支付宝订单，
		// 上游若没开通支付宝，用户看到的是「支付失败」，排查时却找不到原因。
		if !ch.SupportsMethod(m) || !channelHasMethod(ch, m) {
			return payment.Channel{}, "", errs.PayMethodUnavailable
		}
		return ch, m, nil
	}

	// 未指定方式：取该渠道第一个具体方式；一个都没有则按聚合处理。
	for _, raw := range ch.Methods {
		if mm, ok := payment.ParseMethod(raw); ok && mm.Specific() {
			return ch, mm, nil
		}
	}
	return ch, payment.MethodAggregate, nil
}

// channelHasMethod 判断渠道是否勾选了某种支付方式（配置原文可能是中文，先归一化）。
//
// 空 methods 视为「未声明」：协议合法的老配置（迁移前只有聚合支付）靠
// Options() 的兜底分支处理，这里不做二次猜测。
func channelHasMethod(ch payment.Channel, m payment.Method) bool {
	if len(ch.Methods) == 0 {
		return false
	}
	for _, raw := range ch.Methods {
		if parsed, ok := payment.ParseMethod(raw); ok && parsed == m {
			return true
		}
	}
	return false
}

// resolveRetryChannel 是**重试支付专用**的渠道解析：锁死订单原渠道，绝不换渠道。
//
// 为什么不能复用 resolveChannel：后者在渠道停用时会按方式回退到别的渠道。
// 但订单的 notify_url（含 channel_id）、验签密钥、上游单号全部绑死原渠道，
// 用另一个渠道 Create 出来的单，用户付款后回调永远验签失败 —— 钱进了别人家，
// 我们这边订单还是 pending。宁可报错让用户重新下单。
func resolveRetryChannel(o *model.Order) (payment.Channel, payment.Method, error) {
	ch, ok := payment.FindChannel(o.PayChannelID)
	if !ok {
		return payment.Channel{}, "", errs.PayMethodDisabled
	}
	// 停用的渠道不再建新支付单：停用往往意味着商户号异常或正在轮换密钥，
	// 继续用它只会得到一串「支付失败」，用户还以为是系统坏了。
	if !ch.Enabled || !payment.ParseProtocol(ch.Protocol).Valid() {
		return payment.Channel{}, "", errs.PayMethodDisabled
	}
	m, ok := payment.ParseMethod(o.PayMethod)
	if !ok {
		return payment.Channel{}, "", errs.PayMethodUnavailable
	}
	if !ch.SupportsMethod(m) {
		return payment.Channel{}, "", errs.PayMethodUnavailable
	}
	return ch, m, nil
}

// payURLs 构造回调与跳转地址。
//
// 只校验「是绝对 URL」与长度，不做 IP 校验：apiBase 来自**管理员配置**（不是我方要请求的目标），
// SSRF 面在于「我方主动请求用户给的地址」，这里恰好相反——是上游来请求我们。
// 强制 IP 校验会让本地开发（127.0.0.1）与内网预发环境完全无法下单。
func payURLs(ch payment.Channel) (notify, returnURL string, err error) {
	apiBase, siteBase := siteURLs()
	if apiBase == "" {
		return "", "", errs.PayNotConfigured
	}
	notify = apiBase + "/api/pay/" + ch.ID + "/notify"
	if len(notify) > 128 {
		return "", "", errs.PayNotConfigured
	}
	u, perr := url.Parse(notify)
	if perr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", errs.PayNotConfigured
	}
	// 渠道级 return_url 优先（可用于分渠道的落地页）；否则回落站点充值页。
	returnURL = strings.TrimSpace(ch.Params.ReturnURL)
	if returnURL == "" && siteBase != "" {
		returnURL = siteBase + "/topup"
	}
	return notify, returnURL, nil
}

// resultOf 用订单当前状态构造下单响应（含剩余有效期秒数，前端据此倒计时，
// 不用本地时钟减 expire_at —— 用户本机时间快 10 分钟就会一进页面就超时）。
func resultOf(o *model.Order) *CreateResult {
	return &CreateResult{
		ID: o.ID, OutTradeNo: o.OutTradeNo,
		AmountCent: o.AmountCent, Credited: o.Credited, Status: o.Status,
		Currency:   currencyOf(o),
		PayChannel: o.PayChannel, PayChannelID: o.PayChannelID,
		PayMethod: o.PayMethod, PayMethodLabel: payment.MethodLabel(o.PayMethod), PayProtocol: o.PayProtocol,
		CashierURL: o.PayURL, PayURL: o.PayURL, QRRef: o.QRRef,
		ExpiresIn: expiresIn(o), PayError: o.PayError,
	}
}

// expireAtOf 返回订单的绝对失效时间（喂给 payment.CreateRequest.ExpireAt）。
//
// 必须传绝对时间：`RetryPay` 在第 4 分钟重试时若让协议层按 now+TTL 计算，
// 上游有效期会多活 4 分钟，而本地已经要关单了。
func expireAtOf(o *model.Order) time.Time {
	if o.ExpireAt != nil {
		return *o.ExpireAt
	}
	return time.Now().Add(pendingTTL)
}

// expiresIn 返回剩余有效期秒数。
func expiresIn(o *model.Order) int {
	if o.ExpireAt == nil {
		return int(pendingTTL.Seconds())
	}
	d := time.Until(*o.ExpireAt)
	if d <= 0 {
		return 0
	}
	return int(d.Seconds())
}

// List 返回订单分页（UserID=0 时不过滤用户，供管理台使用）。
func (s *Service) List(ctx context.Context, f ListFilter) ([]View, int64, error) {
	scope := func() *gorm.DB {
		q := s.db.WithContext(ctx).Model(&model.Order{})
		if f.UserID != 0 {
			q = q.Where("user_id = ?", f.UserID)
		}
		if f.AppScoped || f.AppID != 0 {
			q = q.Where("app_id = ?", f.AppID)
		}
		if f.Status != "" {
			q = q.Where("status = ?", f.Status)
		}
		if f.PayChannelID != "" {
			q = q.Where("pay_channel_id = ?", f.PayChannelID)
		} else if f.PayChannel != "" {
			q = q.Where("pay_channel = ?", f.PayChannel)
		}
		if f.ClientOrderNo != "" {
			q = q.Where("client_order_no = ?", f.ClientOrderNo)
		}
		return q
	}
	var total int64
	if err := scope().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	orderBy := f.OrderBy
	if orderBy == "" {
		// 显式回落：`Order("")` 在 GORM 里会拼出空的 ORDER BY 片段，
		// 是语法错误而不是"不排序"。默认值必须在这里给出，不能指望调用方。
		orderBy = "id DESC"
	}
	var rows []model.Order
	if err := scope().Order(orderBy).Offset(f.Offset).Limit(f.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	items := make([]View, 0, len(rows))
	for i := range rows {
		items = append(items, toView(&rows[i]))
	}
	return items, total, nil
}

// TransactionView 是资金流水对外视图（含 balance_after 快照，保证跨页读一致）。
type TransactionView struct {
	ID           uint64    `json:"id"`
	Type         string    `json:"type"`
	Amount       int64     `json:"amount"`
	BalanceAfter int64     `json:"balance_after"`
	Reason       string    `json:"reason"`
	CreatedAt    time.Time `json:"created_at"`
}

// Transactions 返回本人资金流水（type 为空不过滤）。
//
// orderBy 是已校验的 ORDER BY 片段（空 = 历史默认 `id DESC`）。
// 流水表的同值行很多（同一秒的多笔扣费），因此默认也带 id 次序键。
func (s *Service) Transactions(ctx context.Context, userID uint64, txType string, offset, limit int, orderBy string) ([]TransactionView, int64, error) {
	return s.TransactionsScoped(ctx, userID, 0, txType, offset, limit, orderBy)
}

// TransactionsScoped 是带应用作用域的流水查询（appID=0 表示整账号）。
//
// 为什么要作用域：托管密钥是"应用后端代表某个用户"，它只应看到自己这个应用下
// 产生的资金流水。不加过滤时，A 应用能读到用户在 B 应用与平台直连的全部充值/消费
// （金额、类型、时间、订单关联）—— 与 keyScopeOf 在文件/任务/用量上已经执行的口径
// 直接矛盾。
//
// 过滤方式不新增列：orders 自带 app_id；usage 类流水经 usages.key_id → api_keys.app_id
// 反查（这条链本来就是用量归因的唯一路径）。`adjust`/`affiliate` 等账户级流水
// 仍然可见 —— 它们不属于任何应用，隐藏反而会让应用对不上账。
func (s *Service) TransactionsScoped(ctx context.Context, userID, appID uint64, txType string, offset, limit int, orderBy string) ([]TransactionView, int64, error) {
	q := s.db.WithContext(ctx).Model(&model.Transaction{}).Where("user_id = ?", userID)
	if txType != "" {
		q = q.Where("type = ?", txType)
	}
	if appID > 0 {
		q = q.Where(`(ref_type NOT IN ('order','usage')
			OR (ref_type = 'order' AND ref_id IN (SELECT id::text FROM orders WHERE app_id = ?))
			OR (ref_type = 'usage' AND ref_id IN (
			      SELECT u.id::text FROM usages u JOIN api_keys k ON k.id = u.key_id WHERE k.app_id = ?)))`,
			appID, appID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if orderBy == "" {
		orderBy = "id DESC"
	}
	var rows []model.Transaction
	if err := q.Order(orderBy).Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	items := make([]TransactionView, 0, len(rows))
	for _, t := range rows {
		items = append(items, TransactionView{
			ID: t.ID, Type: t.Type, Amount: t.Amount,
			BalanceAfter: t.BalanceAfter, Reason: t.Reason, CreatedAt: t.CreatedAt,
		})
	}
	return items, total, nil
}

// ManualCredit 人工入账（need_manual 丢单兜底）：人工核实支付凭据后补入账，与状态变更同事务。
//
// 「资金事实优先于状态机」：先看到账流水，再决定要不要加钱。
// 同时入账必须套嵌套事务（SAVEPOINT）——PostgreSQL 里 23505 会把整个事务置为
// aborted，之后任何语句都报 25P02。旧实现正是在 unique 冲突后继续 Updates，
// 于是"幂等成功"这条分支从来没生效过（refund.go 的 reclaimAffiliate 是正确范式）。
func (s *Service) ManualCredit(ctx context.Context, orderID uint64, reason string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o model.Order
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, orderID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errs.OrderNotFound
			}
			return err
		}
		if o.Status != model.OrderNeedManual {
			return errs.OrderNotPaid
		}

		credited, err := creditedAlready(tx, o.ID)
		if err != nil {
			return err
		}
		if !credited {
			err = tx.Transaction(func(ntx *gorm.DB) error {
				return billing.CreditTx(ntx, billing.CreditInput{
					UserID: o.UserID, Amount: o.Credited, Type: model.TxnRecharge,
					RefType: "order", RefID: strconv.FormatUint(o.ID, 10), Reason: reason,
					Cap: s.balanceLimitByID(ctx, tx, o.UserID),
				})
			})
			switch {
			case err == nil:
				credited = true
			case errors.Is(err, billing.ErrBalanceLimitExceeded):
				return errs.BalanceLimitExceeded
			case pgerr.IsUnique(err):
				// 并发回调抢先入账：回到存档点即可继续，事务仍然可用。
				credited = true
			default:
				return err
			}
		}

		// 返利按「前 N 笔」发放，与回调同序同口径：计费基数是实付（amount_cent）。
		// 同样走「尽力发放」：补单是为了把额度给到用户，不能被营销副作用拖住。
		affiliate.RewardBestEffort(tx, affiliate.RewardInput{
			InviteeID: o.UserID, OrderID: o.ID, AmountCent: o.AmountCent,
		})
		// 带状态条件：已 paid 的订单不能被重写 paid_at（行锁 + 条件双保险）。
		return tx.Model(&model.Order{}).Where("id = ? AND status = ?", o.ID, "need_manual").
			Updates(map[string]any{"status": "paid", "paid_at": time.Now()}).Error
	})
}

// ExpirePending 关闭超时未支付订单，返回关单数量。
//
// 两条规则，缺一不可：
//  1. **没有查单能力**的协议（易支付/空协议/历史数据）：到期即关；
//     expire_at 为 NULL 的老订单用 created_at 兜底（否则手工插入的行会永远 pending）。
//  2. **有查单能力**的协议（虎皮椒/微信/支付宝）：不在这里直接关 —— 先让查单 worker
//     确认未付；只有超过补查窗口（30 分钟）才强制关，且**已有到账流水的绝不关**
//     （最后一道防线：宁可不关，不能关掉已付款的单）。
//
// 协议集合只有一个真相源 `payment.QueryProtocols()`：它与 worker 的领取 SQL 共用，
// 两处不一致就会出现「不查单却也不关单」（永久 pending）或「查单还没跑完就被关掉」。
func (s *Service) ExpirePending(ctx context.Context) (int64, error) {
	var total int64

	queried := payment.QueryProtocols()
	if len(queried) == 0 {
		// 空集合会让 `IN ()` 变成 SQL 语法错误：宁可本轮不动作，也不能每轮报错。
		return 0, nil
	}
	inList, inArgs := paymentInClause(queried)

	// 第一条：**非查单协议**到期即关。
	// `pay_protocol IS NULL` 必须显式处理：`NULL NOT IN (...)` 求值为 NULL，
	// 行不会被匹配 → 这些订单会永远停在 pending（改造前靠 IS DISTINCT FROM 规避）。
	// `NOT EXISTS` 到账守卫两条都必须有：函数头承诺的「已有到账流水的绝不关」
	// 曾经只写在第二条上，于是「回调入账成功但状态更新失败」的订单（正是
	// reconcile 报 pending_but_credited 的那一类）会被这里直接过期 ——
	// 用户侧表现为丢单：额度到账了，订单显示未支付。
	res := s.db.WithContext(ctx).Exec(fmt.Sprintf(`
		UPDATE orders SET status = 'expired', updated_at = now()
		WHERE status = 'pending' AND deleted_at IS NULL
		  AND (pay_protocol IS NULL OR pay_protocol NOT IN (%s))
		  AND ((expire_at IS NOT NULL AND expire_at < now())
		       OR (expire_at IS NULL AND created_at < now() - interval '%d minutes'))
		  AND NOT EXISTS (
		        SELECT 1 FROM transactions t
		        WHERE t.ref_type = 'order' AND t.ref_id = orders.id::text AND t.type = 'recharge')`,
		inList, int(pendingTTL.Minutes())), inArgs...)
	if res.Error != nil {
		return 0, res.Error
	}
	total += res.RowsAffected

	// 第二条：**查单协议**超过补查窗口后强关，且要求没有任何到账流水。
	res2 := s.db.WithContext(ctx).Exec(fmt.Sprintf(`
		UPDATE orders SET status = 'expired', updated_at = now()
		WHERE status = 'pending' AND deleted_at IS NULL AND pay_protocol IN (%s)
		  AND expire_at < now() - interval '%d minutes'
		  AND NOT EXISTS (
		        SELECT 1 FROM transactions t
		        WHERE t.ref_type = 'order' AND t.ref_id = orders.id::text AND t.type = 'recharge')`,
		inList, int(querySettleWindow.Minutes())), inArgs...)
	if res2.Error != nil {
		return total, res2.Error
	}
	total += res2.RowsAffected

	return total, nil
}

// paymentInClause 生成 `?,?,?` 与对应的参数（协议名来自枚举常量，不拼字面量）。
func paymentInClause(ps []payment.Protocol) (string, []any) {
	ph := make([]string, len(ps))
	args := make([]any, len(ps))
	for i, p := range ps {
		ph[i] = "?"
		args[i] = string(p)
	}
	return strings.Join(ph, ","), args
}

func toView(o *model.Order) View {
	tradeNo := ""
	if o.TradeNo != nil {
		tradeNo = *o.TradeNo
	}
	return View{
		ID: o.ID, UserID: o.UserID, AppID: o.AppID, OutTradeNo: o.OutTradeNo, TradeNo: tradeNo,
		AmountCent: o.AmountCent, Credited: o.Credited,
		Currency: currencyOf(o),
		Status:   o.Status, PayChannel: o.PayChannel,
		PayChannelID: o.PayChannelID, PayMethod: o.PayMethod,
		PayMethodLabel: payment.MethodLabel(o.PayMethod), PayProtocol: o.PayProtocol,
		Subject: o.Subject, PackageID: o.PackageID, PayError: o.PayError, ExpireAt: o.ExpireAt,
		CreatedAt: o.CreatedAt, PaidAt: o.PaidAt,
		RefundStatus: o.RefundStatus, RefundMode: o.RefundMode, RefundNo: o.RefundNo,
		RefundAmountCent: o.RefundAmountCent, RefundCredited: o.RefundCredited,
		RefundReason: o.RefundReason,
		RefundError:  o.RefundError, RefundAt: o.RefundAt, RefundDoneAt: o.RefundDoneAt,
	}
}

// currencyOf 返回订单币种；空串（改造前的老订单 / 未回填）回落 CNY。
//
// 回落 CNY 不是「随便挑个默认值」：改造前系统只有人民币收款通道
// （recharge_ratio=69444，四个网关全收人民币），所以订单的历史币种就是 CNY。
// 美元订单必须显式写入 currency，绝不允许依赖回落 —— 那会让 ¥72 与 $72 混淆。
func currencyOf(o *model.Order) string {
	if o == nil {
		return conf.CurrencyCNY
	}
	if c, ok := conf.NormalizeCurrency(o.Currency); ok {
		return c
	}
	return conf.CurrencyCNY
}

// lockTopup 下单并发保护：SETNX topup:{uid} EX 30（Redis 不可用时放行）。
func lockTopup(ctx context.Context, userID uint64) (func(), bool) {
	noop := func() {}
	if !db.RedisReady() {
		return noop, true
	}
	key := fmt.Sprintf("topup:%d", userID)
	ok, err := db.RDB.SetNX(ctx, key, "1", 30*time.Second).Result()
	if err != nil {
		return noop, true
	}
	if !ok {
		return nil, false
	}
	return func() { _ = db.RDB.Del(ctx, key).Err() }, true
}

// lockRetry 是「同一订单重试创建支付」的短锁（15s）。
// 与 lockTopup 分开：用户可能同时有「新下单」和「重试旧单」两个动作，不应互相阻塞。
func lockRetry(ctx context.Context, orderID uint64) (func(), bool) {
	noop := func() {}
	if !db.RedisReady() {
		return noop, true
	}
	key := fmt.Sprintf("payretry:%d", orderID)
	ok, err := db.RDB.SetNX(ctx, key, "1", 15*time.Second).Result()
	if err != nil {
		return noop, true
	}
	if !ok {
		return nil, false
	}
	return func() { _ = db.RDB.Del(ctx, key).Err() }, true
}

// clientIPOf 取请求来源 IP（写审计与风控用；为空时返回空串）。
func clientIPOf(ctx context.Context) string {
	if v, ok := ctx.Value(clientIPKey{}).(string); ok {
		return v
	}
	return ""
}

// clientIPKey 是 clientIPOf 的上下文键。
type clientIPKey struct{}

// WithClientIP 把来源 IP 放进上下文（由 controller 调用）。
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// 唯一约束冲突的判据只有 pkg/pgerr 一份（见该包注释：三种呈现必须一起认）。

// containsString 判断切片是否包含给定值。
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// balanceLimit 返回全局余额上限（微）。0 表示不限。
func balanceLimit() int64 { return configAmount("balance_limit", 0) }

// balanceLimitByID 按 userID 查「生效的余额上限」（微，0 = 不限）。
//
// 查不到用户或用户级未设置时回落全局；查询失败也回落全局而不是报错 ——
// 上限是「防超发」的兜底，不是主流程，DB 抖动时宁可按全局值放行也不能挡掉一笔
// 已经付了钱的入账（否则钱收了、钱不到账）。
func (s *Service) balanceLimitByID(ctx context.Context, tx *gorm.DB, userID uint64) int64 {
	var u model.User
	if err := tx.WithContext(ctx).Select("balance_limit").First(&u, userID).Error; err != nil {
		return balanceLimit()
	}
	return conf.BalanceLimitFor(u.BalanceLimit)
}

// minTopup 返回最低充值金额（微）。
func minTopup() int64 { return configAmount("min_topup", money.Scale) }

func customTopupEnabled() bool { return configBool("custom_topup_enabled", false) }

// siteURLs 返回「机器面基址」与「浏览器面站点地址」。
//
// 两者的分工不可互换：回调/产物只能落在 API 地址上（那条域名才把
// /api/pay 与 /f 转发到后端），而支付完成后的跳回是浏览器行为，用站点地址。
func siteURLs() (api, site string) {
	s := sitecfg.Load()
	return s.PrimaryBase(), s.SiteURL
}

// configAmount 读取金额类配置（微）。0 是合法值（= 不限），因此不能像
// configInt 那样把 0 当「没配」回落默认值。
func configAmount(key string, def int64) int64 {
	raw, ok := conf.System().Get(key)
	if !ok {
		return def
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
		return def
	}
	return v
}

func configBool(key string, def bool) bool {
	raw, ok := conf.System().Get(key)
	if !ok {
		return def
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}

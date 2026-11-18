package controller

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/sorting"
	"lingdong-api/internal/service/order"
)

// orderStatuses 是订单状态白名单（筛选参数校验用）。
//
// 取值来自 model 的常量而不是字面量：这张表决定了「管理台能按哪些状态筛单」，
// 与状态机里的定义分家时，运营会发现自己按「退款中」筛不出任何订单。
var orderStatuses = map[string]bool{
	model.OrderPending: true, model.OrderPaid: true, model.OrderRefunding: true,
	model.OrderExpired: true, model.OrderRefunded: true, model.OrderNeedManual: true,
}

// orderSortColumns 是订单列表的排序白名单：**对外字段名 → SQL 列**。
//
// `amount` 对外是"元"，SQL 列是 `amount_cent`（分）：直接按对外字段名拼 SQL 会
// 报"列不存在"，而那是个 500 —— 接入方看不出是自己传错了字段。
// 键集合与 apicontract `billing.orders_list` 的 Sortable 逐字一致（由门禁比对）。
var orderSortColumns = map[string]string{
	"created_at": "created_at",
	"amount":     "amount_cent",
}

// transactionSortColumns 是资金流水的排序白名单（金额列直接叫 amount，单位是微）。
var transactionSortColumns = map[string]string{
	"created_at": "created_at",
	"amount":     "amount",
}

// PortalPackages 处理 GET /api/portal/packages：套餐 + 钱包配置 + 收银台支付方式。
func PortalPackages(c *gin.Context) {
	items, err := order.New(db.DB).Packages(c.Request.Context())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 上限按**当前登录用户**的生效值下发（用户级 > 0 优先，否则全局）。
	// 这个路由挂在 UserAuth 组内，一定拿得到 user_id；查库失败则回落全局（
	// 与后端校验同口径，预览偏差只会是「保守地显示更严」，不会误导用户多充）。
	var userCap int64
	if uid := c.GetUint64("user_id"); uid > 0 {
		var u model.User
		if err := db.DB.WithContext(c.Request.Context()).
			Select("balance_limit").First(&u, uid).Error; err == nil {
			userCap = u.BalanceLimit
		}
	}
	cfg := order.Config(userCap)
	// 关掉配额查询的 KEY：上限属于账户级策略值，与余额同级别，不该下发。
	// 与 /api/user/self 的 403、transactions 的余额置零保持同一口径。
	if !quotaVisible(c) {
		cfg.BalanceLimit = 0
	}
	response.OK(c, gin.H{
		"items":                items,
		"custom_topup_enabled": cfg.CustomTopupEnabled,
		"min_topup":            cfg.MinTopup,
		"balance_limit":        cfg.BalanceLimit,
		// scale 是唯一换算常量（1 单位货币 = scale 微）：前端所有金额格式化都乘除它，
		// 不允许在 JS 里写死 1e6 —— 否则刻度调整后前后端各算各的。
		"scale":       cfg.Scale,
		"cny_per_usd": cfg.CNYPerUSD,
		// 币种必须随套餐一起下发：前端要按它渲染 ¥/$ 前缀，也要用它判断
		// 「这个套餐能不能买」（order.Packages 只返回同币种套餐）。
		// 缺这两个键时前端 formatCent 会回落默认符号，全站显示错币种。
		"currency":        conf.SiteCurrency(),
		"currency_symbol": conf.SiteCurrencySymbol(),
		// pay_options 只下发 `{method, label}`：用户只选「微信还是支付宝」，
		// 承接渠道（虎皮椒/迅虎/易支付）是供应链信息，且方式→渠道的映射
		// 只在 service/order.resolveChannel 一处实现（前端不参与选路）。
		"pay_options": order.PortalPayOptions(),
	})
}

// topupReq 是下单入参。
//
// 只有 method（支付方式枚举）：承接渠道由服务端按方式路由，
// 前端不再持有 channel_id —— 少一个可被篡改的字段，也少一份「方式→渠道」的真相源。
type topupReq struct {
	PackageID uint64 `json:"package_id"`
	// Amount 是自定义充值金额的**十进制**（如 "10.5"，也接受 JSON number）。
	//
	// 不收 float64：0.1+0.2 这类场景会漂，而金额差 1 分就是账务错误；
	// 解析点只有 money.Decimal → money.Parse 一处，超出两位小数会明确报错而不是静默截断。
	Amount money.Decimal `json:"amount"`
	Method string        `json:"method"`
}

// PortalTopup 处理 POST /api/portal/topup：套餐/自定义金额下单。
//
// 虎皮椒是服务端下单（同步出网），所以这个接口的最坏耗时约 5s；
// 前端必须显示「正在连接支付通道」而不是无声等待。
func PortalTopup(c *gin.Context) {
	var req topupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 走套餐时金额由套餐决定（Amount 忽略）；自定义金额才解析。
	var amount int64
	if req.PackageID == 0 {
		v, err := req.Amount.Micro()
		if err != nil {
			response.FailDef(c, errs.InvalidParam.WithMessage("充值金额格式不正确（最多两位小数）"))
			return
		}
		if v <= 0 {
			response.FailDef(c, errs.InvalidParam.WithMessage("充值金额必须大于 0"))
			return
		}
		amount = v
	}
	ctx := order.WithClientIP(c.Request.Context(), c.ClientIP())
	res, err := order.New(db.DB).Create(ctx, order.CreateInput{
		UserID: c.GetUint64("user_id"), PackageID: req.PackageID,
		Amount: amount, Method: req.Method,
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, order.PortalCreateResultOf(res))
}

// PortalOrderContinue 处理 POST /api/portal/orders/:id/continue：过期订单续付（新建订单）。
func PortalOrderContinue(c *gin.Context) {
	id, ok := orderIDParam(c)
	if !ok {
		return
	}
	res, err := order.New(db.DB).Continue(c.Request.Context(), c.GetUint64("user_id"), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, order.PortalCreateResultOf(res))
}

// PortalOrderRetryPay 处理 POST /api/portal/orders/:id/pay：对同一订单重新获取支付信息。
//
// 前端「支付通道暂不可用 → 重试」必须走这里，**不能重新下单**：
// 重新下单会换单号，而上游可能已有第一笔订单，用户付了新的又付旧的 → 双倍扣款。
func PortalOrderRetryPay(c *gin.Context) {
	id, ok := orderIDParam(c)
	if !ok {
		return
	}
	ctx := order.WithClientIP(c.Request.Context(), c.ClientIP())
	res, err := order.New(db.DB).RetryPay(ctx, c.GetUint64("user_id"), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, order.PortalCreateResultOf(res))
}

// PortalOrderVerify 处理 POST /api/portal/orders/:id/verify：「我已支付，立即核对」。
//
// 主动查单一次并返回最新状态；查单失败不报错（query_ok=false），
// 因为「查不到」不等于「没付」，报错会让用户以为支付失效而重复付款。
func PortalOrderVerify(c *gin.Context) {
	id, ok := orderIDParam(c)
	if !ok {
		return
	}
	res, err := order.New(db.DB).VerifyForUser(c.Request.Context(), c.GetUint64("user_id"), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, order.PortalVerifyResultOf(res))
}

// PortalOrderQRCode 处理 GET /api/portal/orders/:id/qrcode：二维码展示数据。
//
// 返回的是后端代理后的 data URL 或二维码内容，**不是上游图片地址**：
// 直接让浏览器加载上游图片会撞上防盗链、混合内容与「img 无法带 Bearer 头」三个问题。
func PortalOrderQRCode(c *gin.Context) {
	id, ok := orderIDParam(c)
	if !ok {
		return
	}
	scopeApp, _, ok := appFilterOf(c)
	if !ok {
		response.FailDef(c, errs.ScopeDenied)
		return
	}
	payload, left, err := order.New(db.DB).QRCodeFor(c.Request.Context(), c.GetUint64("user_id"), scopeApp, id)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, gin.H{"qr": payload, "expires_in": left})
}

// orderIDParam 解析并校验路径里的订单 ID。
func orderIDParam(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return 0, false
	}
	return id, true
}

// txTypes 是资金流水类型白名单（§4.2.9 六型枚举）。
// 直接引用 model 的常量：白名单与枚举必须是同一份清单。
// 曾经这里手抄一份、model 里手写一份，新增类型时必然只改一边
// （`refund_revert` 就漏在 model 侧，用户按它筛选会 400）。
var txTypes = map[string]bool{
	model.TxnFreeze: true, model.TxnSettle: true, model.TxnRelease: true,
	model.TxnRecharge: true, model.TxnRedeem: true, model.TxnRefund: true,
	model.TxnAdjust: true, model.TxnInvite: true,
	model.TxnRefundRevert: true,
}

// PortalTransactionList 处理 GET /api/portal/transactions：本人资金流水（T21 分页 + type 筛选）。
func PortalTransactionList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	txType := strings.TrimSpace(c.Query("type"))
	if txType != "" && !txTypes[txType] {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 变量名不能叫 order：会把下面的 `order.New(...)` 包名遮住（编译器只报
	// "order.ClauseOr undefined"，看不出是被遮了）。
	sortOrd, ok := sorting.Query(c, transactionSortColumns)
	if !ok {
		return
	}
	// hosted 密钥只看本应用产生的流水；会话票 = 整账号。
	appID, _, ok := appFilterOf(c)
	if !ok {
		response.FailDef(c, errs.ScopeDenied)
		return
	}
	items, total, err := order.New(db.DB).TransactionsScoped(c.Request.Context(), c.GetUint64("user_id"), appID, txType, p.Offset(), p.Limit(), sortOrd.ClauseOr("id DESC"))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 未开启额度查询的 KEY：保留流水（应用对账要用），但把余额快照置零。
	// 不整端点 403 —— 那会让"只想知道自己花了多少"的托管方彻底查不了账。
	if !quotaVisible(c) {
		for i := range items {
			items[i].BalanceAfter = 0
		}
	}
	response.OK(c, pagination.New(items, total, p))
}

// PortalOrderList 处理 GET /api/portal/orders：本人订单（T21 分页 + status 筛选）。
func PortalOrderList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !orderStatuses[status] {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// client_order_no 面向接入方：客户端用它反查"我这次下单成没成"，
	// 因此按精确值匹配，超长直接 400（否则会以 22001 落到 500）。
	clientOrderNo := strings.TrimSpace(c.Query("client_order_no"))
	if len(clientOrderNo) > order.ClientOrderNoMaxLen {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	sortOrd, ok := sorting.Query(c, orderSortColumns)
	if !ok {
		return
	}
	// 托管密钥只看自己应用下的订单：订单的 app_id 就是"这笔充值归因给谁"，
	// 跨应用可见等于让 A 应用看到用户在 B 应用的消费。
	appFilter, _, ok := appFilterOf(c)
	if !ok {
		// hosted 却没有归属应用：拒绝，不能退化成整账号可见。
		response.FailDef(c, errs.ScopeDenied)
		return
	}
	items, total, err := order.New(db.DB).List(c.Request.Context(), order.ListFilter{
		AppID:  appFilter,
		UserID: c.GetUint64("user_id"), Status: status, ClientOrderNo: clientOrderNo,
		OrderBy: sortOrd.ClauseOr("id DESC"),
		Offset:  p.Offset(), Limit: p.Limit(),
	})
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, pagination.New(order.PortalOrderViews(items), total, p))
}

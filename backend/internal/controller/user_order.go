package controller

// 本文件是 `/api/user/orders*`（对外面充值端点）的实现。
//
// 与门户的 `POST /portal/topup`、`GET /portal/orders` 的关系：**同一套 service**，
// 不同的凭证面与不同的幂等契约。门户的下单由浏览器页面驱动（Cookie + CSRF），
// 这里的下单由接入方服务端驱动（Bearer），并且必须支持客户端幂等号 ——
// 因为服务端到服务端的调用一定会遇到"请求发出去了、响应没收到"的情况，
// 而重试一笔充值绝不能变成两笔。

import (
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/keyscope"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/order"
)

// userTopupReq 是开放面下单入参。
//
// 与门户的 `topupReq` 差别只有两个：多了 `client_order_no`（幂等号），
// 且**没有** `channel_id`（承接渠道由服务端按 method 路由，与门户一致）。
type userTopupReq struct {
	PackageID uint64 `json:"package_id"`
	// Amount 是自定义充值金额的十进制文本（"10.5"），套餐路径忽略此项。
	Amount money.Decimal `json:"amount"`
	Method string        `json:"method"`
	// ClientOrderNo 是调用方自己的幂等号（1..64 位，[A-Za-z0-9_.:-]）。
	ClientOrderNo string `json:"client_order_no"`
}

// UserCreateOrder 处理 POST /api/user/orders：套餐/自定义金额下单。
//
// 响应体与门户的"创建结果"是同一个结构（`order.CreateResult`），并额外带
// `status` —— 因为本端点会做幂等重放，重放时必须能回答"上次那笔现在什么状态"。
//
// 幂等契约（接入方必须按这个实现重试）：
//   - 同一个 `client_order_no` 重复提交，**永远**返回 200 与同一张订单；
//   - 首次提交后的任何失败（包括"上游支付通道不可用"），都用**同一个号**重试，
//     不要换号 —— 换号就是新的订单，会真的多出一笔待付单；
//   - `status` 已经是 paid 时不要再发起支付；是 expired 时用**新号**重新下单。
//
// 不开放"我已支付，帮我确认"（门户的 verify）：订单状态的唯一真相源是支付回调，
// 让客户端能触发核对，等于给它一个"用伪造的查询结果推进订单"的入口。
func UserCreateOrder(c *gin.Context) {
	var req userTopupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	no := strings.TrimSpace(req.ClientOrderNo)
	if !validClientOrderNo(no) {
		response.FailDef(c, errs.InvalidParam.WithMessage("client_order_no 只能包含字母、数字与 _ . : - ，长度 1-64"))
		return
	}
	// 走套餐时金额由套餐决定（Amount 忽略）；自定义金额才解析。
	// 金额下限、余额上限、在途占额都在 service.Create 里再校验一遍，
	// 这里只负责"能否解析成整数微"。
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
	res, err := order.New(db.DB).CreateForClient(ctx, order.CreateInput{
		UserID: c.GetUint64("user_id"), PackageID: req.PackageID,
		Amount: amount, Method: req.Method,
		// 归属由**凭证**决定，不由请求体决定：托管密钥 -> 该应用，会话票 -> 平台直连。
		AppID:         appIDOf(c),
		ClientOrderNo: no,
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	// 用与门户同一份投影：原始 `CreateResult` 含 pay_channel / pay_channel_id /
	// pay_protocol 三个**供应链字段**（聚合支付的具体家数、协议名），
	// 对外直出等于把平台的技术栈交给接入方。投影里 AutoRedirect 已取代
	// 「协议 == stripe」的判断，信息量不减。
	response.OK(c, order.PortalCreateResultOf(res))
}

// UserOrderGet 处理 GET /api/user/orders/{id}：本人订单详情。
//
// 客户端轮询它来获知支付结果（`status` 变 paid、`credited` 可见）。
// 越权与不存在都返回 order_not_found：区分二者会让调用方能枚举别人的订单号。
func UserOrderGet(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		// 路径参数不是数字属于**参数非法**，不是"订单不存在"。
		// 合成 not_found 会让按 error_code 分支的第三方把"我把 id 拼错了"
		// 读成"这单不见了"，进而去查对账。
		response.FailDef(c, errs.InvalidParam)
		return
	}
	scopeApp, _, ok := appFilterOf(c)
	if !ok {
		// 托管密钥缺少归属应用：拒绝而不是"当成平台直连"。
		response.FailDef(c, errs.ScopeDenied)
		return
	}
	v, err := order.New(db.DB).GetForUser(c.Request.Context(), c.GetUint64("user_id"), scopeApp, id)
	if err != nil {
		if def, isDef := errs.AsDef(err); isDef {
			response.FailDef(c, def)
			return
		}
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, order.PortalOrderViewOf(v))
}

// appIDOf 返回本次请求的归属应用：API KEY 凭证取密钥上的 AppID，其余为 0。
//
// 唯一真相源是 `api_key_record`（由 UserTokenAuth 写入）。**不要**接受
// 请求体或查询参数里的 app_id：让调用方自报归属，等于让它把消费记到别人账上，
// 账单与限额口径会立刻失去意义。
func appIDOf(c *gin.Context) uint64 {
	s, _ := keyscope.Of(c)
	return s.AppID
}

// validClientOrderNo 校验幂等号形态。
//
// 空串合法（= 不启用幂等，与门户同路径），非空时限制字符集与长度：
//   - 长度上限对齐 `orders.client_order_no` 的列宽，超长会被 PG 以 22001 拒绝，
//     那会变成一次"看起来像服务端故障"的 500；
//   - 字符集排除空白与引号，避免调用方把结构化内容（JSON、URL）塞进来后，
//     日志与对账导出里出现无法解析的行。
func validClientOrderNo(no string) bool {
	if no == "" {
		return true
	}
	if len(no) > order.ClientOrderNoMaxLen {
		return false
	}
	for _, r := range no {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}

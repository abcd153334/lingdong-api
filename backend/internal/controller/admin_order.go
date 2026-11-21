package controller

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/appref"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/logs"
	"lingdong-api/internal/service/order"
	"lingdong-api/internal/service/payment"
)

// 订单导出上限：单次最多 5 万行（与兑换码/审计导出一致）。
const orderExportLimit = 50000

// orderFilterOf 解析列表/导出共用的筛选条件（status 白名单校验；user_id/pay_channel/app 精确匹配）。
func orderFilterOf(c *gin.Context) (order.ListFilter, bool) {
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !orderStatuses[status] {
		return order.ListFilter{}, false
	}
	var userID uint64
	if raw := strings.TrimSpace(c.Query("user_id")); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return order.ListFilter{}, false
		}
		userID = v
	}
	appID, appScoped, ok := appScopeQuery(c)
	if !ok {
		return order.ListFilter{}, false
	}
	return order.ListFilter{
		UserID:       userID,
		AppID:        appID,
		AppScoped:    appScoped,
		Status:       status,
		PayChannelID: strings.TrimSpace(c.Query("pay_channel_id")),
		PayChannel:   strings.TrimSpace(c.Query("pay_channel")),
	}, true
}

// payChannelBrief 是管理台筛选用的渠道元信息（**含停用渠道**）。
type payChannelBrief struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Protocol string   `json:"protocol"`
	Enabled  bool     `json:"enabled"`
	Methods  []string `json:"methods"`
}

// payChannelBriefs 返回全部支付渠道（含停用）。
//
// 管理台订单筛选曾用 /portal/packages 的 pay_options，而那里只有 enabled=true 的渠道 ——
// 渠道一停用就再也筛不出它的历史订单，对账只能翻全量。
func payChannelBriefs() []payChannelBrief {
	all := payment.Channels()
	out := make([]payChannelBrief, 0, len(all))
	for _, ch := range all {
		out = append(out, payChannelBrief{
			ID: ch.ID, Name: ch.Name, Protocol: ch.Protocol, Enabled: ch.Enabled, Methods: ch.Methods,
		})
	}
	return out
}

// adminOrderListResult 与 pagination.Result 的字段名保持一致，额外带渠道元信息。
type adminOrderListResult struct {
	Items       []order.View      `json:"items"`
	Total       int64             `json:"total"`
	Page        int               `json:"page"`
	PageSize    int               `json:"page_size"`
	PayChannels []payChannelBrief `json:"pay_channels"`
}

// AdminOrderList 处理 GET /api/admin/orders：全量订单（分页 + user_id/status/pay_channel 筛选）。
func AdminOrderList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	f, ok := orderFilterOf(c)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	f.Offset, f.Limit = p.Offset(), p.Limit()
	items, total, err := order.New(db.DB).List(c.Request.Context(), f)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 补齐下单用户名（管理台需要按账号定位订单）
	names := orderUsernames(items)
	for i := range items {
		items[i].Username = names[items[i].UserID]
	}
	// 套餐名（同一批 IN 一次查回，避免 N+1）
	pkgs := packageNames(items)
	for i := range items {
		items[i].PackageName = pkgs[items[i].PackageID]
	}
	if err := fillAppNames(c.Request.Context(), items); err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, adminOrderListResult{
		Items: items, Total: total, Page: p.Page, PageSize: p.PageSize,
		PayChannels: payChannelBriefs(),
	})
}

// orderUsernames 批量补齐订单下单用户名（IN 一次查回，避免 N+1；查询失败留空不阻断列表）。
func orderUsernames(items []order.View) map[uint64]string {
	names := make(map[uint64]string, len(items))
	ids := make([]uint64, 0, len(items))
	for _, it := range items {
		if _, ok := names[it.UserID]; !ok {
			names[it.UserID] = ""
			ids = append(ids, it.UserID)
		}
	}
	if len(ids) > 0 {
		var rows []model.User
		if err := db.DB.Model(&model.User{}).Select("id, username").Where("id IN ?", ids).Find(&rows).Error; err == nil {
			for _, u := range rows {
				names[u.ID] = u.Username
			}
		}
	}
	return names
}

// packageNames 批量补齐订单的套餐名（IN 一次查回，避免 N+1）。
//
// 必须 Unscoped：套餐被软删（下架）后，历史订单仍要能显示买的是哪一档 ——
// 用户问「我这笔买的是什么」时，「已删除套餐」比「空字符串」有用得多。
// 查询失败只留空，不阻断列表（与用户名补全同口径）。
func packageNames(items []order.View) map[uint64]string {
	names := make(map[uint64]string, len(items))
	ids := make([]uint64, 0, len(items))
	for _, it := range items {
		if it.PackageID == 0 {
			continue
		}
		if _, ok := names[it.PackageID]; !ok {
			names[it.PackageID] = ""
			ids = append(ids, it.PackageID)
		}
	}
	if len(ids) > 0 {
		var rows []model.Package
		if err := db.DB.Unscoped().Model(&model.Package{}).
			Select("id, name").Where("id IN ?", ids).Find(&rows).Error; err == nil {
			for _, p := range rows {
				names[p.ID] = p.Name
			}
		}
	}
	return names
}

// fillAppNames 批量补齐订单所属应用的展示名（IN 一次查回，无 N+1）。
//
// 口径与卡片一致：display_name 优先、回落后台名（见 appref.Names）。
// app_id=0 的行（平台直连）不查库、AppName 保持空串 —— 前端与 CSV 都据
// AppID==0 显示「平台直连」，而不是靠一个魔法字符串。
//
// 与 orderUsernames/packageNames 的差别：这两个函数查询失败只留空（名称缺失
// 不影响对账），而应用名失败会**阻断**列表 —— 应用列是本轮需求的主角，
// 静默变空等于功能没做，用户却看不出任何错误。
func fillAppNames(ctx context.Context, items []order.View) error {
	ids := make([]uint64, 0, len(items))
	for _, it := range items {
		if it.AppID != 0 {
			ids = append(ids, it.AppID)
		}
	}
	refs, err := appref.Names(ctx, db.DB, ids)
	if err != nil {
		return err
	}
	for i := range items {
		if items[i].AppID == 0 {
			continue
		}
		items[i].AppName = refs[items[i].AppID].Name
	}
	return nil
}

// AdminOrderExport 处理 GET /api/admin/orders/export：按当前筛选导出全部匹配订单 CSV（不分页，无数据返回空表头）。
func AdminOrderExport(c *gin.Context) {
	f, ok := orderFilterOf(c)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	f.Offset, f.Limit = 0, orderExportLimit
	items, _, err := order.New(db.DB).List(c.Request.Context(), f)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	names := orderUsernames(items)
	pkgs := packageNames(items)
	for i := range items {
		items[i].PackageName = pkgs[items[i].PackageID]
	}
	// 导出与列表必须同源：名字都按同一批 id 补齐，否则「列表看到应用名、
	// 导出却是空白」这种不一致只能靠人工发现。
	if err := fillAppNames(c.Request.Context(), items); err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	writeCSV(c, "orders.csv", orderCSV(items, names))
}

// orderCSV 组装订单导出 CSV。
//
// 新增渠道 ID / 协议 / 支付方式三列：渠道展示名是下单时的快照（可能改过名、渠道也可能已停用），
// 对账必须能按稳定 ID 与协议定位到具体网关。
//
// 金额列去掉了「(元)」后缀并新增「币种」列：多币种并存后，一份导出里
// 可能有 ¥72 也有 $72，表头写死单位等于让文件撒谎。这是**破坏性变更**，
// 按表头取列的旧对账脚本会失效（已在发布说明与运维文档标注）。
// 金额仍用 payment.FormatCentToCNY 输出，不经 float（`%.2f` 对 0.005 之类的
// 舍入依赖二进制表示）。
func orderCSV(items []order.View, names map[uint64]string) string {
	var b strings.Builder
	// 到账金额只有一列：旧表里的「额度 / 比例 / 倍率」三列是同一个数的三种写法，
	// 对账时反而要先乘回去。现在库里只存一个最终金额（微），直接输出即可。
	// 应用两列**追加在尾部**：对账脚本普遍按索引取列，插在中间等于静默错列；
	// 追加只影响"多读一列"，前 17 列的位置一字未动。
	b.WriteString("订单号,用户ID,用户名,金额,币种,到账金额,套餐,状态,退款方式,退款状态,退款单号,支付渠道,渠道ID,协议,支付方式,创建时间,支付时间,应用ID,应用\n")
	for _, it := range items {
		paidAt := ""
		if it.PaidAt != nil {
			paidAt = it.PaidAt.Format(time.RFC3339)
		}
		cur := it.Currency
		if cur == "" {
			cur = conf.CurrencyCNY
		}
		b.WriteString(logs.Esc(it.OutTradeNo) + ",")
		b.WriteString(strconv.FormatUint(it.UserID, 10) + ",")
		b.WriteString(logs.Esc(names[it.UserID]) + ",")
		b.WriteString(payment.FormatCentToCNY(it.AmountCent) + ",")
		b.WriteString(cur + ",")
		b.WriteString(money.Format(it.Credited, 2) + ",")
		b.WriteString(logs.Esc(it.PackageName) + ",")
		b.WriteString(it.Status + ",")
		b.WriteString(logs.Esc(it.RefundMode) + ",")
		b.WriteString(logs.Esc(it.RefundStatus) + ",")
		b.WriteString(logs.Esc(it.RefundNo) + ",")
		b.WriteString(logs.Esc(it.PayChannel) + ",")
		b.WriteString(logs.Esc(it.PayChannelID) + ",")
		b.WriteString(it.PayProtocol + ",")
		// 支付方式列输出中文：历史订单存原文、新订单存枚举 code，直接写 code 运营看不懂。
		b.WriteString(logs.Esc(payment.MethodLabel(it.PayMethod)) + ",")
		b.WriteString(it.CreatedAt.Format(time.RFC3339) + ",")
		b.WriteString(paidAt + ",")
		b.WriteString(strconv.FormatUint(it.AppID, 10) + ",")
		// 0 一律写「平台直连」：导出文件是给非技术对账人员看的，
		// 空白单元格会被读成"这条数据缺字段"。
		if it.AppID == 0 {
			b.WriteString("平台直连\n")
		} else {
			b.WriteString(logs.Esc(it.AppName) + "\n")
		}
	}
	return b.String()
}

// orderRefundReq 是退款入参。
//
// Mode：空 = 自动（协议支持原路退款且订单仍在 paid → gateway，否则 balance）；
// gateway = 原路退回；balance = 仅回退平台余额（上游已线下退款 / 协议不支持）。
type orderRefundReq struct {
	OrderID uint64 `json:"order_id"`
	Reason  string `json:"reason"`
	Mode    string `json:"mode"`
}

// orderReasonMax 是退款/入账原因的字符上限（写审计与订单备注都够用）。
const orderReasonMax = 200

// AdminOrderRefund 处理 POST /api/admin/order/refund：退款（原路退回或仅回退余额）。
//
// 返回的是**结果**而不是单纯的成功/失败：退款可能「已受理但结果未定」（refunding），
// 管理台必须能区分「已退款 / 处理中 / 已回滚」，否则运营会重复点击。
func AdminOrderRefund(c *gin.Context) {
	var req orderRefundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
	if req.OrderID == 0 || req.Reason == "" || len([]rune(req.Reason)) > orderReasonMax {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if req.Mode != "" && req.Mode != order.RefundModeGateway && req.Mode != order.RefundModeBalance {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	out, err := order.New(db.DB).Refund(c.Request.Context(), order.RefundInput{
		OrderID: req.OrderID, Reason: req.Reason, Mode: req.Mode,
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionOrderRefund, "order",
		strconv.FormatUint(req.OrderID, 10), gin.H{
			"reason": req.Reason, "mode": out.Mode,
			"refund_status": out.RefundStatus, "refund_no": out.RefundNo,
		})
	response.OK(c, out)
}

type orderRefundConfirmReq struct {
	OrderID uint64 `json:"order_id"`
	Result  string `json:"result"` // success = 确认已退款；failed = 确认退款失败并回滚额度
	Reason  string `json:"reason"`
	// Evidence 是「上游核实凭证」：网关后台的订单号/退款单号/工单号。
	//
	// 确认 failed 会**无条件把额度还回用户**（refundRevertBalanceTx）。如果实际上游
	// 已经退款成功，这一次确认就等于「钱退了 + 额度也留着」。凭一个理由文本框
	// 无法约束这种误操作，所以失败确认必须留下可回查的外部凭据。
	Evidence string `json:"evidence"`
}

// AdminOrderRefundConfirm 处理 POST /api/admin/order/refund/confirm：人工确认退款结果。
//
// 存在的理由：虎皮椒没有退款查询接口，结果只靠回调。回调丢失或响应不可解析时，
// 订单会停在 refunding（额度已扣），必须有带原因、带审计的人工出口，
// 而不是让运维直接改库。
func AdminOrderRefundConfirm(c *gin.Context) {
	var req orderRefundConfirmReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	req.Result = strings.ToLower(strings.TrimSpace(req.Result))
	req.Reason = strings.TrimSpace(req.Reason)
	if req.OrderID == 0 || (req.Result != order.RefundStateSuccess && req.Result != order.RefundStateFailed) {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if req.Reason == "" || len([]rune(req.Reason)) > orderReasonMax {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	req.Evidence = strings.TrimSpace(req.Evidence)
	if len([]rune(req.Evidence)) > orderReasonMax {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 确认 failed 会回滚额度（把额度还回去）。没有外部凭据就是纯口述，
	// 一旦上游其实已经退成功，这一次确认直接造成「钱退了、额度还在」的双重损失。
	if req.Result == order.RefundStateFailed && req.Evidence == "" {
		response.FailDef(c, errs.RefundEvidenceRequired)
		return
	}
	if err := order.New(db.DB).ConfirmRefund(c.Request.Context(), order.RefundConfirmInput{
		OrderID: req.OrderID, Result: req.Result, Reason: req.Reason, Evidence: req.Evidence,
	}); err != nil {
		respondErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionOrderRefundConfirm, "order",
		strconv.FormatUint(req.OrderID, 10),
		gin.H{"result": req.Result, "reason": req.Reason, "evidence": req.Evidence})
	response.OK(c, nil)
}

// AdminOrderManualCredit 处理 POST /api/admin/order/manual-credit：人工入账（need_manual 丢单兜底，原因必填 + 审计）。
func AdminOrderManualCredit(c *gin.Context) {
	var req orderRefundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.OrderID == 0 || req.Reason == "" || len(req.Reason) > 200 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if err := order.New(db.DB).ManualCredit(c.Request.Context(), req.OrderID, req.Reason); err != nil {
		respondErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionOrderManualCredit, "order",
		strconv.FormatUint(req.OrderID, 10), gin.H{"reason": req.Reason})
	response.OK(c, nil)
}

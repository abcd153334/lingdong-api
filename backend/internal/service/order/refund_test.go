package order

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/service/payment"
)

// setupXunhuChannel 写入一个虎皮椒渠道（网关地址不可达）。
//
// 单测**故意**不 mock 出站：净卫策略会拒绝回环地址，
// 于是 payment.Refund 必然返回「结果未知」—— 这正是我们要覆盖的分支
// （真实环境里的超时/连接失败走的是同一条路径）。
func setupXunhuChannel() payment.Channel {
	ch := payment.Channel{
		ID: "xunhupay", Name: "虎皮椒", Protocol: "xunhupay", Enabled: true,
		Methods: []string{"聚合支付"},
		Params: payment.ChannelParams{
			PayAddress: "https://api.xunhupay.com/payment/do.html",
			AppID:      "2019061201", AppSecret: "secret",
		},
	}
	setCfg("pay_channels", []payment.Channel{ch})
	return ch
}

// seedPaidOrder 直接造一笔已支付订单（避开上游下单，退款用例只关心退款状态机）。
func seedPaidOrder(t *testing.T, gdb *gorm.DB, userID uint64, amountCent, quota int64) *model.Order {
	t.Helper()
	exp := time.Now().Add(-time.Minute)
	suffix := time.Now().UnixNano()
	o := &model.Order{
		UserID: userID, OutTradeNo: "LDRF" + strconv.FormatInt(suffix, 10),
		AmountCent: amountCent, Credited: quota, Status: "paid",
		PayChannel: "虎皮椒-聚合支付", PayChannelID: "xunhupay",
		PayMethod: "聚合支付", PayProtocol: "xunhupay",
		ChannelOrderID: "OPEN" + strconv.FormatInt(suffix, 10),
		Subject:        "充值-体验", ExpireAt: &exp,
	}
	if err := gdb.Create(o).Error; err != nil {
		t.Fatalf("造已支付订单失败: %v", err)
	}
	// 必须补一条 recharge 流水：真实链路里「已支付」与「已入账」是同一次回调写的
	// （notify.go 用 RefType=order / RefID=<订单ID> 记账）。只造订单不造流水，
	// 这个夹具就对应「从未入账的 need_manual 单」—— 而退款在那种单上**不该扣额度**
	// （见 refund.go 的 credited 判据），用例期望却相反，等于在测一个不存在的状态。
	var u model.User
	if err := gdb.Select("balance").First(&u, userID).Error; err != nil {
		t.Fatalf("读用户余额失败: %v", err)
	}
	txn := &model.Transaction{
		UserID: userID, Type: "recharge", Amount: quota, BalanceAfter: u.Balance,
		RefType: "order", RefID: strconv.FormatUint(o.ID, 10), Reason: "充值到账",
	}
	if err := gdb.Create(txn).Error; err != nil {
		t.Fatalf("造充值流水失败: %v", err)
	}
	return o
}

// refundNotifyOf 造一条退款类回调（amout=0：退款回调允许不带金额）。
func refundNotifyOf(outTradeNo string, kind payment.NotifyKind) *payment.Notification {
	return &payment.Notification{OutTradeNo: outTradeNo, Kind: kind, Raw: map[string]string{"status": "CD"}}
}

// TestRefundGatewayUnknownThenNotifyConfirm 覆盖原路退款的完整状态机：
// 上游不可达 → refunding（不判失败、不回滚）→ 回调 CD → refunded（不再动额度）。
func TestRefundGatewayUnknownThenNotifyConfirm(t *testing.T) {
	gdb := withTestDB(t)
	ch := setupXunhuChannel()
	ctx := context.Background()
	svc := New(gdb)

	const quota = int64(694440)
	u := seedUser(t, gdb, quota)
	o := seedPaidOrder(t, gdb, u.ID, 1000, quota)

	out, err := svc.Refund(ctx, RefundInput{OrderID: o.ID, Reason: "重复支付"})
	if err != nil {
		t.Fatalf("退款调用不应直接报错（应为未知态）: %v", err)
	}
	if out.Mode != RefundModeGateway {
		t.Fatalf("虎皮椒已支付订单应走原路退款，实际 %s", out.Mode)
	}
	if out.RefundStatus != RefundStatePending {
		t.Fatalf("上游不可达时必须停留在待确认，实际 %s", out.RefundStatus)
	}

	var got model.Order
	_ = gdb.First(&got, o.ID)
	var user model.User
	_ = gdb.First(&user, u.ID)
	if got.Status != "refunding" || got.RefundStatus != RefundStatePending {
		t.Fatalf("订单状态不符: status=%s refund=%s", got.Status, got.RefundStatus)
	}
	if user.Balance != 0 {
		t.Fatalf("发起退款必须先扣额度，实际余额 %d", user.Balance)
	}
	if got.RefundError == "" {
		t.Fatal("结果未知必须留下原因，否则运维无从排查")
	}

	// 重复提交：在途退款不允许再发一次
	_, err = svc.Refund(ctx, RefundInput{OrderID: o.ID, Reason: "再来一次"})
	wantErr(t, err, errs.OrderRefunding)

	// 上游回调 CD：终结为已退款，**额度不得再次变动**（发起时已扣）
	if r := svc.settle(ctx, ch, refundNotifyOf(o.OutTradeNo, payment.NotifyRefunded), "test"); r != NotifyDone {
		t.Fatalf("CD 回调应处理成功: %v", r)
	}
	_ = gdb.First(&got, o.ID)
	_ = gdb.First(&user, u.ID)
	if got.Status != "refunded" || got.RefundStatus != RefundStateSuccess || got.RefundDoneAt == nil {
		t.Fatalf("CD 后状态不符: status=%s refund=%s", got.Status, got.RefundStatus)
	}
	if user.Balance != 0 {
		t.Fatalf("CD 回调不得再次扣额度，实际余额 %d", user.Balance)
	}
	var revert int64
	_ = gdb.Model(&model.Transaction{}).Where("user_id = ? AND type = ?", u.ID, "refund_revert").
		Count(&revert)
	if revert != 0 {
		t.Fatalf("成功的退款不应产生冲正流水，实际 %d 条", revert)
	}
}

// TestRefundNotifyFailedRevertsQuota 覆盖 UD：回滚额度恢复 paid，且重复回调不重复回滚。
func TestRefundNotifyFailedRevertsQuota(t *testing.T) {
	gdb := withTestDB(t)
	ch := setupXunhuChannel()
	ctx := context.Background()
	svc := New(gdb)

	const quota = int64(500000)
	u := seedUser(t, gdb, quota)
	o := seedPaidOrder(t, gdb, u.ID, 7200, quota)

	if _, err := svc.Refund(ctx, RefundInput{OrderID: o.ID, Reason: "用户申请"}); err != nil {
		t.Fatalf("发起退款失败: %v", err)
	}
	var user model.User
	_ = gdb.First(&user, u.ID)
	if user.Balance != 0 {
		t.Fatalf("发起后应扣光余额，实际 %d", user.Balance)
	}

	n := refundNotifyOf(o.OutTradeNo, payment.NotifyRefundFailed)
	if r := svc.settle(ctx, ch, n, "test"); r != NotifyDone {
		t.Fatalf("UD 回调应处理成功: %v", r)
	}
	var got model.Order
	_ = gdb.First(&got, o.ID)
	_ = gdb.First(&user, u.ID)
	if got.Status != "paid" || got.RefundStatus != RefundStateFailed {
		t.Fatalf("UD 后状态不符: status=%s refund=%s", got.Status, got.RefundStatus)
	}
	if user.Balance != quota {
		t.Fatalf("额度应全额回滚，实际 %d（期望 %d）", user.Balance, quota)
	}

	// 重复 UD：状态已不在 refunding，不得再次回滚
	if r := svc.settle(ctx, ch, n, "test"); r != NotifyDone {
		t.Fatalf("重复 UD 应幂等成功: %v", r)
	}
	_ = gdb.First(&user, u.ID)
	if user.Balance != quota {
		t.Fatalf("重复回调导致重复回滚: %d", user.Balance)
	}
}

// TestRefundRetryAfterFailedRejected 退款失败之后再发起退款，必须由状态机直接拒绝。
//
// 失败后 onRefundFailed 已把额度全额回滚、status 退回 paid，订单看起来「又可以退了」。
// 但退款流水已经存在（uq_transactions_order_refund 一单一条），再退一次要么撞索引
// 变 500，要么在没建索引的库（只跑 AutoMigrate 的环境）真的再扣一次已还回的额度。
// 所以判据必须是状态机里的 refund_status=failed，而不是「等数据库报 23505」。
func TestRefundRetryAfterFailedRejected(t *testing.T) {
	gdb := withTestDB(t)
	ch := setupXunhuChannel()
	ctx := context.Background()
	svc := New(gdb)

	const quota = int64(500000)
	u := seedUser(t, gdb, quota)
	o := seedPaidOrder(t, gdb, u.ID, 6300, quota)

	if _, err := svc.Refund(ctx, RefundInput{OrderID: o.ID, Reason: "用户申请"}); err != nil {
		t.Fatalf("发起退款失败: %v", err)
	}
	if r := svc.settle(ctx, ch, refundNotifyOf(o.OutTradeNo, payment.NotifyRefundFailed), "test"); r != NotifyDone {
		t.Fatalf("退款失败回调应处理成功: %v", r)
	}

	// 两种模式都必须被拒：原路退款会重复扣额度，余额模式会扣一次「已经还回去」的额度。
	for _, mode := range []string{"", RefundModeGateway, RefundModeBalance} {
		_, err := svc.Refund(ctx, RefundInput{OrderID: o.ID, Mode: mode, Reason: "再试一次"})
		if err == nil {
			t.Fatalf("mode=%q 在上一笔退款失败后必须被拒", mode)
		}
		if d, ok := errs.AsDef(err); !ok || d.Code != errs.InvalidParam.Code {
			t.Fatalf("mode=%q 应归类为 invalid_param（4xx 可读文案），实际: %v", mode, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "上一次原路退款已失败") {
			t.Fatalf("mode=%q 的文案必须说清「上一次已失败、额度已回滚」，实际: %q", mode, msg)
		}
	}

	// 余额仍是回滚后的全额：被拒的退款一分都不能动。
	var user model.User
	_ = gdb.First(&user, u.ID)
	if user.Balance != quota {
		t.Fatalf("被拒的退款不得动余额，实际 %d（期望 %d）", user.Balance, quota)
	}
}

// TestConfirmRefund 覆盖人工确认两个方向：确认已退款（不回滚）与确认失败（回滚额度）。
func TestConfirmRefund(t *testing.T) {
	gdb := withTestDB(t)
	setupXunhuChannel()
	ctx := context.Background()
	svc := New(gdb)

	// ① 确认已退款：额度保持已扣，状态终态
	const quota = int64(300000)
	u1 := seedUser(t, gdb, quota)
	o1 := seedPaidOrder(t, gdb, u1.ID, 500, quota)
	if _, err := svc.Refund(ctx, RefundInput{OrderID: o1.ID, Reason: "重复支付"}); err != nil {
		t.Fatalf("发起退款失败: %v", err)
	}
	if err := svc.ConfirmRefund(ctx, RefundConfirmInput{
		OrderID: o1.ID, Result: RefundStateSuccess, Reason: "网关后台显示已退款",
	}); err != nil {
		t.Fatalf("确认已退款失败: %v", err)
	}
	var got1 model.Order
	var user model.User
	_ = gdb.First(&got1, o1.ID)
	_ = gdb.First(&user, u1.ID)
	if got1.Status != "refunded" || got1.RefundStatus != RefundStateSuccess {
		t.Fatalf("确认已退款后状态不符: %s/%s", got1.Status, got1.RefundStatus)
	}
	if user.Balance != 0 {
		t.Fatalf("确认已退款不应退还额度，实际 %d", user.Balance)
	}

	// ② 确认失败：额度回滚、订单回到 paid
	const quota2 = int64(200000)
	u2 := seedUser(t, gdb, quota2)
	o2 := seedPaidOrder(t, gdb, u2.ID, 300, quota2)
	if _, err := svc.Refund(ctx, RefundInput{OrderID: o2.ID, Reason: "重复支付"}); err != nil {
		t.Fatalf("发起退款失败: %v", err)
	}
	// 缺「上游核实凭证」必须拒绝：确认 failed 会**无条件**把额度还回去，
	// 一旦上游其实已经退成功，这一次确认就是「钱退了、额度还在」的双重损失。
	wantErr(t, svc.ConfirmRefund(ctx, RefundConfirmInput{
		OrderID: o2.ID, Result: RefundStateFailed, Reason: "网关后台无退款记录",
	}), errs.RefundEvidenceRequired)

	if err := svc.ConfirmRefund(ctx, RefundConfirmInput{
		OrderID: o2.ID, Result: RefundStateFailed, Reason: "网关后台无退款记录",
		Evidence: "虎皮椒后台订单 20251027001 显示「未退款」",
	}); err != nil {
		t.Fatalf("确认退款失败处理失败: %v", err)
	}
	var got2 model.Order
	var user2 model.User
	_ = gdb.First(&got2, o2.ID)
	_ = gdb.First(&user2, u2.ID)
	if got2.Status != "paid" || got2.RefundStatus != RefundStateFailed {
		t.Fatalf("确认失败后状态不符: %s/%s", got2.Status, got2.RefundStatus)
	}
	if user2.Balance != quota2 {
		t.Fatalf("确认失败应回滚余额，实际 %d（期望 %d）", user2.Balance, quota2)
	}

	// ③ 非 refunding 订单不能确认
	err := svc.ConfirmRefund(ctx, RefundConfirmInput{OrderID: o1.ID, Result: RefundStateSuccess, Reason: "x"})
	wantErr(t, err, errs.RefundNotPending)
}

// TestRefundUpstreamManualRefundNeedsManual 上游后台手工退款（本地未发起）：
// 只能转人工，**绝不能**自动扣额度（额度可能已被消费）。
//
// 关键点：转人工的载体是 refund_status=upstream_only，**不是** status=need_manual。
// need_manual 不在「支付成功」的终态短路集合里，网关重放一次支付成功就会再走
// onPaid → CreditTx，同一笔钱入账两次。
func TestRefundUpstreamManualRefundNeedsManual(t *testing.T) {
	gdb := withTestDB(t)
	ch := setupXunhuChannel()
	ctx := context.Background()
	svc := New(gdb)

	const quota = int64(400000)
	u := seedUser(t, gdb, quota)
	o := seedPaidOrder(t, gdb, u.ID, 800, quota)

	if r := svc.settle(ctx, ch, refundNotifyOf(o.OutTradeNo, payment.NotifyRefunded), "test"); r != NotifyNeedManual {
		t.Fatalf("本地未发起的退款应转人工: %v", r)
	}
	var got model.Order
	_ = gdb.First(&got, o.ID)
	var user model.User
	_ = gdb.First(&user, u.ID)
	if user.Balance != quota {
		t.Fatalf("转人工时不得自动扣额度，实际 %d", user.Balance)
	}
	if got.Status != "paid" {
		t.Fatalf("支付事实不得被改：期望 status=paid，实际 %s", got.Status)
	}
	if got.RefundStatus != RefundStateUpstreamOnly {
		t.Fatalf("应标记 refund_status=%s，实际 %q", RefundStateUpstreamOnly, got.RefundStatus)
	}

	// 重放一次「支付成功」：必须幂等短路，不能再入账一次。
	paidNotify := &payment.Notification{
		Kind: payment.NotifyPaid, OutTradeNo: o.OutTradeNo,
		TradeNo: "REPLAY-" + o.OutTradeNo, AmountCent: o.AmountCent,
	}
	if r := svc.settle(ctx, ch, paidNotify, "test-replay"); r != NotifyDone {
		t.Fatalf("重放支付成功应幂等成功: %v", r)
	}
	var afterReplay model.User
	_ = gdb.First(&afterReplay, u.ID)
	if afterReplay.Balance != quota {
		t.Fatalf("重放支付成功不得二次入账，实际 %d（期望 %d）", afterReplay.Balance, quota)
	}

	// 上游已经退过款：原路退款必须拒绝，只能「仅回退平台余额」。
	_, err := svc.Refund(ctx, RefundInput{OrderID: o.ID, Reason: "上游已退款，扣回额度", Mode: RefundModeGateway})
	wantErr(t, err, errs.RefundModeInvalid)

	out, err := svc.Refund(ctx, RefundInput{OrderID: o.ID, Reason: "上游已退款，扣回额度", Mode: RefundModeBalance})
	if err != nil {
		t.Fatalf("人工态回退余额应成功: %v", err)
	}
	if out.RefundStatus != RefundStateSuccess {
		t.Fatalf("回退余额应为成功态: %+v", out)
	}
	_ = gdb.First(&user, u.ID)
	if user.Balance != 0 {
		t.Fatalf("回退余额后额度应为 0，实际 %d", user.Balance)
	}
	// 补扣额度不能把「已经退回渠道多少钱」抹成 0：这笔钱运营确实在网关后台退过，
	// 对账与客诉都以 refund_amount_cent 为准，写成 0 就等于谎报「没退过」。
	if out.AmountCent != o.AmountCent {
		t.Fatalf("upstream_only 补扣必须保留原路退款金额 %d，实际 %d", o.AmountCent, out.AmountCent)
	}
	_ = gdb.First(&got, o.ID)
	if got.RefundAmountCent != o.AmountCent {
		t.Fatalf("落库的 refund_amount_cent 应为 %d，实际 %d", o.AmountCent, got.RefundAmountCent)
	}
	if got.RefundCredited != quota {
		t.Fatalf("落库的 refund_credited 应为扣回额度 %d，实际 %d", quota, got.RefundCredited)
	}
}

// TestRefundPendingOrderRejected 未支付订单不可退款（含 pending / expired）。
func TestRefundPendingOrderRejected(t *testing.T) {
	gdb := withTestDB(t)
	setupXunhuChannel()
	ctx := context.Background()
	svc := New(gdb)

	u := seedUser(t, gdb, 0)
	exp := time.Now().Add(time.Minute)
	o := &model.Order{
		UserID: u.ID, OutTradeNo: "LDRFP" + strconv.FormatInt(time.Now().UnixNano(), 10),
		AmountCent: 100, Credited: 100, Status: "pending",
		PayChannelID: "xunhupay", PayProtocol: "xunhupay", ExpireAt: &exp,
	}
	if err := gdb.Create(o).Error; err != nil {
		t.Fatalf("造 pending 订单失败: %v", err)
	}
	_, err := svc.Refund(ctx, RefundInput{OrderID: o.ID, Reason: "退款"})
	wantErr(t, err, errs.OrderNotPaid)
}

// wantErr 断言 err 是带指定错误码的业务错误。
//
// 用 errs.AsDef 而不是比较错误字符串：文案会改，错误码是契约
// （前端与网关按 code 分支），断言文案的测试会在改一句提示语时误报。
func wantErr(t *testing.T, err error, def errs.Def) {
	t.Helper()
	if err == nil {
		t.Fatalf("应返回 %s，实际 nil", def.Code)
	}
	got, ok := errs.AsDef(err)
	if !ok {
		t.Fatalf("应返回 errs.Def，实际 %T: %v", err, err)
	}
	if got.Code != def.Code {
		t.Fatalf("错误码应为 %s，实际 %s", def.Code, got.Code)
	}
}

// TestRefundNotifyUnknownKeepsRefundingAndQuota 守住微信 REFUND.ABNORMAL 的处置。
//
// 这是本次修复唯一的资金正确性护栏。ABNORMAL 表示「原路退回银行卡失败（卡注销/冻结）、
// 资金停在微信退款中间账户」，**不是终态**，后续会流转成 SUCCESS。若按「退款失败」处理，
// onRefundFailed 会把额度加回用户、订单回 paid；运营随后还会在商户平台把这笔钱人工
// 退给用户 —— 用户既拿平台额度又拿真钱，平台双向损失。
//
// 与 TestRefundNotifyFailedRevertsQuota 的区别：那条测 CLOSED/UD（资金仍在商户，
// 必须回滚），这条测 ABNORMAL（资金已划出，绝不回滚）。两条必须同时存在，
// 否则改错任意一侧都没人发现。
func TestRefundNotifyUnknownKeepsRefundingAndQuota(t *testing.T) {
	gdb := withTestDB(t)
	ch := setupXunhuChannel()
	ctx := context.Background()
	svc := New(gdb)

	const quota = int64(500000)
	u := seedUser(t, gdb, quota)
	o := seedPaidOrder(t, gdb, u.ID, 7200, quota)

	if _, err := svc.Refund(ctx, RefundInput{OrderID: o.ID, Reason: "用户申请"}); err != nil {
		t.Fatalf("发起退款失败: %v", err)
	}
	var user model.User
	_ = gdb.First(&user, u.ID)
	if user.Balance != 0 {
		t.Fatalf("发起后应扣光余额，实际 %d", user.Balance)
	}

	n := refundNotifyOf(o.OutTradeNo, payment.NotifyRefundUnknown)
	if r := svc.settle(ctx, ch, n, "test"); r != NotifyDone {
		t.Fatalf("ABNORMAL 回调应处理成功: %v", r)
	}

	var got model.Order
	_ = gdb.First(&got, o.ID)
	_ = gdb.First(&user, u.ID)

	// ① 状态必须保持 refunding：退款巡检只领 refunding 的单，改了它就再也查不到这笔。
	if got.Status != "refunding" {
		t.Fatalf("ABNORMAL 后必须保持 refunding，实际 %s", got.Status)
	}
	// ② 退款状态保持发起时的 pending，不能写成 failed（那是「钱没退」的语义）。
	if got.RefundStatus != RefundStatePending {
		t.Fatalf("退款状态应保持 pending，实际 %s", got.RefundStatus)
	}
	// ③ 绝不回滚额度。
	if user.Balance != 0 {
		t.Fatalf("ABNORMAL 绝不能回滚额度，实际余额 %d", user.Balance)
	}
	// ④ 不写 refund_done_at：它不是终态，写了管理台会误以为已结案。
	if got.RefundDoneAt != nil {
		t.Fatal("ABNORMAL 不是终态，不应写 refund_done_at")
	}
	// ⑤ 不产生冲正流水。
	var revert int64
	if err := gdb.Model(&model.Transaction{}).
		Where("user_id = ? AND type = ?", u.ID, model.TxnRefundRevert).Count(&revert).Error; err != nil {
		t.Fatalf("统计冲正流水失败: %v", err)
	}
	if revert != 0 {
		t.Fatalf("ABNORMAL 不应产生冲正流水，实际 %d 条", revert)
	}
	// ⑥ 必须留下运营可见的人工待办原因。
	if got.RefundError == "" {
		t.Fatal("ABNORMAL 必须写 refund_error，否则运营无从知道要去商户平台处理")
	}

	// 重复推送幂等：状态与余额都不再变化。
	if r := svc.settle(ctx, ch, n, "test"); r != NotifyDone {
		t.Fatalf("重复 ABNORMAL 应幂等成功: %v", r)
	}
	_ = gdb.First(&user, u.ID)
	if user.Balance != 0 {
		t.Fatalf("重复 ABNORMAL 不应改变余额，实际 %d", user.Balance)
	}
}

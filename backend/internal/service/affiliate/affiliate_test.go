package affiliate

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/affcfg"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/money"
)

// ---------- 数据库集成用例（需 LD_TEST_DSN；未设置则跳过） ----------

const testSchema = "ld_test_aff_v3"

func withTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + testSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+testSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// migrateAffTables 迁移本包用到的表，并补上生产环境由 db/indexes.go 建的唯一索引。
//
// `INSERT ... ON CONFLICT (order_id, role) DO NOTHING` 依赖这条唯一约束：
// 缺了它会直接报「there is no unique or exclusion constraint matching the
// ON CONFLICT」，而不是退化成普通插入 —— 所以测试里必须显式建，否则并发用例全是假绿。
func migrateAffTables(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	if err := gdb.AutoMigrate(&model.User{}, &model.Transaction{}, &model.AffiliateReward{}, &model.Order{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_affiliate_rewards_order_role
		ON affiliate_rewards (order_id, role)`).Error; err != nil {
		t.Fatalf("建唯一索引失败: %v", err)
	}
}

func setCfg(key string, v any) {
	raw, _ := json.Marshal(v)
	conf.System().Set(key, raw)
}

// baseCfg 是本包用例的默认配置：10% × 前 3 笔 + 每笔 5 元、冷却 48h、门槛 0.1 元。
func baseCfg() Config {
	return Config{Settings: affcfg.Settings{
		Enabled: true, InviterRate: 100_000, InviteeAmount: 5_000_000,
		MaxRewards: 3, HoldHours: 48, MinTransfer: 100_000, DailyInviteCap: 20,
	}}
}

// setAff 写入配置并在用例结束时恢复默认，避免用例之间互相污染。
func setAff(t *testing.T, c Config) {
	t.Helper()
	setCfg("aff", c)
	t.Cleanup(func() { setCfg("aff", baseCfg()) })
}

// seedSeq 让同一纳秒内的多次造数不撞唯一键。
//
// 只靠 UnixNano 是不够的：Windows 上 time.Now 的粒度可能粗到毫秒级，
// 连续三次调用会拿到同一个值 → 23505 → 用例随机失败（而失败信息指向"造数据"，
// 看起来像数据库问题，实际是造数不唯一）。
var seedSeq atomic.Int64

func seedUser(t *testing.T, gdb *gorm.DB) *model.User {
	t.Helper()
	suffix := strconv.FormatInt(time.Now().UnixNano()+seedSeq.Add(1), 36)
	u := &model.User{Username: "af" + suffix, PasswordHash: "x", Group: "default", AffCode: "A" + strings.ToUpper(suffix)}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造测试用户失败: %v", err)
	}
	return u
}

func reload(t *testing.T, gdb *gorm.DB, id uint64) model.User {
	t.Helper()
	var u model.User
	if err := gdb.First(&u, id).Error; err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	return u
}

// invite 建一个「已绑定邀请人」的被邀请人。
func invite(t *testing.T, gdb *gorm.DB, inviter *model.User) *model.User {
	t.Helper()
	u := seedUser(t, gdb)
	if err := gdb.Model(&model.User{}).Where("id = ?", u.ID).
		Update("inviter_id", inviter.ID).Error; err != nil {
		t.Fatalf("绑定邀请关系失败: %v", err)
	}
	return u
}

// reward 直接走生产路径发一笔返利（在事务里，与充值入账同构）。
func reward(t *testing.T, gdb *gorm.DB, inviteeID, orderID uint64, amountCent int64) {
	t.Helper()
	if err := gdb.Transaction(func(tx *gorm.DB) error {
		return RewardTx(tx, RewardInput{InviteeID: inviteeID, OrderID: orderID, AmountCent: amountCent})
	}); err != nil {
		t.Fatalf("发放返利失败: %v", err)
	}
}

func countRewards(t *testing.T, gdb *gorm.DB, where string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := gdb.Model(&model.AffiliateReward{}).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatalf("统计返利行失败: %v", err)
	}
	return n
}

// legOf 取某订单某一角色的行（一行一受益人，所以必须带 role）。
func legOf(t *testing.T, gdb *gorm.DB, orderID uint64, role string) model.AffiliateReward {
	t.Helper()
	var row model.AffiliateReward
	if err := gdb.Where("order_id = ? AND role = ?", orderID, role).First(&row).Error; err != nil {
		t.Fatalf("查返利行失败 order=%d role=%s: %v", orderID, role, err)
	}
	return row
}

// TestOnRegister 覆盖：有效邀请码绑定（大小写不敏感）；无效码/返利关闭/自我邀请一律不绑定；
// 每日上限生效；邀请码必生成。
func TestOnRegister(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	svc := New(gdb)
	ctx := context.Background()

	inviter := seedUser(t, gdb)
	if err := gdb.Model(&model.User{}).Where("id = ?", inviter.ID).
		Update("aff_code", "INVITE01").Error; err != nil {
		t.Fatalf("写入邀请码失败: %v", err)
	}

	// 小写 + 前后空格：必须能绑上（生成侧恒大写，归一化不一致会让邀请制注册直接 403）
	invitee := seedUser(t, gdb)
	if err := svc.OnRegister(ctx, invitee.ID, " invite01 "); err != nil {
		t.Fatalf("绑定应成功: %v", err)
	}
	eu := reload(t, gdb, invitee.ID)
	if eu.InviterID == nil || *eu.InviterID != inviter.ID {
		t.Fatalf("未绑定邀请人: %+v", eu.InviterID)
	}
	if n := countRewards(t, gdb, "invitee_id = ?", invitee.ID); n != 0 {
		t.Fatalf("注册阶段不应产生返利行: %d", n)
	}

	// 无效邀请码：静默忽略，但仍生成自身邀请码
	orphan := seedUser(t, gdb)
	if err := svc.OnRegister(ctx, orphan.ID, "NOPE1234"); err != nil {
		t.Fatalf("无效码不应报错: %v", err)
	}
	ou := reload(t, gdb, orphan.ID)
	if ou.InviterID != nil {
		t.Fatalf("无效码不应绑定: %+v", ou)
	}
	if ou.AffCode == "" {
		t.Fatal("注册后必须生成自身邀请码")
	}

	// 自我邀请：不绑定
	if err := svc.OnRegister(ctx, inviter.ID, "INVITE01"); err != nil {
		t.Fatalf("自我邀请不应报错: %v", err)
	}
	if after := reload(t, gdb, inviter.ID); after.InviterID != nil {
		t.Fatalf("自我邀请不应绑定: %+v", after)
	}

	// 返利关闭：有效码也不绑定，但邀请码照发
	off := baseCfg()
	off.Enabled = false
	setCfg("aff", off)
	dave := seedUser(t, gdb)
	if err := svc.OnRegister(ctx, dave.ID, "INVITE01"); err != nil {
		t.Fatalf("返利关闭不应报错: %v", err)
	}
	du := reload(t, gdb, dave.ID)
	if du.InviterID != nil {
		t.Fatalf("返利关闭不应绑定: %+v", du)
	}
	if du.AffCode == "" {
		t.Fatal("邀请码必须生成")
	}

	// 每日上限：cap=1 时同一邀请人只能再绑 1 个
	capped := baseCfg()
	capped.DailyInviteCap = 1
	setCfg("aff", capped)
	one := seedUser(t, gdb)
	if err := svc.OnRegister(ctx, one.ID, "INVITE01"); err != nil {
		t.Fatalf("第一笔绑定应成功: %v", err)
	}
	two := seedUser(t, gdb)
	if err := svc.OnRegister(ctx, two.ID, "INVITE01"); err != nil {
		t.Fatalf("超限不应报错（只记原因）: %v", err)
	}
	if u := reload(t, gdb, two.ID); u.InviterID != nil {
		t.Fatalf("超过每日上限仍绑上了: %+v", u.InviterID)
	}
}

// TestRegisterMutualInviteNoDeadlock 互填邀请码并发注册不能死锁。
//
// 旧实现先锁自己再锁邀请人：A 填 B 的码、B 填 A 的码同时注册会以相反顺序取锁，
// PG 会杀掉其中一个（40P01）—— 而调用方 `bindAffiliate` 只记 WARN，
// 表现就是「注册成功但邀请关系丢了」，谁都查不出来。
func TestRegisterMutualInviteNoDeadlock(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	svc := New(gdb)
	ctx := context.Background()

	a := seedUser(t, gdb)
	b := seedUser(t, gdb)
	if err := gdb.Model(&model.User{}).Where("id = ?", a.ID).Update("aff_code", "CODEAAAA").Error; err != nil {
		t.Fatalf("写码失败: %v", err)
	}
	if err := gdb.Model(&model.User{}).Where("id = ?", b.ID).Update("aff_code", "CODEBBBB").Error; err != nil {
		t.Fatalf("写码失败: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errsCh := make(chan error, 2)
	for _, fn := range []func() error{
		func() error { return svc.OnRegister(ctx, a.ID, "CODEBBBB") },
		func() error { return svc.OnRegister(ctx, b.ID, "CODEAAAA") },
	} {
		wg.Add(1)
		go func(f func() error) {
			defer wg.Done()
			<-start
			errsCh <- f()
		}(fn)
	}
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatalf("并发注册不应报错（死锁/序列化失败）: %v", err)
		}
	}

	ua, ub := reload(t, gdb, a.ID), reload(t, gdb, b.ID)
	if ua.InviterID == nil || *ua.InviterID != b.ID {
		t.Fatalf("A 未绑上 B: %+v", ua.InviterID)
	}
	if ub.InviterID == nil || *ub.InviterID != a.ID {
		t.Fatalf("B 未绑上 A: %+v", ub.InviterID)
	}
}

// TestRewardTwoLegsAndBase 一单两行：基数取**实付**、比例快照落库、赠金不超过实付。
func TestRewardTwoLegsAndBase(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg()) // 10%
	inviter := seedUser(t, gdb)
	invitee := invite(t, gdb, inviter)

	reward(t, gdb, invitee.ID, 9001, 10_000) // 10_000 分 = 100 元

	wantBase, _ := money.FromCents(10_000)
	if wantBase != 100_000_000 {
		t.Fatalf("测试前提错了：100 元应为 1e8 微，实际 %d", wantBase)
	}
	inv := legOf(t, gdb, 9001, model.AffRoleInviter)
	if inv.BaseAmount != wantBase {
		t.Fatalf("基数必须是实付 1e8 微（不是 credited），实际 %d", inv.BaseAmount)
	}
	if inv.Amount != 10_000_000 || inv.Rate != 100_000 {
		t.Fatalf("邀请人腿应为 10 元 / 10%%：amount=%d rate=%d", inv.Amount, inv.Rate)
	}
	if inv.BeneficiaryID != inviter.ID || inv.InviteeID != invitee.ID {
		t.Fatalf("邀请人腿归属错误: %+v", inv)
	}
	ie := legOf(t, gdb, 9001, model.AffRoleInvitee)
	if ie.Amount != 5_000_000 || ie.BeneficiaryID != invitee.ID {
		t.Fatalf("被邀请人腿应为 5 元且归付款人: %+v", ie)
	}
}

// TestInviteeAmountCappedByPaid 赠金每笔照发，但每笔不超过该笔实付。
func TestInviteeAmountCappedByPaid(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	cfg := baseCfg()
	cfg.InviteeAmount = 5_000_000 // 5 元/笔
	setAff(t, cfg)
	inviter := seedUser(t, gdb)
	invitee := invite(t, gdb, inviter)

	cases := []struct {
		order  uint64
		cent   int64
		expect int64
	}{
		{9101, 100, 1_000_000},     // 充 1 元 → 拿 1 元（被实付封顶）
		{9102, 1, 10_000},          // 充 0.01 元 → 拿 0.01 元
		{9103, 100_000, 5_000_000}, // 充 1000 元 → 只按配置给 5 元
	}
	for _, c := range cases {
		reward(t, gdb, invitee.ID, c.order, c.cent)
		if got := legOf(t, gdb, c.order, model.AffRoleInvitee).Amount; got != c.expect {
			t.Fatalf("order=%d 赠金应为 %d，实际 %d", c.order, c.expect, got)
		}
	}
}

// TestRewardStopsAfterN 守住「前 N 笔」：按 DISTINCT order_id 计数，第 N+1 笔不落行。
func TestRewardStopsAfterN(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	cfg := baseCfg()
	cfg.MaxRewards = 2
	setAff(t, cfg)
	inviter := seedUser(t, gdb)
	invitee := invite(t, gdb, inviter)

	for i := 0; i < 3; i++ {
		reward(t, gdb, invitee.ID, uint64(9200+i), 10_000)
	}
	// 两单 × 两行 = 4 行；第三单一行都不该有
	if n := countRewards(t, gdb, "invitee_id = ?", invitee.ID); n != 4 {
		t.Fatalf("应只发 2 单（4 行），实际 %d", n)
	}
	if n := countRewards(t, gdb, "invitee_id = ? AND order_id = ?", invitee.ID, 9202); n != 0 {
		t.Fatalf("第 N+1 笔不该落行，实际 %d", n)
	}
}

// TestVoidedOrderFreesSlot 作废释放名额（按「未作废的 DISTINCT order_id」计次）。
func TestVoidedOrderFreesSlot(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	cfg := baseCfg()
	cfg.MaxRewards = 2
	setAff(t, cfg)
	inviter := seedUser(t, gdb)
	invitee := invite(t, gdb, inviter)

	reward(t, gdb, invitee.ID, 9301, 10_000)
	reward(t, gdb, invitee.ID, 9302, 10_000)
	reward(t, gdb, invitee.ID, 9303, 10_000) // 已满 2 单 → 不发
	if n := countRewards(t, gdb, "invitee_id = ?", invitee.ID); n != 4 {
		t.Fatalf("满额后不应再发，实际 %d 行", n)
	}

	var voided Voided
	if err := gdb.Transaction(func(tx *gorm.DB) error {
		var e error
		voided, e = VoidTx(tx, 9301)
		return e
	}); err != nil {
		t.Fatalf("作废失败: %v", err)
	}
	if voided.Rows != 2 {
		t.Fatalf("一单两行应一起作废，实际 %d", voided.Rows)
	}

	reward(t, gdb, invitee.ID, 9304, 10_000)
	var live int64
	if err := gdb.Model(&model.AffiliateReward{}).
		Where("invitee_id = ? AND NOT voided", invitee.ID).
		Distinct("order_id").Count(&live).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if live != 2 {
		t.Fatalf("作废后未释放名额，未作废订单数 %d", live)
	}
	if n := countRewards(t, gdb, "invitee_id = ?", invitee.ID); n != 6 {
		t.Fatalf("总行数应为 6（含作废 2 行），实际 %d", n)
	}
}

// TestInfoAndRecords 覆盖：三段派生额、记录脱敏与角色。
func TestInfoAndRecords(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	svc := New(gdb)
	ctx := context.Background()

	inviter := seedUser(t, gdb)
	if err := gdb.Model(&model.User{}).Where("id = ?", inviter.ID).
		Update("aff_code", "MYCODE01").Error; err != nil {
		t.Fatalf("写入邀请码失败: %v", err)
	}
	for i := 0; i < 2; i++ {
		invite(t, gdb, inviter)
	}

	info, err := svc.Info(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("面板查询失败: %v", err)
	}
	if info.AffCode != "MYCODE01" || info.AffCount != 2 {
		t.Fatalf("面板基本数据不符: %+v", info)
	}
	if !info.Enabled || info.MinTransfer != 100_000 || info.InviterRate != 100_000 ||
		info.InviteeAmount != 5_000_000 || info.MaxRewards != 3 || info.HoldHours != 48 {
		t.Fatalf("面板配置不符: %+v", info)
	}
	if info.AffAvailable != 0 || info.AffCooling != 0 || info.AffHistory != 0 {
		t.Fatalf("无返利时应全为 0: %+v", info)
	}

	items, total, err := svc.Records(ctx, inviter.ID, 0, 20)
	if err != nil {
		t.Fatalf("记录查询失败: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("无返利时记录应为空（数组不能是 null）: total=%d len=%d", total, len(items))
	}
}

// TestInfoDerivesBothRoles 同一用户既是邀请人又是被邀请人时，两侧金额都要算进去。
func TestInfoDerivesBothRoles(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	svc := New(gdb)
	ctx := context.Background()

	me := seedUser(t, gdb)
	up := seedUser(t, gdb)
	down := seedUser(t, gdb)
	now := time.Now()
	rows := []model.AffiliateReward{
		// 我作为被邀请人拿到的赠金
		{OrderID: 9401, InviteeID: me.ID, BeneficiaryID: me.ID, Role: model.AffRoleInvitee,
			Amount: 100, BaseAmount: 1000, AvailableAt: now.Add(-time.Hour), CreatedAt: now},
		// 我作为邀请人拿到的返利
		{OrderID: 9402, InviteeID: down.ID, BeneficiaryID: me.ID, Role: model.AffRoleInviter,
			Amount: 400, BaseAmount: 4000, Rate: 100_000, AvailableAt: now.Add(-time.Hour), CreatedAt: now},
		// 已作废：不进任何一项
		{OrderID: 9403, InviteeID: me.ID, BeneficiaryID: me.ID, Role: model.AffRoleInvitee,
			Amount: 999, BaseAmount: 9000, AvailableAt: now.Add(time.Hour), Voided: true, CreatedAt: now},
		// 别人的钱：绝不能算进我的面板
		{OrderID: 9404, InviteeID: me.ID, BeneficiaryID: up.ID, Role: model.AffRoleInviter,
			Amount: 777, BaseAmount: 7770, Rate: 100_000, AvailableAt: now.Add(-time.Hour), CreatedAt: now},
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatalf("写返利行失败: %v", err)
	}

	info, err := svc.Info(ctx, me.ID)
	if err != nil {
		t.Fatalf("面板查询失败: %v", err)
	}
	if info.AffAvailable != 500 || info.AffHistory != 500 {
		t.Fatalf("可转出/累计应为 100+400=500，实际 %d/%d", info.AffAvailable, info.AffHistory)
	}
	if info.AffCooling != 0 {
		t.Fatalf("到期行不该计入冷却: %d", info.AffCooling)
	}
}

// TestRecordsSnapshotNotConfig 历史金额读行快照：改配置后不回算。
func TestRecordsSnapshotNotConfig(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	svc := New(gdb)
	ctx := context.Background()

	inviter := seedUser(t, gdb)
	invitee := invite(t, gdb, inviter)
	reward(t, gdb, invitee.ID, 9501, 10_000)

	loud := baseCfg()
	loud.InviterRate = 500_000
	loud.InviteeAmount = 10_000_000
	setCfg("aff", loud)

	items, total, err := svc.Records(ctx, inviter.ID, 0, 20)
	if err != nil {
		t.Fatalf("记录查询失败: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("邀请人只该看到自己那一行: total=%d len=%d", total, len(items))
	}
	if items[0].Reward != 10_000_000 { // 100 元 × 10%
		t.Fatalf("历史金额跟着配置变了: %d", items[0].Reward)
	}
	if items[0].Role != model.AffRoleInviter {
		t.Fatalf("角色不符: %q", items[0].Role)
	}
	if items[0].Counterpart == "" || !strings.Contains(items[0].Counterpart, "***") {
		t.Fatalf("被邀请人用户名应脱敏: %q", items[0].Counterpart)
	}
	if items[0].State != StateCooling {
		t.Fatalf("冷却 48h 时应为 cooling，实际 %q", items[0].State)
	}
	if items[0].OrderID != 9501 || items[0].BaseAmount != 100_000_000 {
		t.Fatalf("记录快照字段不符: %+v", items[0])
	}
}

// TestTransferBelowMinAndLedger 未达门槛拒绝；达标转入余额并写 invite 流水。
func TestTransferBelowMinAndLedger(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	svc := New(gdb)
	ctx := context.Background()
	now := time.Now()

	low := seedUser(t, gdb)
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 9601, InviteeID: low.ID, BeneficiaryID: low.ID, Role: model.AffRoleInvitee,
		Amount: 99_999, BaseAmount: 99_999, AvailableAt: now.Add(-time.Hour), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写返利行失败: %v", err)
	}
	if _, err := svc.Transfer(ctx, low.ID); err != errs.AffBelowMin {
		t.Fatalf("未达门槛应拒绝: %v", err)
	}

	rich := seedUser(t, gdb)
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 9602, InviteeID: rich.ID, BeneficiaryID: rich.ID, Role: model.AffRoleInvitee,
		Amount: 150_000, BaseAmount: 150_000, AvailableAt: now.Add(-time.Hour), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写返利行失败: %v", err)
	}
	got, err := svc.Transfer(ctx, rich.ID)
	if err != nil {
		t.Fatalf("转出应成功: %v", err)
	}
	if got != 150_000 {
		t.Fatalf("转出额度不符: %d", got)
	}
	if after := reload(t, gdb, rich.ID); after.Balance != 150_000 {
		t.Fatalf("余额未入账: %d", after.Balance)
	}
	var txn model.Transaction
	if err := gdb.Where("user_id = ? AND type = ?", rich.ID, model.TxnInvite).First(&txn).Error; err != nil {
		t.Fatalf("应写 invite 流水: %v", err)
	}
	if txn.Amount != 150_000 || txn.BalanceAfter != 150_000 || txn.RefType != model.RefTypeAffiliate {
		t.Fatalf("流水不符: %+v", txn)
	}
	if row := legOf(t, gdb, 9602, model.AffRoleInvitee); row.TransferredAt == nil {
		t.Fatal("转出后必须标记 transferred_at，否则能被转第二次")
	}
	// 二次转出：已无可转出行
	if _, err := svc.Transfer(ctx, rich.ID); err != errs.AffNothing {
		t.Fatalf("二次转出应返回「无可转出」: %v", err)
	}

	// 关闭开关只停新增：存量返利仍可转出（否则用户看到钱却提不出来）
	off := baseCfg()
	off.Enabled = false
	setCfg("aff", off)
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 9603, InviteeID: rich.ID, BeneficiaryID: rich.ID, Role: model.AffRoleInvitee,
		Amount: 200_000, BaseAmount: 200_000, AvailableAt: now.Add(-time.Hour), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写返利行失败: %v", err)
	}
	if _, err := svc.Transfer(ctx, rich.ID); err != nil {
		t.Fatalf("关闭开关后存量返利应仍可转出: %v", err)
	}
}

// TestTransferBothSidesIndependent 一行一受益人的核心回归：一方转出不影响另一方。
//
// 旧结构一行装两个人、只有一个 transferred_at：邀请人先转出，被邀请人那 5 元
// 就再也转不出来（且退款也不再作废）。这条用例在旧结构上必红。
func TestTransferBothSidesIndependent(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	cfg := baseCfg()
	cfg.MinTransfer = 1_000_000 // 1 元，让双方都能单独达到门槛
	setAff(t, cfg)
	svc := New(gdb)
	ctx := context.Background()

	inviter := seedUser(t, gdb)
	invitee := invite(t, gdb, inviter)
	// 实付 100 元 → 邀请人 10 元、被邀请人 5 元；两行都已到期
	now := time.Now()
	rows := []model.AffiliateReward{
		{OrderID: 9901, InviteeID: invitee.ID, BeneficiaryID: inviter.ID, Role: model.AffRoleInviter,
			Amount: 10_000_000, BaseAmount: 100_000_000, Rate: 100_000,
			AvailableAt: now.Add(-time.Hour), CreatedAt: now},
		{OrderID: 9901, InviteeID: invitee.ID, BeneficiaryID: invitee.ID, Role: model.AffRoleInvitee,
			Amount: 5_000_000, BaseAmount: 100_000_000,
			AvailableAt: now.Add(-time.Hour), CreatedAt: now},
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatalf("写返利行失败: %v", err)
	}

	got, err := svc.Transfer(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("邀请人转出应成功: %v", err)
	}
	if got != 10_000_000 {
		t.Fatalf("邀请人只该拿到自己那 10 元: %d", got)
	}
	// 被邀请人的钱必须还在自己名下
	info, err := svc.Info(ctx, invitee.ID)
	if err != nil {
		t.Fatalf("被邀请人面板查询失败: %v", err)
	}
	if info.AffAvailable != 5_000_000 {
		t.Fatalf("邀请人转出后，被邀请人的 5 元应仍可转出，实际 %d", info.AffAvailable)
	}
	got, err = svc.Transfer(ctx, invitee.ID)
	if err != nil {
		t.Fatalf("被邀请人转出应成功: %v", err)
	}
	if got != 5_000_000 {
		t.Fatalf("被邀请人应转出 5 元，实际 %d", got)
	}
	if u := reload(t, gdb, invitee.ID); u.Balance != 5_000_000 {
		t.Fatalf("被邀请人余额不符: %d", u.Balance)
	}
}

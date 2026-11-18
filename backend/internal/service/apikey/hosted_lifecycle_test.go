package apikey

// 托管密钥生命周期集成用例：kind 过滤止血、轮换续期不解禁、闲置过期滑动续期。
// 均为白盒（同包），直接操作 db.DB 全局（与被测代码同一管线，不另造入口）。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

const hkSchema = "apikey_hk_test"

// withHKDB 在独立 schema 里准备 users/api_keys 并指向 db.DB（apikey 包用全局句柄）。
func withHKDB(t *testing.T) *gorm.DB {
	t.Helper()
	// rotateHosted 的 Encrypt/Decrypt 需要进程级主密钥；与其它包的测试同一注入方式。
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	conf.Load()
	dsn := DSNOf(t)
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + hkSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+hkSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.APIKey{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	// 节流 map 是包级全局：每个用例从干净状态开始，避免上一个用例的条目吞掉本次落库。
	touchMu.Lock()
	touchAt = map[uint64]time.Time{}
	touchMu.Unlock()
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + hkSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// seedHostedKey 造一把托管密钥并返回（明文、行）。
func seedHostedKey(t *testing.T, gdb *gorm.DB, uid uint64, status int16, expiredAt, lastUsed *time.Time) (string, model.APIKey) {
	t.Helper()
	plain := "ld-hktest-" + time.Now().Format("150405.000000000")
	k := model.APIKey{
		UserID: uid, AppID: 7, Kind: model.KeyKindHosted,
		KeyCipher: "x", KeyHash: Hash(plain), KeyPrefix: "ld-hktest",
		Name: "托管", Status: status, Scopes: mustJSON(t, []string{"chat:write"}),
		ExpiredAt: expiredAt, LastUsedAt: lastUsed,
	}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建托管密钥失败: %v", err)
	}
	return plain, k
}

func mustJSON(t *testing.T, v any) model.JSON {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return model.JSON(raw)
}

// TestRevokeSelfBuiltTx_KindFilter 断言止血只禁自建 KEY：hosted 行不动、
// user 行禁用、返回数恰好等于自建启用数。
func TestRevokeSelfBuiltTx_KindFilter(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-revoke", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	hosted := model.APIKey{UserID: u.ID, AppID: 1, Kind: model.KeyKindHosted, KeyCipher: "x",
		KeyHash: "hk1", Name: "h", Status: KeyStatusEnabled}
	self := model.APIKey{UserID: u.ID, Kind: model.KeyKindUser, KeyCipher: "x",
		KeyHash: "uk1", Name: "s", Status: KeyStatusEnabled}
	selfDisabled := model.APIKey{UserID: u.ID, Kind: model.KeyKindUser, KeyCipher: "x",
		KeyHash: "uk2", Name: "s2", Status: KeyStatusDisabled}
	for _, k := range []model.APIKey{hosted, self, selfDisabled} {
		if err := gdb.Create(&k).Error; err != nil {
			t.Fatal(err)
		}
	}

	n, err := RevokeSelfBuiltTx(gdb, u.ID)
	if err != nil {
		t.Fatalf("RevokeSelfBuiltTx 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应只禁 1 把自建启用 KEY，实际 %d", n)
	}
	var got model.APIKey
	if err := gdb.Take(&got, "key_hash = ?", "hk1").Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != KeyStatusEnabled {
		t.Fatalf("托管密钥必须保持启用，实际 status=%d", got.Status)
	}
	// dest 必须清零重用：Take/First 会把 dest 的非零主键（上次查到的 id=1）
	// 追加为查询条件，复用 got 会让本查询变成 key_hash=? AND id=1 → 必然 not found。
	got = model.APIKey{}
	if err := gdb.Take(&got, "key_hash = ?", "uk1").Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != KeyStatusDisabled {
		t.Fatalf("自建启用 KEY 应被禁用，实际 status=%d", got.Status)
	}

	// RevokeAllTx 仍是全禁语义（管理员强制重置路径）。
	n, err = RevokeAllTx(gdb, u.ID)
	if err != nil || n != 1 {
		t.Fatalf("RevokeAllTx 应再禁 1 把托管 KEY，n=%d err=%v", n, err)
	}
}

// TestRotate_RejectsAdminBlock 断言 admin_block 的钥**不会被轮换**：既不生成
// 新明文，也不改动库中任何字段。
//
// 这是安全处置的最后一道闸。旧实现会返回 200 + 一个新明文，但库中那把钥
// 仍是 status=2 —— 应用把必然 401 的值写进配置，还以为"轮换成功了"。
func TestRotate_RejectsAdminBlock(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-rotate", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	stale := time.Now().AddDate(0, 0, -200)
	plain, k := seedHostedKey(t, gdb, u.ID, KeyStatusDisabled, &stale, &stale)
	// 管理员封禁：唯一不可被轮换覆盖的禁用类型。
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Update("disabled_reason", model.KeyDisabledAdminBlock).Error; err != nil {
		t.Fatal(err)
	}
	k.DisabledReason = model.KeyDisabledAdminBlock

	res, err := rotateHosted(context.Background(), &k, false)
	if !errors.Is(err, ErrKeyDisabled) {
		t.Fatalf("admin_block 必须拒绝轮换，实际 err=%v res=%+v", err, res)
	}
	if res != nil {
		t.Fatal("拒绝时不得返回任何结果（含明文）")
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.KeyHash != Hash(plain) {
		t.Fatal("拒绝时不得改动 key_hash（旧明文必须仍然有效）")
	}
	if got.Status != KeyStatusDisabled || got.DisabledReason != model.KeyDisabledAdminBlock {
		t.Fatalf("拒绝时不得改动禁用状态：status=%d reason=%q", got.Status, got.DisabledReason)
	}
}

// TestRotate_RenewsExpiryAndTouches 断言正常轮换三件事：换明文、续满有效期、
// 刷 last_used_at —— 与"是否解禁"无关（启用中的钥本来就不涉及解禁）。
func TestRotate_RenewsExpiryAndTouches(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-rotate-renew", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	future := time.Now().AddDate(0, 0, 30)
	plain, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &future, nil)

	res, err := rotateHosted(context.Background(), &k, false)
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if res.Plaintext == "" || res.Plaintext == plain {
		t.Fatal("轮换必须产生新明文")
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ExpiredAt == nil || !got.ExpiredAt.After(time.Now().AddDate(0, 0, 170)) {
		t.Fatalf("轮换必须把有效期续满（约 180 天），实际 %v", got.ExpiredAt)
	}
	if got.KeyHash == Hash(plain) {
		t.Fatal("旧 hash 必须被替换")
	}
	if got.LastUsedAt == nil || got.LastUsedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("轮换应刷新 last_used_at，实际 %v", got.LastUsedAt)
	}
	if got.Status != KeyStatusEnabled {
		t.Fatalf("启用中的钥轮换后仍应启用，实际 %d", got.Status)
	}
}

// TestRotateHosted_OverlapWindow 断言轮换不制造断服窗口：
// 旧明文在重叠期内仍可通过鉴权，窗口结束后返回 ErrKeyRetired（而不是"密钥不存在"）。
//
// 这是本模块最核心的一条可用性约定：轮换是新明文与旧配置之间的**过渡**，
// 不是"立刻把旧钥掐死"。没有它，多实例部署下每次轮换都是一次可控故障。
func TestRotateHosted_OverlapWindow(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-overlap", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	future := time.Now().AddDate(0, 0, 30)
	oldPlain, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &future, nil)

	res, err := rotateHosted(context.Background(), &k, false)
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if res.PreviousExpiresAt == nil {
		t.Fatal("默认配置下轮换必须给出旧钥的窗口终点")
	}
	if d := time.Until(*res.PreviousExpiresAt); d < 71*time.Hour || d > 73*time.Hour {
		t.Fatalf("默认窗口应约 72 小时，实际剩余 %v", d)
	}

	// ① 窗口内：旧明文仍可用。
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.PrevKeyHash != Hash(oldPlain) {
		t.Fatal("上一代 hash 必须落库（否则旧明文在窗口内无法鉴权）")
	}
	if _, err := Authenticate(oldPlain); err != nil {
		t.Fatalf("窗口内旧明文应仍可用，实际 %v", err)
	}
	// ② 新明文同时可用。
	if _, err := Authenticate(res.Plaintext); err != nil {
		t.Fatalf("新明文应立即可用，实际 %v", err)
	}

	// ③ 窗口结束后：旧明文 → ErrKeyRetired（可区分于"无效"）。
	past := time.Now().Add(-time.Minute)
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Update("prev_key_expires_at", past).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := Authenticate(oldPlain); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("窗口结束后应为 ErrKeyRetired，实际 %v", err)
	}
	// 新明文不受影响。
	if _, err := Authenticate(res.Plaintext); err != nil {
		t.Fatalf("窗口结束后新明文仍应可用，实际 %v", err)
	}
}

// TestRotateHosted_RecoversNonSecurityDisable 断言"非安全类禁用"可被轮换自愈。
//
// 分类型的意义：退役（应用没在窗口内换完）与闲置超期不代表凭证不可信，
// 它们可以靠一次轮换恢复；只有 admin_block 必须人工解锁。
func TestRotateHosted_RecoversNonSecurityDisable(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-recover", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	stale := time.Now().AddDate(0, 0, -1)
	_, k := seedHostedKey(t, gdb, u.ID, KeyStatusDisabled, &stale, &stale)
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Update("disabled_reason", model.KeyDisabledExpired).Error; err != nil {
		t.Fatal(err)
	}
	k.DisabledReason = model.KeyDisabledExpired

	if _, err := rotateHosted(context.Background(), &k, false); err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != KeyStatusEnabled {
		t.Fatalf("闲置超期属于可自愈禁用，轮换后应启用，实际 status=%d", got.Status)
	}
	if got.DisabledReason != "" {
		t.Fatalf("恢复后应清空 disabled_reason，实际 %q", got.DisabledReason)
	}
}

// TestAuthenticate_IdleHostedRenews 断言过期救回：expired_at 已过但闲置未超限
// → 放行且库内 expired_at 已续期。
func TestAuthenticate_IdleHostedRenews(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-idle", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	tenDaysAgo := time.Now().AddDate(0, 0, -10)
	plain, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &tenDaysAgo, &tenDaysAgo)

	got, err := Authenticate(plain)
	if err != nil {
		t.Fatalf("闲置未超限的过期托管密钥应被救回放行，实际 %v", err)
	}
	var row model.APIKey
	if err := gdb.First(&row, got.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.ExpiredAt == nil || !row.ExpiredAt.After(time.Now()) {
		t.Fatalf("救回必须已把 expired_at 续到未来，实际 %v", row.ExpiredAt)
	}
	if row.LastUsedAt == nil || row.LastUsedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("使用应刷新 last_used_at，实际 %v", row.LastUsedAt)
	}
	_ = k
}

// TestAuthenticate_StaleHostedRejected 断言真闲置超限：救回无意义，拒绝且不续期。
func TestAuthenticate_StaleHostedRejected(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-stale", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	longAgo := time.Now().AddDate(0, 0, -200)
	plain, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &longAgo, &longAgo)

	if _, err := Authenticate(plain); err != ErrKeyExpired {
		t.Fatalf("闲置超限应返回 ErrKeyExpired，实际 %v", err)
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ExpiredAt == nil || got.ExpiredAt.After(time.Now()) {
		t.Fatalf("拒绝路径不得续期，实际 expired_at=%v", got.ExpiredAt)
	}
	// 必须落成显式状态：否则门户显示"启用中"、应用却每次 401，谁也解释不清。
	if got.Status != KeyStatusDisabled || got.DisabledReason != model.KeyDisabledExpired {
		t.Fatalf("闲置超期应标记 status=2/reason=expired，实际 status=%d reason=%q",
			got.Status, got.DisabledReason)
	}
	// 且它属于可自愈的一类：轮换一次就恢复，不需要管理台介入。
	if _, err := rotateHosted(context.Background(), &got, false); err != nil {
		t.Fatalf("轮换恢复失败: %v", err)
	}
	var revived model.APIKey
	if err := gdb.First(&revived, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if revived.Status != KeyStatusEnabled {
		t.Fatalf("轮换后应恢复启用，实际 %d", revived.Status)
	}
}

// TestAuthenticate_UserKeyHardExpire 断言自建 KEY 硬过期：绝不续期。
func TestAuthenticate_UserKeyHardExpire(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "uk-exp", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	past := time.Now().AddDate(0, 0, -1)
	plain := "ld-uk-exp-" + time.Now().Format("150405.000000000")
	k := model.APIKey{UserID: u.ID, Kind: model.KeyKindUser, KeyCipher: "x",
		KeyHash: Hash(plain), Name: "s", Status: KeyStatusEnabled, ExpiredAt: &past,
		LastUsedAt: &past}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := Authenticate(plain); err != ErrKeyExpired {
		t.Fatalf("自建过期应 ErrKeyExpired，实际 %v", err)
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ExpiredAt == nil || got.ExpiredAt.Sub(past).Abs() > time.Second {
		t.Fatalf("自建 KEY 的 expired_at 不得被改，实际 %v", got.ExpiredAt)
	}
}

// TestAuthenticate_ExpireDaysZero_DisablesExpiry 断言配置总闸：expire_days=0
// 时过期的托管密钥直接放行，且不改库。
func TestAuthenticate_ExpireDaysZero_DisablesExpiry(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-noexp", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	past := time.Now().AddDate(0, 0, -400)
	plain, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &past, &past)

	conf.System().Set("hosted_key", json.RawMessage(`{"expire_days":0}`))
	t.Cleanup(func() { conf.System().Set("hosted_key", nil) })

	if _, err := Authenticate(plain); err != nil {
		t.Fatalf("过期总闸关闭时应放行，实际 %v", err)
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ExpiredAt == nil || got.ExpiredAt.Sub(past).Abs() > time.Second {
		t.Fatalf("总闸关闭时不得写库，实际 expired_at=%v", got.ExpiredAt)
	}
}

// TestTouchKey_ThrottleAndCleanup 断言节流：间隔内二次调用不落库。
func TestTouchKey_ThrottleAndCleanup(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-touch", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	_, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, nil, nil)

	now := time.Now()
	touchKey(&k, now)
	var first model.APIKey
	if err := gdb.First(&first, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if first.LastUsedAt == nil {
		t.Fatal("首次 touch 必须落库")
	}

	// 间隔内第二次：不落库（last_used_at 不变）
	touchMu.Lock()
	touchAt[k.ID] = now // 强制视为刚写过
	touchMu.Unlock()
	later := now.Add(time.Minute)
	touchKey(&k, later)
	var second model.APIKey
	if err := gdb.First(&second, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !second.LastUsedAt.Equal(*first.LastUsedAt) {
		t.Fatalf("节流窗口内不得落库：first=%v second=%v", first.LastUsedAt, second.LastUsedAt)
	}
}

// DSNOf 独立命名，避免与 potential future testutil 导入冲突。
// DSNOf 独立命名，避免与 potential future testutil 导入冲突。
func DSNOf(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("LD_TEST_DSN"))
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	return dsn
}

// TestAuthenticate_StorageErrorIsUnavailable 断言存储层错误被包装成
// ErrAuthUnavailable（中间件据此回 503），而不是原始错误（会被默认分支
// 折成「密钥非法 401」—— 那会让接入方误以为凭证失效去换钥）。
func TestAuthenticate_StorageErrorIsUnavailable(t *testing.T) {
	// 用真实连接后**关闭连接池**制造查询期错误：gorm.Open 会立刻 ping，
	// 指向坏端口在 Open 就失败，测不到查询期路径。
	gdb := withHKDB(t)
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("取连接池失败: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("关闭连接池失败: %v", err)
	}

	if _, err := Authenticate("ld-whatever"); !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("存储错误必须包装成 ErrAuthUnavailable，实际 %v", err)
	}
}

// ---- 轮换的 CAS 裁决 ----

// TestRotate_RejectsAdminBlockWrittenConcurrently 断言「读取后被管理员改判封禁」
// 时轮换必须失败，且库中一个字段都不动。
//
// 回归 A1：旧实现的 CAS 只看 key_hash，status/reason 用加载时快照，于是
// "加载时 expired（可自愈）、期间被改判 admin_block"会写回 status=1 + 空原因
// —— 管理员处置被一次 rotate 静默绕过，且没有日志。
func TestRotate_RejectsAdminBlockWrittenConcurrently(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-cas-block", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	stale := time.Now().AddDate(0, 0, -1)
	plain, k := seedHostedKey(t, gdb, u.ID, KeyStatusDisabled, &stale, &stale)
	// 快照：可自愈的禁用。
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Update("disabled_reason", model.KeyDisabledExpired).Error; err != nil {
		t.Fatal(err)
	}
	k.DisabledReason = model.KeyDisabledExpired
	// 读取之后：管理员把它改判为封禁（status 不变，只有 reason 变）。
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Update("disabled_reason", model.KeyDisabledAdminBlock).Error; err != nil {
		t.Fatal(err)
	}

	res, err := rotateHosted(context.Background(), &k, false)
	if !errors.Is(err, ErrKeyDisabled) {
		t.Fatalf("期间被封禁必须拒绝轮换，实际 err=%v res=%+v", err, res)
	}
	if res != nil {
		t.Fatal("拒绝时不得返回任何结果（含明文）")
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.KeyHash != Hash(plain) {
		t.Fatal("拒绝时不得改动 key_hash（旧明文必须仍然有效）")
	}
	if got.Status != KeyStatusDisabled || got.DisabledReason != model.KeyDisabledAdminBlock {
		t.Fatalf("不得覆盖管理员裁决：status=%d reason=%q", got.Status, got.DisabledReason)
	}
}

// TestRotate_RecoversExpiredWrittenConcurrently 断言「读取后被并发请求标记为
// 闲置超期」时，轮换以**新快照**判定并真正恢复启用，且返回的明文可用。
//
// 回归 A2：旧实现 updates 不含 status，于是加载时 status=1、期间被置为
// status=2/expired 的钥会拿到 200 + 新明文，但那把钥是禁用的 —— 应用把
// 必然 401 的值写进配置，还以为成功了。
func TestRotate_RecoversExpiredWrittenConcurrently(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-cas-expire", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	future := time.Now().AddDate(0, 0, 30)
	_, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &future, nil)
	// 读取之后：另一个请求判定它闲置超期并落库。
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Updates(map[string]any{
			"status":          KeyStatusDisabled,
			"disabled_reason": model.KeyDisabledExpired,
		}).Error; err != nil {
		t.Fatal(err)
	}

	res, err := rotateHosted(context.Background(), &k, false)
	if err != nil {
		t.Fatalf("expired 属于可自愈禁用，轮换应成功，实际 %v", err)
	}
	if res.Plaintext == "" {
		t.Fatal("必须返回新明文")
	}
	// 最关键的一条：返回的明文必须**真的能用**（不能是死钥）。
	if _, err := Authenticate(res.Plaintext); err != nil {
		t.Fatalf("轮换返回的明文必须可用，实际 %v", err)
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != KeyStatusEnabled || got.DisabledReason != "" {
		t.Fatalf("轮换后应恢复启用并清空原因：status=%d reason=%q", got.Status, got.DisabledReason)
	}
}

// TestRotate_NoOverwriteAfterCompetitorWins 断言「别人已经换过钥」时直接退出竞争，
// 不再生成第二把。
//
// 回归 A4：旧实现重试循环会在冲突后继续轮换，把胜者刚交付的明文写成 prev；
// 窗口 0 时那把明文毫秒内失效，而调用方已拿到 200 + 明文，无从察觉。
func TestRotate_NoOverwriteAfterCompetitorWins(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-cas-lose", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	future := time.Now().AddDate(0, 0, 30)
	_, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &future, nil)
	// 读取之后：另一个请求轮换成功（hash 变了）。
	winnerHash := strings.Repeat("a", 64)
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Update("key_hash", winnerHash).Error; err != nil {
		t.Fatal(err)
	}

	res, err := rotateHosted(context.Background(), &k, false)
	if !errors.Is(err, ErrRotateConflict) {
		t.Fatalf("别人赢了必须返回 ErrRotateConflict，实际 err=%v res=%+v", err, res)
	}
	if res != nil {
		t.Fatal("冲突时不得返回任何明文")
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.KeyHash != winnerHash {
		t.Fatal("绝不覆盖胜者的密钥哈希")
	}
}

// TestRotate_ViaPrevKeepsOriginalWindow 断言窗口期旧钥自救轮换的语义：
// 新明文立即生效、prev 指向**原主钥**、窗口终点**不变**（不延长）。
//
// prev 指向谁很关键：若指向调用方手里的旧钥，实例 A 先前拿到的新钥就会变成
// "既非主钥也非 prev"，当场失效 —— 一次自救变成一次断服。
func TestRotate_ViaPrevKeepsOriginalWindow(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-via-prev", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	future := time.Now().AddDate(0, 0, 30)
	_, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &future, nil)
	// 模拟"已被别人轮换过一次"：主钥 = k.KeyHash，prev = 一把更早的钥，窗口到 T。
	mainHash := k.KeyHash
	origPrevExp := time.Now().Add(24 * time.Hour).Truncate(time.Microsecond)
	oldHash := strings.Repeat("b", 64)
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Updates(map[string]any{
			"prev_key_hash":       oldHash,
			"prev_key_expires_at": origPrevExp,
		}).Error; err != nil {
		t.Fatal(err)
	}
	k.PrevKeyHash, k.PrevKeyExpiresAt = oldHash, &origPrevExp

	res, err := rotateHosted(context.Background(), &k, true)
	if err != nil {
		t.Fatalf("窗口期旧钥自救轮换应成功，实际 %v", err)
	}
	if res.PreviousExpiresAt == nil {
		t.Fatal("必须给出上一代的窗口终点")
	}
	if d := res.PreviousExpiresAt.Sub(origPrevExp); d > time.Millisecond || d < -time.Millisecond {
		t.Fatalf("窗口不得延长：期望 %v，实际 %v", origPrevExp, *res.PreviousExpiresAt)
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.PrevKeyHash != mainHash {
		t.Fatalf("prev 必须指向原主钥，实际 %q", got.PrevKeyHash)
	}
	if got.PrevKeyExpiresAt == nil || got.PrevKeyExpiresAt.Sub(origPrevExp).Abs() > time.Millisecond {
		t.Fatalf("窗口终点不得变化，实际 %v", got.PrevKeyExpiresAt)
	}
	if got.KeyHash == mainHash {
		t.Fatal("主钥必须换成新值")
	}
	if _, err := Authenticate(res.Plaintext); err != nil {
		t.Fatalf("自救返回的明文必须可用，实际 %v", err)
	}
}

// TestHostedScopesForIssue_ClipsBillingWrite 断言权限集只有**一个出口**。
//
// 回归 A8：旧实现 insert 走裁剪、rotate 直接传契约全集，于是
// allow_billing_write=false 被一次轮换静默推翻（billing:write 又回来了）。
func TestHostedScopesForIssue_ClipsBillingWrite(t *testing.T) {
	conf.System().Set("hosted_key", json.RawMessage(`{"allow_billing_write":false}`))
	t.Cleanup(func() { conf.System().Set("hosted_key", nil) })

	hasBilling := func(list []string) bool {
		for _, s := range list {
			if s == apicontract.ScopeBillingWrite {
				return true
			}
		}
		return false
	}
	if hasBilling(hostedScopesForIssue()) {
		t.Fatal("allow_billing_write=false 时不得下发 billing:write")
	}

	conf.System().Set("hosted_key", json.RawMessage(`{"allow_billing_write":true}`))
	if !hasBilling(hostedScopesForIssue()) {
		t.Fatal("allow_billing_write=true 时必须下发 billing:write（否则开关形同虚设）")
	}
}

// TestRotateSelf_ClipsBillingWrite 端到端验证轮换不会把 billing:write 加回来。
func TestRotateSelf_ClipsBillingWrite(t *testing.T) {
	gdb := withHKDB(t)
	conf.System().Set("hosted_key", json.RawMessage(`{"allow_billing_write":false}`))
	t.Cleanup(func() { conf.System().Set("hosted_key", nil) })

	u := model.User{Username: "hk-self-clip", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	future := time.Now().AddDate(0, 0, 30)
	_, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &future, nil)
	// 先人为塞进 billing:write（模拟改造前签发的钥）。
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Update("scopes", mustJSON(t, []string{apicontract.ScopeBillingWrite, "task:write"})).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := RotateSelf(context.Background(), k.ID, false); err != nil {
		t.Fatalf("自举轮换失败: %v", err)
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	for _, s := range DecodeStrings(got.Scopes) {
		if s == apicontract.ScopeBillingWrite {
			t.Fatal("轮换后不得保留 billing:write（配置说不要就是不要）")
		}
	}
}

// TestRotateSelf_Gone 断言目标行消失时返回 ErrKeyGone（对客映射为 401 无效密钥，
// 而不是 401 已禁用 —— 后者会让应用去等平台解锁）。
func TestRotateSelf_Gone(t *testing.T) {
	withHKDB(t)
	if _, err := RotateSelf(context.Background(), 999999, false); !errors.Is(err, ErrKeyGone) {
		t.Fatalf("不存在的钥应返回 ErrKeyGone，实际 %v", err)
	}
}

// ---- 过期判定的两个方向 ----

// TestMarkExpired_DoesNotKillFreshlyEnabled 断言管理员刚解禁（刷新 last_used_at）
// 后，一个基于**旧** last_used_at 判定的在途请求不会把它打回禁用。
//
// 回归 A5：旧谓词只有 status=1，"判定条件"与"写入条件"是两套口径，于是在途
// 请求可以覆盖刚刚发生的解禁 —— 处置被一次并发吞掉。
func TestMarkExpired_DoesNotKillFreshlyEnabled(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-mark-expire", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	stale := time.Now().AddDate(0, 0, -200)
	_, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &stale, &stale)
	// 管理员解禁：刷新 last_used_at = now（status 仍是 1）。
	now := time.Now()
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Update("last_used_at", now).Error; err != nil {
		t.Fatal(err)
	}
	// 在途请求手里仍是 200 天前的快照，它会判定"真闲置超期"。
	// base 是它算出的旧基准（last_used_at 的旧值）—— 落库谓词必须拿它比对，
	// 而不是拿"现在"重新算，否则刚刷新的 last_used_at 会被这次写覆盖。
	markExpired(&k, now, stale)

	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != KeyStatusEnabled {
		t.Fatalf("刚刷新过 last_used_at 的钥不得被判闲置超期，实际 status=%d", got.Status)
	}
	if got.DisabledReason != "" {
		t.Fatalf("不得写入 disabled_reason，实际 %q", got.DisabledReason)
	}
}

// TestRenewExpiredAt_PreRenewWorks 断言临期预续真的能落库。
//
// 回归 B1：预续的调用前提是"expired_at 还没到但快到了"，而旧谓词是
// `expired_at < now`，两者互斥 ⇒ 预续恒 0 行，续期只发生在"过期后的第一次请求"。
func TestRenewExpiredAt_PreRenewWorks(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-prerenew", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	soon := time.Now().AddDate(0, 0, 10) // 未过期，但剩余不足 30 天
	_, k := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &soon, nil)
	pol := hostedPolicyOf()

	renewed, err := renewExpiredAt(&k, time.Now(), pol, renewAhead)
	if err != nil {
		t.Fatalf("预续失败: %v", err)
	}
	if !renewed {
		t.Fatal("剩余 10 天 < 30 天，预续必须真正落库（ahead=0 时才会 0 行）")
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ExpiredAt == nil || !got.ExpiredAt.After(time.Now().AddDate(0, 0, 170)) {
		t.Fatalf("应续满约 180 天，实际 %v", got.ExpiredAt)
	}

	// 对照：同一把钥改用 ahead=0 不该再续（它还没过期）。
	if renewed, err := renewExpiredAt(&got, time.Now(), pol, 0); err != nil || renewed {
		t.Fatalf("ahead=0 只对已过期的钥生效，实际 renewed=%v err=%v", renewed, err)
	}
}

// TestAuthenticate_ExpiredStaysExpired 断言"闲置超期"的码不会变脸。
//
// 回归：markExpired 把行写成 status=2，而 Authenticate 先判 status —— 于是
// 第一次请求返回 key_expired（当时还没落状态），之后每次都返回 key_disabled。
// 同一个故障两种语义：前者说"让用户在门户轮换一次"，后者说"联系平台"，
// 文档与排障全部对不上。判定顺序修好后，两种时刻都必须是 key_expired。
func TestAuthenticate_ExpiredStaysExpired(t *testing.T) {
	gdb := withHKDB(t)
	u := model.User{Username: "hk-expired-stable", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	longAgo := time.Now().AddDate(0, 0, -200)
	plain, _ := seedHostedKey(t, gdb, u.ID, KeyStatusEnabled, &longAgo, &longAgo)

	for i := 0; i < 2; i++ {
		if _, err := Authenticate(plain); !errors.Is(err, ErrKeyExpired) {
			t.Fatalf("第 %d 次调用应返回 ErrKeyExpired，实际 %v", i+1, err)
		}
	}
}

// TestRenewAheadFor 断言预续提前量随 expire_days 收缩。
//
// 回归：ahead 恒为 30 天，而 expire_days 可配。配成 7 天时"剩余 < 30 天"恒真，
// 每次 touch（每钥每 5 分钟）都会命中预续谓词 —— 一条 UPDATE + 一条事件，
// 写放大从"每天几次"变成"每天几百次"。
func TestRenewAheadFor(t *testing.T) {
	cases := []struct {
		days int
		want time.Duration
	}{
		{180, renewAhead},           // 默认：不收缩
		{60, renewAhead},            // 仍是 30 天
		{7, 7 * 24 * time.Hour / 2}, // 收缩到一半
		{1, 12 * time.Hour},         // 更短
		{0, renewAhead},             // 关闭过期：不会走这条路径
	}
	for _, c := range cases {
		if got := renewAheadFor(HostedPolicy{ExpireDays: c.days}); got != c.want {
			t.Fatalf("ExpireDays=%d 的提前量应为 %v，实际 %v", c.days, c.want, got)
		}
	}
}

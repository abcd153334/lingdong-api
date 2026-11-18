package apikey

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/testutil"
)

// TestAuthenticateWithUserMatchesTwoStep 守住「合并查询」与「两步查询」的等价性。
//
// 为什么必须有这条：`/v1` 的鉴权从「查 KEY + 查 users」合并成了一条 JOIN
// （压测：1.84 → ≈0.92 事务/请求）。合并的实现用了 GORM 的 embeddedPrefix
// 自动映射列名 —— 一旦字段漏映射，症状不是报错，而是**用户被判成禁用、
// 余额读成 0、分组退回 default**，属于最难发现的一类缺陷。
func zeroTime() time.Time { return time.Time{} }

func TestAuthenticateWithUserMatchesTwoStep(t *testing.T) {
	gdb := testutil.Open(t)
	old := db.DB
	db.DB = gdb
	t.Cleanup(func() { db.DB = old })

	if err := gdb.AutoMigrate(&model.User{}, &model.APIKey{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	uname := fmt.Sprintf("au%x", time.Now().UnixNano()%1e16)
	u := model.User{
		Username: uname, PasswordHash: "x", Status: 1,
		Balance: 1234567, Used: 42, Frozen: 7, Group: "vip", Role: 1,
		Avatar: "data:image/webp;base64,AAAA",
	}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	plain, hash, prefix, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	k := model.APIKey{
		UserID: u.ID, KeyCipher: "c", KeyHash: hash, KeyPrefix: prefix,
		Name: "t", Status: 1, RPMLimit: 7, ConcurrencyLimit: 3, Group: "vip",
	}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	t.Cleanup(func() {
		gdb.Delete(&model.APIKey{}, k.ID)
		gdb.Delete(&model.User{}, u.ID)
	})

	stepKey, err := Authenticate(plain)
	if err != nil {
		t.Fatalf("Authenticate 失败: %v", err)
	}
	joinKey, joinUser, err := AuthenticateWithUser(plain)
	if err != nil {
		t.Fatalf("AuthenticateWithUser 失败: %v", err)
	}

	// 1) KEY 行：整结构比较（列名映射错了这里先红）。
	//
	// 三个时间字段要排除：touchKey 在两次调用之间各写了一次 last_used_at，
	// updated_at 也随之变；它们不是"映射错"，而是两次读发生在两次写之间。
	a, b := *stepKey, *joinKey
	a.LastUsedAt, a.UpdatedAt, a.CreatedAt = nil, zeroTime(), zeroTime()
	b.LastUsedAt, b.UpdatedAt, b.CreatedAt = nil, zeroTime(), zeroTime()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("KEY 行不一致：\n两步: %+v\n合并: %+v", a, b)
	}
	// last_used_at 必须真的被写（两边都应该在最近几秒内）
	for name, ts := range map[string]*time.Time{"两步": stepKey.LastUsedAt, "合并": joinKey.LastUsedAt} {
		if ts == nil || time.Since(*ts) > time.Minute {
			t.Fatalf("%s 的 last_used_at 未被 touchKey 写入：%v", name, ts)
		}
	}

	// 2) 用户行：与单独查询的结果整结构比较
	var want model.User
	if err := gdb.First(&want, u.ID).Error; err != nil {
		t.Fatalf("读用户失败: %v", err)
	}
	// 头像**刻意不查**（见 authPairSelect 注释）：它是门户资料字段，
	// /v1 不读，但 24KB × 每请求会把 PG 带宽打满。这里显式断言"查不到"，
	// 一旦有人把它加回来，会立刻红。
	if joinUser.Avatar != "" {
		t.Fatalf("鉴权查询不应带出头像，实际 %q", joinUser.Avatar)
	}
	want.Avatar = ""
	if !reflect.DeepEqual(*joinUser, want) {
		t.Fatalf("用户行不一致：\n合并查询: %+v\n独立查询: %+v", *joinUser, want)
	}
	// 抽样断言：这三个字段错了会直接改变业务结论（拒调用 / 欠费闸 / 分组倍率）
	if joinUser.Status != 1 || joinUser.Balance != 1234567 || joinUser.Group != "vip" {
		t.Fatalf("关键字段读错：status=%d balance=%d group=%q",
			joinUser.Status, joinUser.Balance, joinUser.Group)
	}
}

// TestAuthenticateWithUserPrevKeyWindow 守住轮换重叠窗口也走合并查询。
//
// 旧明文在 prev_key_expires_at 之前必须仍能通过，并且同样带回用户；
// 否则轮换期间所有实例会同时 401，或拿到 nil 用户触发空指针。
func TestAuthenticateWithUserPrevKeyWindow(t *testing.T) {
	gdb := testutil.Open(t)
	old := db.DB
	db.DB = gdb
	t.Cleanup(func() { db.DB = old })
	if err := gdb.AutoMigrate(&model.User{}, &model.APIKey{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	uname := fmt.Sprintf("pv%x", time.Now().UnixNano()%1e16)
	u := model.User{Username: uname, PasswordHash: "x", Status: 1, Balance: 500}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	oldPlain, oldHash, prefix, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	_, newHash, _, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	k := model.APIKey{
		UserID: u.ID, KeyCipher: "c", KeyHash: newHash, KeyPrefix: prefix,
		Name: "rot", Status: 1, PrevKeyHash: oldHash, PrevKeyExpiresAt: &future,
	}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	t.Cleanup(func() {
		gdb.Delete(&model.APIKey{}, k.ID)
		gdb.Delete(&model.User{}, u.ID)
	})

	gotKey, gotUser, err := AuthenticateWithUser(oldPlain)
	if err != nil {
		t.Fatalf("窗口内旧明文应通过，实际: %v", err)
	}
	if gotKey.ID != k.ID || gotUser.ID != u.ID {
		t.Fatalf("取错行：key=%d user=%d", gotKey.ID, gotUser.ID)
	}

	// 窗口过期后必须变成 key_retired（语义与两步路径一致）
	past := time.Now().Add(-time.Hour)
	if err := gdb.Model(&model.APIKey{}).Where("id = ?", k.ID).
		Update("prev_key_expires_at", past).Error; err != nil {
		t.Fatalf("改窗口失败: %v", err)
	}
	if _, _, err := AuthenticateWithUser(oldPlain); err != ErrKeyRetired {
		t.Fatalf("窗口外应返回 ErrKeyRetired，实际: %v", err)
	}
}

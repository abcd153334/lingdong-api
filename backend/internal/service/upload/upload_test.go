package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/uploadcfg"
)

// ---------- 纯函数用例 ----------

func TestSafeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"photo.jpg", "photo.jpg"},
		{"a b.png", "a_b.png"},
		{"../../etc/passwd", "passwd"},
		{`C:\dir\file.mp4`, "file.mp4"},
		{".hidden", "f_.hidden"},
		{"", "f_"},
		{strings.Repeat("x", 100) + ".png", strings.Repeat("x", 80)},
	}
	for _, c := range cases {
		if got := SafeName(c.in); got != c.want {
			t.Errorf("SafeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := SafeName("中文.txt"); got != "______.txt" {
		t.Errorf("SafeName 中文应逐字节替换为 _，实际 %q", got)
	}
}

func TestSniff(t *testing.T) {
	wav := append([]byte("RIFF\x00\x00\x00\x00"), []byte("WAVEfmt ")...)
	webp := append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBPVP8 ")...)
	bmp := append([]byte{'B', 'M'}, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 40, 0, 0, 0}...)
	cases := []struct {
		name   string
		head   []byte
		family string
		danger bool
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n"), "image", false},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image", false},
		{"gif", []byte("GIF89a"), "image", false},
		{"webp", webp, "image", false},
		{"bmp", bmp, "image", false},
		{"mp3", []byte("ID3\x04\x00"), "audio", false},
		{"mp3-frame", []byte{0xFF, 0xFB, 0x90}, "audio", false},
		{"wav", wav, "audio", false},
		{"ogg", []byte("OggS\x00\x02"), "audio", false},
		{"mp4", append([]byte{0, 0, 0, 0x20}, []byte("ftypisom")...), "ftyp", false},
		{"m4a", append([]byte{0, 0, 0, 0x20}, []byte("ftypM4A ")...), "ftyp", false},
		{"webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x01}, "webm", false},
		{"pdf", []byte("%PDF-1.7"), "pdf", false},
		{"zip", []byte("PK\x03\x04\x14\x00"), "zip", false},
		{"txt", []byte("hello,world\n1,2\n"), "text", false},
		{"exe", append([]byte("MZ"), bytes.Repeat([]byte{0}, 60)...), "", true},
		{"elf", []byte{0x7F, 'E', 'L', 'F', 2, 1}, "", true},
		{"script", []byte("#!/bin/sh"), "", true},
		{"binary", []byte{0x00, 0x01, 0x02, 0x03}, "", false},
	}
	for _, c := range cases {
		family, danger := sniff(c.head)
		if family != c.family || danger != c.danger {
			t.Errorf("sniff(%s) = (%q,%v), want (%q,%v)", c.name, family, danger, c.family, c.danger)
		}
	}
}

// TestExtGate 校验「扩展名类别 × 魔数族」双校验矩阵（打的是 Create 用的同一个 typeGate）。
func TestExtGate(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n")
	mz := append([]byte("MZ"), bytes.Repeat([]byte{0}, 60)...)
	cases := []struct {
		ext  string
		head []byte
		ok   bool
	}{
		{"png", png, true},
		{"PNG", png, true}, // 扩展名大小写不敏感
		{"png", mz, false}, // 伪装扩展名
		{"png", []byte("hello,world"), false},
		{"txt", png, false},
		{"pdf", []byte("%PDF-1.7"), true},
		{"json", []byte(`{"a":1}`), true},
		{"docx", []byte("PK\x03\x04\x14\x00"), true},
		{"exe", png, false}, // 后缀不在配置里
		{"", png, false},
	}
	s := uploadcfg.Defaults()
	for _, c := range cases {
		if got := typeGate(s, uploadcfg.NormalizeExt(c.ext), c.head); got != c.ok {
			t.Errorf("gate(ext=%q) = %v, want %v", c.ext, got, c.ok)
		}
	}
}

// TestExtGateFollowsConfig 守住「支持的类型只由配置决定」。
//
// 这是本次需求的核心：删掉一个后缀就该 415，加一个自定义后缀（连同它的类别）
// 就该能用 —— 代码里不再有任何扩展名白名单可以直接决定这件事。
func TestExtGateFollowsConfig(t *testing.T) {
	s := uploadcfg.Defaults()
	delete(s.Types, "pdf")
	if typeGate(s, "pdf", []byte("%PDF-1.7")) {
		t.Fatal("配置里删掉 pdf 后应判不支持")
	}

	// 自定义后缀：归到 image 类，ftyp 头（heic/avif 的容器）应当被接受。
	s.Types["heic"] = uploadcfg.KindImage
	if !typeGate(s, "heic", append([]byte{0, 0, 0, 24}, []byte("ftypheic")...)) {
		t.Fatal("自定义的 heic 应通过（image 类接受 ftyp 族）")
	}
	// 同一个后缀配成 text 类就不能再收 ftyp 头。
	s.Types["heic"] = uploadcfg.KindText
	if typeGate(s, "heic", append([]byte{0, 0, 0, 24}, []byte("ftypheic")...)) {
		t.Fatal("归到 text 类后不应接受 ftyp 头")
	}
}

// ---------- 数据库集成用例（需 LD_TEST_DSN；未设置则跳过） ----------
// LD_TEST_DSN 必须是 URL 形式（如 postgres://user:pass@host:5432/db?sslmode=disable），
// 因为用例会在其后追加 search_path 以隔离到独立 schema；关键字/值形式不支持追加参数。

const testSchema = "ld_test_s8"

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
	if err := gdb.AutoMigrate(&model.User{}, &model.APIKey{}, &model.Upload{}, &model.File{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// withTestStorage 把本地存储根指向临时目录（避免污染仓库工作区）。
// localStorageCfg 是「显式写本地磁盘」的最小 storage 配置。
//
// 必须显式给 active：缺 storage 键时写路径会拒绝（503），这是生产语义 ——
// 测试不能靠"缺配置就当本地"来跑，否则被测的分支与线上不是同一条。
const localStorageCfg = `{"active":"none","channels":{"none":{}}}`

func withTestStorage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("STORAGE_DIR", dir)
	conf.Load()
	conf.System().Set("storage", json.RawMessage(localStorageCfg))
	return dir
}

// snapshotSystem 在用例结束时还原 system_config 内存镜像。
func snapshotSystem(t *testing.T) {
	t.Helper()
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })
}

// setUploads 覆写 uploads 配置项。
func setUploads(t *testing.T, s uploadcfg.Settings) {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("序列化 uploads 配置失败: %v", err)
	}
	conf.System().Set("uploads", raw)
}

func seedUserAndKey(t *testing.T, gdb *gorm.DB) (uint64, uint64) {
	t.Helper()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	u := &model.User{Username: "s8" + suffix, PasswordHash: "x", Group: "default", AffCode: "aff" + suffix}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造测试用户失败: %v", err)
	}
	k := &model.APIKey{UserID: u.ID, KeyCipher: "x", KeyHash: "h" + suffix, Name: "上传测试KEY", Status: 1, Group: "default"}
	if err := gdb.Create(k).Error; err != nil {
		t.Fatalf("造测试 KEY 失败: %v", err)
	}
	return u.ID, k.ID
}

// pngBody 造一个最小合法 PNG（魔数 + 填充）。
func pngBody(n int) []byte {
	body := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x33}, n)...)
	return body
}

func TestCreateDirectDelete(t *testing.T) {
	withTestDB(t)
	dir := withTestStorage(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{"storage": json.RawMessage(localStorageCfg)})
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, db.DB)

	body := pngBody(120)
	rec, err := Create(ctx, CreateInput{UserID: uid, KeyID: kid, Filename: "照 片.png", Size: int64(len(body)), Reader: bytes.NewReader(body)})
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	// 「照 片.png」：照(3字节)+空格(1)+片(3字节) = 7 个非白名单字节，逐字节替换为下划线。
	if rec.SizeBytes != int64(len(body)) || rec.ContentType != "image/png" || rec.SafeName != strings.Repeat("_", 7)+".png" {
		t.Fatalf("落库字段不符: %+v", rec)
	}
	// key 末段带文件名：云端地址是跨域直链，下载名只能取自 URL 最后一段。
	wantKey := "files/" + strconv.FormatUint(uid, 10) + "/" + rec.ID + "/" + rec.SafeName
	if rec.StorageKey != wantKey {
		t.Fatalf("存储路径不符: got=%s want=%s", rec.StorageKey, wantKey)
	}
	if d := time.Until(rec.ExpiresAt); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("有效期应为 24h，实际 %v", d)
	}

	view := View(rec, "上传测试KEY", "https://api.example.com/")
	if view.URL != "https://api.example.com/f/"+rec.ID+"/"+rec.SafeName {
		t.Fatalf("直链拼接不符: %s", view.URL)
	}

	d, err := OpenDirect(ctx, rec.ID)
	if err != nil {
		t.Fatalf("直链取回失败: %v", err)
	}
	got, _ := readAll(d)
	d.Body.Close()
	if !bytes.Equal(got, body) || d.Size != int64(len(body)) {
		t.Fatalf("直链内容不符: size=%d len=%d", d.Size, len(got))
	}

	keyID := kid
	if err := Delete(ctx, rec.ID, Scope{UserID: uid, KeyID: &keyID}); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := OpenDirect(ctx, rec.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("删除后直链应判过期: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, rec.StorageKey)); !os.IsNotExist(err) {
		t.Fatalf("存储对象应已删除: %v", err)
	}
	if err := Delete(ctx, rec.ID, Scope{UserID: uid}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应 404 语义: %v", err)
	}
}

func TestCreateGuards(t *testing.T) {
	withTestDB(t)
	withTestStorage(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{"storage": json.RawMessage(localStorageCfg)})
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, db.DB)
	base := CreateInput{UserID: uid, KeyID: kid, Filename: "a.png", Reader: bytes.NewReader(pngBody(16))}

	cases := []struct {
		name string
		in   CreateInput
		want error
	}{
		{"扩展名白名单外", CreateInput{UserID: uid, KeyID: kid, Filename: "a.exe", Size: 16, Reader: bytes.NewReader(pngBody(16))}, ErrTypeUnsupported},
		{"魔数不符", CreateInput{UserID: uid, KeyID: kid, Filename: "a.png", Size: 62, Reader: bytes.NewReader(append([]byte("MZ"), bytes.Repeat([]byte{0}, 60)...))}, ErrTypeUnsupported},
		{"大小声明为零", CreateInput{UserID: uid, KeyID: kid, Filename: "a.png", Size: 0, Reader: bytes.NewReader(pngBody(16))}, ErrEmpty},
	}
	for _, c := range cases {
		if _, err := Create(ctx, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}

	// 超单文件上限：声明值即拦截（无落盘）
	big := base
	big.Size = (uploadcfg.Defaults().Limits[uploadcfg.KindImage] << 20) + 1
	if _, err := Create(ctx, big); !errors.Is(err, ErrTooLarge) {
		t.Errorf("超单文件上限应 413 语义: %v", err)
	}

	// 总闸关闭
	off := uploadcfg.Defaults()
	off.Enabled = false
	setUploads(t, off)
	if _, err := Create(ctx, base); !errors.Is(err, ErrDisabled) {
		t.Errorf("总闸关闭应 ErrDisabled: %v", err)
	}
}

func TestCreateLimits(t *testing.T) {
	gdb := withTestDB(t)
	withTestStorage(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{"storage": json.RawMessage(localStorageCfg)})
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, db.DB)

	// 日限：limit=1，第二次拒绝且不落行
	s := uploadcfg.Defaults()
	s.DailyLimitPerKey = 1
	setUploads(t, s)
	body := pngBody(32)
	first, err := Create(ctx, CreateInput{UserID: uid, KeyID: kid, Filename: "1.png", Size: int64(len(body)), Reader: bytes.NewReader(body)})
	if err != nil {
		t.Fatalf("首次上传失败: %v", err)
	}
	if _, err := Create(ctx, CreateInput{UserID: uid, KeyID: kid, Filename: "2.png", Size: int64(len(body)), Reader: bytes.NewReader(body)}); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("日限应拒绝: %v", err)
	}
	var n int64
	gdb.Model(&model.Upload{}).Where("key_id = ?", kid).Count(&n)
	if n != 1 {
		t.Fatalf("日限拒绝时不应落行，实际 %d 行", n)
	}

	// 容量：在存 32B + 新声明的 1MB > 上限 1MB
	s = uploadcfg.Defaults()
	s.UserTotalMB = 1
	setUploads(t, s)
	if _, err := Create(ctx, CreateInput{UserID: uid, KeyID: kid, Filename: "3.png", Size: (1 << 20), Reader: bytes.NewReader(pngBody(1 << 20))}); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("容量上限应拒绝: %v", err)
	}

	// 容量按「未过期件」计算：把首件置为已过期后即可通过
	if err := gdb.Model(&model.Upload{}).Where("id = ?", first.ID).Update("expires_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatalf("改过期时间失败: %v", err)
	}
	if _, err := Create(ctx, CreateInput{UserID: uid, KeyID: kid, Filename: "4.png", Size: 4096, Reader: bytes.NewReader(pngBody(4096))}); err != nil {
		t.Fatalf("过期件不应占用容量: %v", err)
	}
}

func TestListScopedByKeyAndStats(t *testing.T) {
	gdb := withTestDB(t)
	withTestStorage(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{"storage": json.RawMessage(localStorageCfg)})
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, db.DB)
	k2 := &model.APIKey{UserID: uid, KeyCipher: "x", KeyHash: "h2" + strconv.FormatInt(time.Now().UnixNano(), 36), Name: "第二KEY", Status: 1, Group: "default"}
	if err := gdb.Create(k2).Error; err != nil {
		t.Fatalf("造第二 KEY 失败: %v", err)
	}
	body := pngBody(64)
	for _, kk := range []uint64{kid, kid, k2.ID} {
		if _, err := Create(ctx, CreateInput{UserID: uid, KeyID: kk, Filename: "f.png", Size: int64(len(body)), Reader: bytes.NewReader(body)}); err != nil {
			t.Fatalf("上传失败: %v", err)
		}
	}

	items, total, err := List(ctx, ListFilter{UserID: uid, KeyID: kid, Offset: 0, Limit: 10}, "https://api.example.com")
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if total != 2 || len(items) != 2 || items[0].KeyName != "上传测试KEY" || !strings.HasSuffix(items[0].URL, "/"+items[0].SafeName) {
		t.Fatalf("KEY 维度列表不符: total=%d items=%+v", total, items)
	}
	all, totalAll, err := List(ctx, ListFilter{UserID: uid, Offset: 0, Limit: 10}, "https://api.example.com")
	if err != nil || totalAll != 3 || len(all) != 3 {
		t.Fatalf("跨 KEY 列表不符: total=%d err=%v", totalAll, err)
	}
	names := map[string]bool{}
	for i := range all {
		names[all[i].KeyName] = true
	}
	if !names["上传测试KEY"] || !names["第二KEY"] {
		t.Fatalf("跨 KEY 列表应带 KEY 名称: %+v", names)
	}

	stats, err := StatsOf(ctx, uid)
	if err != nil || stats.TodayCount != 3 || stats.TotalBytes != int64(3*len(body)) {
		t.Fatalf("账号用量不符: %+v err=%v", stats, err)
	}

	// 归属隔离：用另一用户删本用户文件 → 404 语义
	other := Scope{UserID: uid + 999}
	if err := Delete(ctx, items[0].ID, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨用户删除应拒: %v", err)
	}
	// 跨 KEY 删除：/v1 语义（限定 KeyID）拒绝，门户语义（不限 KEY）放行
	keyID := k2.ID
	if err := Delete(ctx, items[0].ID, Scope{UserID: uid, KeyID: &keyID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨 KEY 删除应拒: %v", err)
	}
	if err := Delete(ctx, items[0].ID, Scope{UserID: uid}); err != nil {
		t.Fatalf("门户跨 KEY 删除应放行: %v", err)
	}
}

func TestPurgeExpired(t *testing.T) {
	gdb := withTestDB(t)
	dir := withTestStorage(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{"storage": json.RawMessage(localStorageCfg)})
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, db.DB)
	body := pngBody(48)
	rec, err := Create(ctx, CreateInput{UserID: uid, KeyID: kid, Filename: "old.png", Size: int64(len(body)), Reader: bytes.NewReader(body)})
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	// 到期后直链即 410（清理任务尚未跑）
	if err := gdb.Model(&model.Upload{}).Where("id = ?", rec.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("改过期时间失败: %v", err)
	}
	if _, err := OpenDirect(ctx, rec.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("到期应判过期: %v", err)
	}
	st, err := PurgeExpired(ctx, 100)
	if err != nil || st.Deleted != 1 {
		t.Fatalf("清理应处理 1 条: %+v err=%v", st, err)
	}
	var cnt int64
	gdb.Model(&model.Upload{}).Where("id = ?", rec.ID).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("记录应已删除")
	}
	if _, err := os.Stat(filepath.Join(dir, rec.StorageKey)); !os.IsNotExist(err) {
		t.Fatalf("对象应已删除: %v", err)
	}
}

// TestPurgeExpiredNotStarvedByDeadBackend 守住「清理队列不被不可用后端饿死」。
//
// 场景：一条较早到期的记录，它当初写入的后端（oss）已从配置里移除；
// 另一条较晚到期的记录落在本地磁盘。查询是 ORDER BY expires_at LIMIT batch，
// 若不过滤「后端仍可构建」，每轮都先选中那条 oss 记录、逐条 continue 跳过，
// 正常的本地记录永远排不进 batch —— 表现为「到期文件永远清不掉、磁盘只涨不降」，
// 而日志里只有一条条「后端已移除」，看不出是队列饿死。
func TestPurgeExpiredNotStarvedByDeadBackend(t *testing.T) {
	gdb := withTestDB(t)
	withTestStorage(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{"storage": json.RawMessage(localStorageCfg)})
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, db.DB)

	body := pngBody(48)
	rec, err := Create(ctx, CreateInput{UserID: uid, KeyID: kid, Filename: "live.png", Size: int64(len(body)), Reader: bytes.NewReader(body)})
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if err := gdb.Model(&model.Upload{}).Where("id = ?", rec.ID).
		Update("expires_at", time.Now().Add(-2*time.Minute)).Error; err != nil {
		t.Fatalf("改过期时间失败: %v", err)
	}

	// 更早到期、但后端已不在配置里：这条会被「后端可构建」过滤掉，而不是选中后跳过。
	dead := &model.Upload{
		ID: "01J8ZQ7W6Y5X4V3T2S1R0Q9P8D", UserID: uid, KeyID: kid,
		Filename: "dead.png", SafeName: "dead.png", SizeBytes: 1, ContentType: "image/png",
		StorageBackend: "oss", StorageKey: "lost/dead",
		CreatedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}
	if err := gdb.Create(dead).Error; err != nil {
		t.Fatalf("造「后端已移除」记录失败: %v", err)
	}

	st, err := PurgeExpired(ctx, 1)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if st.Deleted != 1 {
		t.Fatalf("batch=1 时应清掉那条正常记录（%+v）", st)
	}
	var live int64
	gdb.Model(&model.Upload{}).Where("id = ?", rec.ID).Count(&live)
	if live != 0 {
		t.Fatalf("正常的到期记录应已删除")
	}
	// 记录本身不能凭空丢：对象还在桶里，补回配置后才能删，删记录等于制造孤儿对象。
	var still int64
	gdb.Model(&model.Upload{}).Where("id = ?", dead.ID).Count(&still)
	if still != 1 {
		t.Fatalf("后端不可用的记录必须保留（等配置恢复后再删对象）")
	}
}

// TestDirectURLDecisions 覆盖 /f/ 302 的判定：只由**记录的后端**决定。
//
// 三个分支必须是三种答案，混起来就会出真故障：
//   - 云后端 → 返回对象存储的公开地址（永不签名、无有效期）；
//   - 本地后端 → ok=false，调用方走代理（本地对象没有对外地址，不是错误）；
//   - 已过期/不存在 → ErrExpired（410）。若把「过期」折成 ok=false，
//     调用方会去代理一个必然 404 的对象，用户看到的是 502 而不是「文件已过期」。
func TestDirectURLDecisions(t *testing.T) {
	gdb := withTestDB(t)
	withTestStorage(t)
	snapshotSystem(t)
	const ossCfg = `{"active":"none","channels":{"none":{},"oss":{` +
		`"endpoint":"oss-cn-shanghai.aliyuncs.com","bucket":"ling-ai",` +
		`"access_key_id":"ak","access_key_secret":"sk"}}}`
	conf.System().Set("storage", json.RawMessage(ossCfg))
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, db.DB)

	body := pngBody(48)
	rec, err := Create(ctx, CreateInput{UserID: uid, KeyID: kid, Filename: "p.png", Size: int64(len(body)), Reader: bytes.NewReader(body)})
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	setExpiry := func(d time.Duration) {
		t.Helper()
		if err := gdb.Model(&model.Upload{}).Where("id = ?", rec.ID).
			Update("expires_at", time.Now().Add(d)).Error; err != nil {
			t.Fatalf("改过期时间失败: %v", err)
		}
	}

	// ① 本地记录：没有云地址，交回代理分支。
	if url, ok, err := DirectURL(ctx, rec.ID); err != nil || ok || url != "" {
		t.Fatalf("本地后端应走代理: url=%q ok=%v err=%v", url, ok, err)
	}

	// ② 同一条记录换成云后端：地址只由行内 storage_backend/bucket 决定，与 active 无关。
	if err := gdb.Model(&model.Upload{}).Where("id = ?", rec.ID).
		Updates(map[string]interface{}{
			"storage_backend": "oss", "storage_bucket": "ling-ai", "storage_key": "files/1/x.png",
		}).Error; err != nil {
		t.Fatalf("改后端失败: %v", err)
	}
	url, ok, err := DirectURL(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("云后端应返回地址: ok=%v err=%v", ok, err)
	}
	if want := "https://ling-ai.oss-cn-shanghai.aliyuncs.com/files/1/x.png"; url != want {
		t.Fatalf("地址不符\n got=%s\nwant=%s", url, want)
	}

	// ③ 过期：ErrExpired（调用方映射 410），不能返回「降级」让代理去 404。
	setExpiry(-time.Minute)
	if _, _, err := DirectURL(ctx, rec.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("到期应判 ErrExpired: %v", err)
	}

	// ④ 不存在：同样 ErrExpired（不暴露「这个 id 曾经存在」）。
	if _, _, err := DirectURL(ctx, "01J8ZQ7W6Y5X4V3T2S1R0Q9P8N"); !errors.Is(err, ErrExpired) {
		t.Fatalf("不存在应判 ErrExpired: %v", err)
	}
}

// readAll 读取直链内容（避免 import io 只为 io.ReadAll）。
func readAll(d *Direct) ([]byte, error) {
	buf := &bytes.Buffer{}
	_, err := buf.ReadFrom(d.Body)
	return buf.Bytes(), err
}

// TestCreateLimitsPerKind 守住「体积上限按类型」这条判据。
//
// 改造前只有一个全局 max_size_mb（外加 middleware 里写死的 25 MiB），
// 结果是「所有类型共用一个上限」。这里把图片类压到 1MB：同一份配置下
// 1.2MB 的图片必须被拒，而 1.2MB 的视频必须放行 —— 两者不可能同时成立
// 于「一个全局上限」的实现。
func TestCreateLimitsPerKind(t *testing.T) {
	withTestDB(t)
	withTestStorage(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{"storage": json.RawMessage(localStorageCfg)})
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, db.DB)

	s := uploadcfg.Defaults()
	s.Limits[uploadcfg.KindImage] = 1
	s.Limits[uploadcfg.KindVideo] = 2
	s.Compress.Enabled = false // 本用例只测限额，不引入压缩
	setUploads(t, s)

	pngBig := pngBody(1200 << 10)
	if _, err := Create(ctx, CreateInput{
		UserID: uid, KeyID: kid, Filename: "big.png", Size: int64(len(pngBig)), Reader: bytes.NewReader(pngBig),
	}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("1.2MB 图片超过 1MB 上限，应 ErrTooLarge：%v", err)
	}

	mp4 := append([]byte{0, 0, 0, 0x18}, []byte("ftypmp42")...)
	mp4 = append(mp4, make([]byte, (1200<<10)-len(mp4))...)
	rec, err := Create(ctx, CreateInput{
		UserID: uid, KeyID: kid, Filename: "clip.mp4", Size: int64(len(mp4)), Reader: bytes.NewReader(mp4),
	})
	if err != nil {
		t.Fatalf("1.2MB 视频在 2MB 上限内应通过：%v", err)
	}
	if rec.ContentType != "video/mp4" || rec.SizeBytes != int64(len(mp4)) {
		t.Fatalf("落库元信息不符: %+v", rec)
	}
}

// TestCreateCompressedMetadata 守住压缩后的落库元信息与新后缀。
func TestCreateCompressedMetadata(t *testing.T) {
	withTestDB(t)
	withTestStorage(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{"storage": json.RawMessage(localStorageCfg)})
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, db.DB)

	s := uploadcfg.Defaults()
	s.Compress = uploadcfg.Compress{Enabled: true, Quality: 60, Format: uploadcfg.FormatWebP}
	setUploads(t, s)

	body := noisePNG(t, 640, 480)
	rec, err := Create(ctx, CreateInput{
		UserID: uid, KeyID: kid, Filename: "shot.png", Size: int64(len(body)), Reader: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if rec.ContentType != "image/webp" {
		t.Fatalf("应落成 image/webp，实际 %s", rec.ContentType)
	}
	if !strings.HasSuffix(rec.SafeName, ".webp") || !strings.HasSuffix(rec.Filename, ".webp") {
		t.Fatalf("文件名后缀应同步为 webp: %s / %s", rec.Filename, rec.SafeName)
	}
	if !strings.HasSuffix(rec.StorageKey, ".webp") {
		t.Fatalf("对象 key 末段应带新后缀: %s", rec.StorageKey)
	}
	if rec.SizeBytes >= int64(len(body)) {
		t.Fatalf("记录体积应是压缩后的字节数：rec=%d 原始=%d", rec.SizeBytes, len(body))
	}
}

package icon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件覆盖图标上传的**落库 + 存储**语义（必须连库 + 真实本地盘）：
// 幂等去重、并发唯一冲突回查、存储回落、超大/类型拒绝。
// 这些错都不会在"上传成功"时暴露：要么运营看到两张一样的图，要么图裂。

const iconTestSchema = "ld_test_icon"

func withIconEnv(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + iconTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + iconTestSchema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+iconTestSchema), silent)
	if err != nil {
		t.Fatalf("连接 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Icon{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 与生产同一条唯一索引（db/indexes.go 的 uq_icons_sha256）：
	// 少了它，用例里的"幂等/并发"就只是内存行为，测不到真正兜底的那条路径。
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_icons_sha256 ON icons (sha256)`).Error; err != nil {
		t.Fatalf("建去重索引失败: %v", err)
	}

	// 存储根目录指到临时目录；storage.active=none 让 Active() 返回本地盘。
	t.Setenv("STORAGE_DIR", t.TempDir())
	conf.Load()
	prevStorage, hadStorage := conf.System().Get("storage")
	conf.System().Set("storage", []byte(`{"active":"none"}`))
	prevDB := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if hadStorage {
			conf.System().Set("storage", prevStorage)
		}
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + iconTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prevDB
	})
	return gdb
}

// fakePNG 造一段带合法 PNG 魔数的内容（SniffImage 只看头部，内容不必是真图）。
// 尾部用种子填充，让不同用例得到不同 sha256。
func fakePNG(seed byte, n int) []byte {
	b := make([]byte, n)
	copy(b, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	for i := 8; i < n; i++ {
		b[i] = seed
	}
	return b
}

// fakePNGCanvas 在 fakePNG 之上写一个真实的 IHDR，让 ImageSize 能读出宽高。
//
// 长度仍是几十字节：这正是要守的场景 —— 8000×8000 的**纯色** PNG 压完就这么大，
// 字节上限完全放行，拦它的只能是尺寸上限。
func fakePNGCanvas(w, h uint32, n int) []byte {
	b := fakePNG(0, n)
	copy(b[8:], []byte{0, 0, 0, 13, 'I', 'H', 'D', 'R'})
	binary.BigEndian.PutUint32(b[16:], w)
	binary.BigEndian.PutUint32(b[20:], h)
	return b
}

// save 是"调用方视角"的最小封装。
func save(t *testing.T, body []byte) (*model.Icon, error) {
	t.Helper()
	return Save(context.Background(), bytes.NewReader(body), int64(len(body)), 7)
}

func TestSaveIsIdempotentByContent(t *testing.T) {
	withIconEnv(t)
	body := fakePNG(9, 1024)

	first, err := save(t, body)
	if err != nil {
		t.Fatalf("首次上传失败: %v", err)
	}
	second, err := save(t, body)
	if err != nil {
		t.Fatalf("重复上传失败: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("同一张图应复用同一行：%s vs %s", first.ID, second.ID)
	}
	if first.Ext != "png" || first.MIME != "image/png" || first.Size != int64(len(body)) {
		t.Fatalf("落库字段不符: %+v", first)
	}
	var n int64
	if err := db.DB.Model(&model.Icon{}).Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应只有一行，实际 %d 行", n)
	}
	// 对象真的写到盘上了：否则"上传成功、图片 404"。
	if _, err := os.Stat(filepath.Join(conf.EnvConfig().StorageDir, first.ObjectKey())); err != nil {
		t.Fatalf("对象未落盘: %v", err)
	}
}

func TestSaveRejectsOversizeAndBadType(t *testing.T) {
	withIconEnv(t)
	if _, err := save(t, fakePNG(1, MaxBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超 512KB 应返回 ErrTooLarge，实际 %v", err)
	}
	// 声明大小撒谎（fh.Size 说 10 字节，流里是 1MB）也必须靠实际字节挡住。
	liar := bytes.NewReader(fakePNG(2, MaxBytes+1))
	if _, err := Save(context.Background(), liar, 10, 7); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("实际字节超限应返回 ErrTooLarge，实际 %v", err)
	}
	// SVG 与 GIF 都不收。
	if _, err := save(t, []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)); !errors.Is(err, ErrTypeUnsupported) {
		t.Fatalf("SVG 应被拒绝，实际 %v", err)
	}
	if _, err := save(t, []byte("GIF89a....")); !errors.Is(err, ErrTypeUnsupported) {
		t.Fatalf("GIF 应被拒绝，实际 %v", err)
	}
	if _, err := save(t, nil); !errors.Is(err, ErrEmpty) {
		t.Fatalf("空文件应返回 ErrEmpty，实际 %v", err)
	}
	var n int64
	if err := db.DB.Model(&model.Icon{}).Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("被拒绝的上传不得留下行，实际 %d 行", n)
	}
}

// TestSaveRejectsHugeCanvas 守「边长上限」，它是**字节上限拦不住**的那一类。
//
// 8000×8000 纯色 PNG 只有几十 KB（压缩比极高），512KB 的字节校验一路放行；
// 而选择器要同时渲染 72 + N 张图，8000×8000 RGBA 解码 ≈ 256MB ——
// 一张就能把标签页打死，且服务端没有任何报错可查。
func TestSaveRejectsHugeCanvas(t *testing.T) {
	withIconEnv(t)

	if _, err := save(t, fakePNGCanvas(8000, 8000, 64)); !errors.Is(err, ErrEdgeTooLarge) {
		t.Fatalf("8000×8000 应返回 ErrEdgeTooLarge，实际 %v", err)
	}
	// 单边超限也要拒（宽 1024 合规、高 4000 不合规）。
	if _, err := save(t, fakePNGCanvas(1024, 4000, 64)); !errors.Is(err, ErrEdgeTooLarge) {
		t.Fatalf("单边超限应返回 ErrEdgeTooLarge，实际 %v", err)
	}
	var n int64
	if err := db.DB.Model(&model.Icon{}).Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("被拒绝的上传不得留下行，实际 %d 行", n)
	}

	// 边界值必须放行（= MaxEdge 而非 < MaxEdge）：否则「1024 的图传不上去」，
	// 而文档写的是"边长不超过 1024"。
	if _, err := save(t, fakePNGCanvas(MaxEdge, MaxEdge, 64)); err != nil {
		t.Fatalf("1024×1024 应被接受，实际 %v", err)
	}
}

// openIcon 把「取元数据 + 开对象流」两步合成一次调用（与控制器同一条路径）。
func openIcon(ctx context.Context, id string) (io.ReadCloser, int64, *model.Icon, error) {
	row, err := Lookup(ctx, id)
	if err != nil {
		return nil, 0, nil, err
	}
	rc, n, err := OpenRow(ctx, row)
	if err != nil {
		return nil, 0, nil, err
	}
	return rc, n, row, nil
}

func TestOpenStreamsBackAndFallsBackToLocal(t *testing.T) {
	withIconEnv(t)
	body := fakePNG(3, 2048)
	rec, err := save(t, body)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}

	rc, size, got, err := openIcon(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(buf, body) {
		t.Fatal("读回内容与上传不一致")
	}
	if size != int64(len(body)) {
		t.Fatalf("长度不符: %d vs %d", size, len(body))
	}
	if got.SHA256 != rec.SHA256 {
		t.Fatalf("回查到的行不是同一行: %s vs %s", got.SHA256, rec.SHA256)
	}

	// 非法 id 形态（含路径穿越字符）一律 NotFound，不打数据库。
	for _, bad := range []string{"", "../../etc/passwd", "a/b", strings.Repeat("z", 27)} {
		if _, _, _, err := openIcon(context.Background(), bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("非法 id %q 应 NotFound，实际 %v", bad, err)
		}
	}

	// 大写 id 必须也能读：ULID 是大小写等价的 Crockford Base32，
	// 而库里的 id 恒为小写。不做归一的表现是「上传成功、立刻 404」，
	// 且错误信息只有「图标不存在」，从现象上完全看不出是大小写问题。
	up := strings.ToUpper(rec.ID)
	rcUp, _, _, err := openIcon(context.Background(), up)
	if err != nil {
		t.Fatalf("大写 id 应归一后命中，实际 %v", err)
	}
	rcUp.Close()

	// 存储配置指向一个**不可用**的后端：读路径必须回落本地盘（P1-5）。
	// 不回落的表现是「老图标全裂、新图标正常」，而库里每行都完好。
	conf.System().Set("storage", []byte(`{"active":"oss"}`))
	rc2, _, _, err := openIcon(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("存储不可用时应回落本地盘，实际 %v", err)
	}
	buf2, _ := io.ReadAll(rc2)
	rc2.Close()
	if !bytes.Equal(buf2, body) {
		t.Fatal("回落读取的内容不一致")
	}
}

func TestListIsNeverNil(t *testing.T) {
	withIconEnv(t)
	// 空表也必须是 []：返回 nil 会序列化成 null，前端 .length 抛错 → 选择器白屏。
	rows, err := List(context.Background(), 10)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if rows == nil {
		t.Fatal("空表必须返回空切片而不是 nil")
	}
	if len(rows) != 0 {
		t.Fatalf("空表应为 0 条，实际 %d", len(rows))
	}
}

// TestValidateChecksUploadExistence 守住写路径的那条新判据。
//
// 背景：brandicon.Normalize 只看形状，`upload:<合法 ulid>` 无论对象在不在库里都能通过。
// 于是「保存成功、对外 404 坏图」是可达的 —— 运营看到的是"我明明设过图标"。
// Validate 把形状校验之外的那一步补上，并且**只在写路径**调用（读路径的历史值
// 允许指向已删对象，那时应当照常显示、前端走首字）。
func TestValidateChecksUploadExistence(t *testing.T) {
	withIconEnv(t)
	ctx := context.Background()

	// 空值 = 清除图标，合法的"不设置"
	if v, err := Validate(ctx, ""); err != nil || v != "" {
		t.Fatalf("空值应通过并归一为空串，得到 (%q, %v)", v, err)
	}
	// 内置图标只查 catalog（形状 + key 是否存在）
	if v, err := Validate(ctx, "builtin:openai-gpt"); err != nil || v != "builtin:openai-gpt" {
		t.Fatalf("内置图标应通过，得到 (%q, %v)", v, err)
	}
	if _, err := Validate(ctx, "builtin:no-such-brand"); !errors.Is(err, brandicon.ErrInvalid) {
		t.Fatalf("不存在的内置 key 应报 ErrInvalid，得到 %v", err)
	}
	if _, err := Validate(ctx, "openai-gpt"); !errors.Is(err, brandicon.ErrInvalid) {
		t.Fatalf("裸 key 应报 ErrInvalid，得到 %v", err)
	}

	// 形状合法但不存在的 ulid：这是 Normalize 放行、Validate 必须拦下的那一类
	if _, err := Validate(ctx, "upload:01j9z3k4m5n6p7q8r9s0t1v2w3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的上传 id 应报 ErrNotFound，得到 %v", err)
	}

	// 真上传过的图标必须能通过（否则保存永远失败）
	row, err := save(t, fakePNG(9, 64))
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	tok := brandicon.UploadToken(row.ID)
	if v, err := Validate(ctx, tok); err != nil || v != tok {
		t.Fatalf("已存在的上传图标应通过，得到 (%q, %v)", v, err)
	}
}

package captcha

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
)

// setupRedis 起一个隔离的 Redis（默认 15 号库），用例结束清掉。
// 用 15 号库而不是 0 号：router 包的用例会 FlushDB，混用会互相清掉。
func setupRedis(t *testing.T) {
	t.Helper()
	addr := os.Getenv("LD_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	prev := db.RDB
	db.InitRedis(addr, "", 15)
	if !db.RedisReady() {
		if prev != nil {
			db.RDB = prev
		}
		t.Skip("Redis 不可用，跳过验证码用例")
	}
	t.Cleanup(func() {
		if db.RDB != nil {
			_ = db.RDB.FlushDB(context.Background()).Err()
			_ = db.RDB.Close()
		}
		db.RDB = prev
	})
}

// answerOf 直接读 Redis 拿答案 —— 这是"真实校验"的关键：
// 不走任何测试后门，证明「图上的答案」与「校验用的答案」是同一份。
func answerOf(t *testing.T, id string) string {
	t.Helper()
	v, err := db.RDB.Get(context.Background(), capPrefix+id).Result()
	if err != nil {
		t.Fatalf("读取答案失败: %v", err)
	}
	return v
}

func wantCode(t *testing.T, err error, want errs.Def) {
	t.Helper()
	got, ok := errs.AsDef(err)
	if !ok {
		t.Fatalf("期望 %s，拿到的是裸 error: %v", want.Code, err)
	}
	if got.Code != want.Code {
		t.Fatalf("期望 %s，实际 %s（%s）", want.Code, got.Code, got.Message)
	}
}

func TestNewThenVerify(t *testing.T) {
	setupRedis(t)
	ctx := context.Background()

	id, img, err := New(ctx)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(id) != 32 {
		t.Fatalf("id 应是 32 位 hex，实际 %d 位: %s", len(id), id)
	}
	if len(img) == 0 {
		t.Fatal("PNG 为空")
	}
	// 解开验证：确实是 240×80 的 PNG（不是坏字节）。
	m, err := png.Decode(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("PNG 解不开: %v", err)
	}
	if b := m.Bounds(); b.Dx() != 240 || b.Dy() != 80 {
		t.Fatalf("尺寸应为 240x80，实际 %dx%d", b.Dx(), b.Dy())
	}

	ans := answerOf(t, id)
	if len(ans) != digitsLen {
		t.Fatalf("答案应是 %d 位，实际 %q", digitsLen, ans)
	}
	if err := Verify(ctx, id, ans); err != nil {
		t.Fatalf("正确的答案应通过，实际 %v", err)
	}
}

// 一次性：同一份答案不能再用第二次。
func TestVerifyConsumesOnce(t *testing.T) {
	setupRedis(t)
	ctx := context.Background()

	id, _, err := New(ctx)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	ans := answerOf(t, id)
	if err := Verify(ctx, id, ans); err != nil {
		t.Fatalf("第一次应通过: %v", err)
	}
	wantCode(t, Verify(ctx, id, ans), errs.CaptchaInvalid)
}

func TestVerifyWrongAnswer(t *testing.T) {
	setupRedis(t)
	ctx := context.Background()

	id, _, err := New(ctx)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	ans := answerOf(t, id)
	wrong := "00000"
	if wrong == ans {
		wrong = "11111"
	}
	wantCode(t, Verify(ctx, id, wrong), errs.CaptchaInvalid)
}

func TestVerifyUnknownID(t *testing.T) {
	setupRedis(t)
	wantCode(t, Verify(context.Background(), "deadbeef", "12345"), errs.CaptchaInvalid)
}

// 过期等价于"取不到"：服务端不区分，用户看到同一句提示。
func TestVerifyExpired(t *testing.T) {
	setupRedis(t)
	ctx := context.Background()

	id, _, err := New(ctx)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	ans := answerOf(t, id)
	if err := db.RDB.Del(ctx, capPrefix+id).Err(); err != nil {
		t.Fatalf("模拟过期失败: %v", err)
	}
	wantCode(t, Verify(ctx, id, ans), errs.CaptchaInvalid)
}

func TestVerifyEmptyInputs(t *testing.T) {
	setupRedis(t)
	ctx := context.Background()
	wantCode(t, Verify(ctx, "", "12345"), errs.CaptchaRequired)
	wantCode(t, Verify(ctx, "abc", ""), errs.CaptchaRequired)
	// 长度不对直接拒（不查 Redis）
	wantCode(t, Verify(ctx, "abc", "12"), errs.CaptchaInvalid)
}

// 生成 300 张必须全部可解码：库内部有 rng.Int(border, maxx-border)，
// 尺寸或参数不对会直接 panic —— 这条就是守它的。
func TestNewAlwaysDecodablePNG(t *testing.T) {
	setupRedis(t)
	ctx := context.Background()

	for i := 0; i < 300; i++ {
		_, img, err := New(ctx)
		if err != nil {
			t.Fatalf("第 %d 张生成失败: %v", i, err)
		}
		if _, err := png.Decode(bytes.NewReader(img)); err != nil {
			t.Fatalf("第 %d 张 PNG 解不开: %v", i, err)
		}
		// base64 后体积要可控：前端是一次性把图塞进 JSON 的。
		if len(img) > 30_000 {
			t.Fatalf("第 %d 张 PNG 过大: %d 字节", i, len(img))
		}
	}
}

func TestTTLIsBounded(t *testing.T) {
	setupRedis(t)
	id, _, err := New(context.Background())
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	ttl, err := db.RDB.TTL(context.Background(), capPrefix+id).Result()
	if err != nil {
		t.Fatalf("读 TTL 失败: %v", err)
	}
	// 容差给 10 秒：生成与断言之间的耗时。
	if ttl <= 0 || ttl > capTTL+10*time.Second {
		t.Fatalf("TTL 应在 (0, %s]，实际 %s", capTTL, ttl)
	}
}

// 答案必须是 5 位 ASCII 数字 —— 库里 RandomDigits 返回的是 0-9 数值字节，
// 直接 string() 会得到不可见控制字符，导致"图与答案永远对不上"。
func TestStoredAnswerIsASCIIDigits(t *testing.T) {
	setupRedis(t)
	id, _, err := New(context.Background())
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	ans := answerOf(t, id)
	for _, c := range ans {
		if c < '0' || c > '9' {
			t.Fatalf("答案里出现非数字字节: %q", ans)
		}
	}
	if strings.TrimSpace(ans) != ans {
		t.Fatalf("答案含空白: %q", ans)
	}
}

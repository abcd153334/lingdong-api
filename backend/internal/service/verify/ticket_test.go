package verify

import (
	"context"
	"errors"
	"os"
	"testing"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
)

// testRedis 初始化测试用 Redis（DB 15），不可用则跳过。
func testRedis(t *testing.T) {
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
		t.Skip("Redis 不可用，跳过凭据往返用例")
	}
	t.Cleanup(func() {
		if db.RDB != nil {
			_ = db.RDB.FlushDB(context.Background()).Err()
			_ = db.RDB.Close()
		}
		db.RDB = prev
	})
}

// TestRedeemTicketOnce 覆盖「签发 → 兑换 → 再兑换失败」。
//
// 这条路径同样是「取出即删」：曾经用 GETDEL（Redis ≥ 6.2），在 5.x 上直接报
// unknown command，表现是**找回密码最后一步永远失败**。用例跑在真实 Redis 上，
// 就是为了让版本兼容问题在门禁里暴露，而不是等用户改不了密码才发现。
func TestRedeemTicketOnce(t *testing.T) {
	testRedis(t)
	ctx := context.Background()

	ticket, ttl, err := IssueTicket(ctx, 42)
	if err != nil {
		t.Fatalf("签发凭据失败: %v", err)
	}
	if ticket == "" || ttl <= 0 {
		t.Fatalf("凭据或有效期异常: ticket=%q ttl=%d", ticket, ttl)
	}

	uid, err := RedeemTicket(ctx, ticket)
	if err != nil {
		t.Fatalf("兑换应成功，实际: %v（若为 auth_unavailable，多为 Redis 版本不支持所用命令）", err)
	}
	if uid != 42 {
		t.Fatalf("兑换出的 uid 不对: %d", uid)
	}
	if _, err := RedeemTicket(ctx, ticket); !errors.Is(err, errs.ResetTicketInvalid) {
		t.Fatalf("同一凭据第二次应 reset_ticket_invalid，实际: %v", err)
	}
}

// TestRedeemTicketUnknown 断言乱填的凭据走「无效」而不是「服务不可用」。
func TestRedeemTicketUnknown(t *testing.T) {
	testRedis(t)
	if _, err := RedeemTicket(context.Background(), "ldr_deadbeef"); !errors.Is(err, errs.ResetTicketInvalid) {
		t.Fatalf("未知凭据应 reset_ticket_invalid，实际: %v", err)
	}
	if _, err := RedeemTicket(context.Background(), "   "); !errors.Is(err, errs.ResetTicketInvalid) {
		t.Fatalf("空凭据应 reset_ticket_invalid，实际: %v", err)
	}
}

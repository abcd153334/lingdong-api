package session

import (
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件只测票据本身的密码学性质（纯函数，不需要 DB）。
// 「会话能建立/能撤销」属集成用例，靠 LD_TEST_DSN 开启。

// TestNewTicketFormat 断言票据形态：可辨识的前缀 + 足够熵 + 单独返回哈希。
//
// 三条都不是洁癖：
//   - 前缀让票据在日志/截图里一眼可辨（误贴到别处能被识别）；
//   - 32 字节（256 bit）是「不可猜」的下限，少了就等于给了爆破空间；
//   - 哈希必须与明文不同：库里只存哈希，拖库拿不到可用凭证。
func TestNewTicketFormat(t *testing.T) {
	ticket, hash, err := newTicket()
	if err != nil {
		t.Fatalf("生成票据失败: %v", err)
	}
	if !strings.HasPrefix(ticket, TicketPrefix) {
		t.Fatalf("票据前缀应为 %q，实际 %q", TicketPrefix, ticket)
	}
	body := strings.TrimPrefix(ticket, TicketPrefix)
	// base64.RawURLEncoding：无填充、只含 URL 安全字符（否则 Cookie/头里会被截断）
	if strings.ContainsAny(body, "+/=") {
		t.Fatalf("票据体必须 URL 安全（不含 + / =），实际 %q", body)
	}
	if len(body) < 43 { // 32 字节 base64url 无填充 = 43 字符
		t.Fatalf("票据体过短（熵不足），长度 %d", len(body))
	}
	if hash == ticket || strings.Contains(hash, ticket) {
		t.Fatal("哈希不得包含明文票据")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
		t.Fatalf("哈希应为 64 位小写 hex（sha256），实际 %q", hash)
	}
	if got := hashTicket(ticket); got != hash {
		t.Fatalf("hashTicket 应与生成时返回的哈希一致")
	}
}

// TestNewTicketUnique 断言连续生成的票据不重复。
//
// 随机源退化（例如误用 math/rand 种子固定）时，症状是「同一秒登录的所有人
// 共用一个会话」—— 这个用例是那条缺陷唯一的自动化拦网。
func TestNewTicketUnique(t *testing.T) {
	seen := make(map[string]bool, 512)
	for i := 0; i < 512; i++ {
		_, hash, err := newTicket()
		if err != nil {
			t.Fatalf("生成票据失败: %v", err)
		}
		if seen[hash] {
			t.Fatalf("第 %d 次生成了重复票据哈希", i)
		}
		seen[hash] = true
		if _, err := hex.DecodeString(hash); err != nil {
			t.Fatalf("哈希不是合法 hex: %v", err)
		}
	}
}

// TestHashTicketStable 断言同一票据的哈希稳定（否则每次校验都会「查不到会话」）。
func TestHashTicketStable(t *testing.T) {
	const tk = "lds_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	if hashTicket(tk) != hashTicket(tk) {
		t.Fatal("同一票据的哈希必须稳定")
	}
	if hashTicket(tk) == hashTicket(tk+"x") {
		t.Fatal("不同票据的哈希必须不同")
	}
}

// TestNewCSRFTokenFormat 断言 CSRF 令牌形态（非 hex 的 URL 安全串，且长度足够）。
func TestNewCSRFTokenFormat(t *testing.T) {
	a, err := NewCSRFToken()
	if err != nil {
		t.Fatalf("生成 CSRF 令牌失败: %v", err)
	}
	if len(a) < 32 {
		t.Fatalf("CSRF 令牌过短: %d", len(a))
	}
	b, _ := NewCSRFToken()
	if a == b {
		t.Fatal("两次生成的 CSRF 令牌不得相同")
	}
}

// TestRevokeReasonConstants 断言撤销原因都在契约枚举内。
//
// 原因是**审计字段**：前端与对账脚本按字符串判读，新增一个拼错的值
// 不会报错，只会让统计里多出一个孤立的桶。
func TestRevokeReasonConstants(t *testing.T) {
	all := []string{
		model.SessionRevokeLogout,
		model.SessionRevokeUserRevoked,
		model.SessionRevokeAdminRevoked,
		model.SessionRevokePasswordChanged,
		model.SessionRevokeMFAChanged,
		model.SessionRevokeIdle,
		model.SessionRevokeSuperseded,
		model.SessionRevokeMaxActive,
		model.SessionRevokePortalClosed,
	}
	seen := make(map[string]bool, len(all))
	for _, r := range all {
		if r == "" {
			t.Fatal("撤销原因不得为空")
		}
		if len(r) > 32 {
			// 列宽 size:32，超长会被 Postgres 拒绝（写入侧就没兜住）
			t.Fatalf("撤销原因 %q 超过列宽 32", r)
		}
		if seen[r] {
			t.Fatalf("撤销原因 %q 重复定义", r)
		}
		seen[r] = true
	}
}

package router_test

// 本文件守的是**应用级隔离的负向用例**：用应用 A 的托管密钥，拿不到应用 B 的
// 任何东西。每一条都对应一个真实存在过（或差点存在）的越权路径，注释里写清
// "删掉这段会怎样"，避免后续重构把它当成冗余判断删掉。

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// issueHosted 用会话票为某个应用签发一把托管密钥，返回明文。
func issueHosted(t *testing.T, r *gin.Engine, sessToken, slug string) string {
	t.Helper()
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/"+slug+"/hosted-key", sessToken, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("签发 %s 的托管密钥应 200，实际 %d %s", slug, rec.Code, rec.Body.String())
	}
	plain, _ := dataOf(t, rec)["plaintext"].(string)
	if plain == "" {
		t.Fatalf("首次签发必须返回明文: %s", rec.Body.String())
	}
	return plain
}

// hostedAppSlugs 调 GET /api/user/apps 并抽出 slug 列表。
func hostedAppSlugs(t *testing.T, r *gin.Engine, token string) []string {
	t.Helper()
	rec := doAPI(t, r, http.MethodGet, "/api/user/apps", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询已接入应用应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	items, _ := dataOf(t, rec)["items"].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		row, _ := it.(map[string]any)
		s, _ := row["slug"].(string)
		out = append(out, s)
	}
	return out
}

// TestUserAppListScopedToHostedApp 应用 A 的托管密钥只能看到应用 A。
//
// 删掉这条收口会怎样：返回该用户在**所有**应用下的 slug、应用名、key_id、
// 密钥前缀与权限集 —— 平台接入了哪些产品、用户在别处用什么凭证，全部可枚举。
// 会话票（用户本人）仍应看到全部，否则门户的"我接入的应用"会缺项。
func TestUserAppListScopedToHostedApp(t *testing.T) {
	r, token := setupUserAPI(t)
	a := seedApp(t, "iso-app-a")
	b := seedApp(t, "iso-app-b")

	plainA := issueHosted(t, r, token, a.Slug)
	_ = issueHosted(t, r, token, b.Slug)

	got := hostedAppSlugs(t, r, plainA)
	if len(got) != 1 || got[0] != a.Slug {
		t.Fatalf("托管密钥只应看到本应用 %q，实际 %v", a.Slug, got)
	}

	// 用户本人（会话票）视角不变：两个应用都在。
	mine := hostedAppSlugs(t, r, token)
	if len(mine) != 2 {
		t.Fatalf("会话票应看到 2 个已接入应用，实际 %v", mine)
	}
}

// TestUserSelfHidesPIIForHostedKey 托管密钥拿不到用户的邮箱/手机号/头像。
//
// 删掉这段脱敏会怎样：一把应用密钥泄露 = 用户身份信息泄露（邮箱、手机号）。
// 余额必须保留 —— 应用要靠它提示用户充值，拿掉等于让接入方无法判断能否继续用。
func TestUserSelfHidesPIIForHostedKey(t *testing.T) {
	r, token := setupUserAPI(t)
	app := seedApp(t, "iso-app-pii")
	plain := issueHosted(t, r, token, app.Slug)

	var u model.User
	if err := db.DB.Where("username = ?", "apiuser").First(&u).Error; err != nil {
		t.Fatalf("取测试用户失败: %v", err)
	}
	if err := db.DB.Model(&model.User{}).Where("id = ?", u.ID).
		Updates(map[string]any{"email": "pii@example.com", "phone": "+8613800000000"}).Error; err != nil {
		t.Fatalf("写入邮箱/手机号失败: %v", err)
	}

	// 会话票：完整资料（门户要展示）。
	self := dataOf(t, doAPI(t, r, http.MethodGet, "/api/user/self", token, nil))
	if self["email"] != "pii@example.com" {
		t.Fatalf("会话票应拿到完整邮箱，实际 %v", self["email"])
	}

	// 托管密钥：PII 置空，余额与冻结仍在。
	hosted := dataOf(t, doAPI(t, r, http.MethodGet, "/api/user/self", plain, nil))
	if hosted["email"] != "" || hosted["phone"] != "" || hosted["avatar_url"] != "" {
		t.Fatalf("托管密钥不得下发 PII，实际 email=%v phone=%v avatar=%v",
			hosted["email"], hosted["phone"], hosted["avatar_url"])
	}
	if _, ok := hosted["balance"]; !ok {
		t.Fatalf("余额必须保留（应用要靠它提示充值）: %s", "missing balance")
	}
}

// TestHostedKeyWithoutAppIsRejected 作用域自相矛盾时必须拒绝，而不是降级成整账号。
//
// `kind='hosted'` 但 `app_id=0` 只可能来自手工改库/数据修复。放它过去，
// 下游所有 `if app_id != 0` 的过滤都会静默失效 —— 应用密钥变成账号级凭证。
func TestHostedKeyWithoutAppIsRejected(t *testing.T) {
	r, token := setupUserAPI(t)
	app := seedApp(t, "iso-app-broken")
	plain := issueHosted(t, r, token, app.Slug)

	var u model.User
	if err := db.DB.Where("username = ?", "apiuser").First(&u).Error; err != nil {
		t.Fatalf("取测试用户失败: %v", err)
	}
	// 造脏数据：把归属抹掉，但保留 kind='hosted'。
	if err := db.DB.Model(&model.APIKey{}).
		Where("user_id = ? AND kind = ?", u.ID, model.KeyKindHosted).
		Update("app_id", 0).Error; err != nil {
		t.Fatalf("构造脏数据失败: %v", err)
	}

	rec := doAPI(t, r, http.MethodGet, "/api/user/self", plain, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("hosted 且无归属应用必须 403，实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "scope_denied" {
		t.Fatalf("错误码应为 scope_denied，实际 %q", env.ErrorCode)
	}
}

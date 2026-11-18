package router_test

// 本文件守的是**应用托管密钥的完整生命周期**：
// 签发 → 复用 → 可写 → 不可管凭证 → 门户不可操作 → 轮换 → 归因读取。
//
// 为什么用 HTTP 级用例而不是服务层单测：这里的每条约束都由**不同的层**实现
// （ScopeGuard 判鉴权级别、ownEditableKey 判门户可操作性、CreateHosted 判唯一性）。
// 只测服务层会漏掉守卫，只测守卫会漏掉门户拦截，而它们组合起来才是"接入方拿到
// 一把可以写数据但不能接管账号的凭证"。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/session"
)

// seedApp 造一行启用状态的应用。
func seedApp(t *testing.T, slug string) model.App {
	t.Helper()
	app := model.App{Slug: slug, Name: "测试应用", Status: model.AppStatusEnabled}
	if err := db.DB.Create(&app).Error; err != nil {
		t.Fatalf("造应用失败: %v", err)
	}
	return app
}

// TestHostedKeyLifecycle 覆盖托管密钥的签发、复用、权限边界与轮换。
func TestHostedKeyLifecycle(t *testing.T) {
	r, token := setupUserAPI(t)
	app := seedApp(t, "demo-app")

	// ① 首次签发：必须返回一次性明文。
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/demo-app/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("首次签发应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	first := dataOf(t, rec)
	plain, _ := first["plaintext"].(string)
	if plain == "" {
		t.Fatalf("首次签发必须返回明文: %s", rec.Body.String())
	}
	keyObj, _ := first["key"].(map[string]any)
	keyID, _ := keyObj["id"].(float64)
	if keyID == 0 {
		t.Fatalf("响应缺少 key.id: %s", rec.Body.String())
	}
	scopes, _ := keyObj["scopes"].([]any)
	if len(scopes) == 0 {
		t.Fatalf("托管密钥必须带权限集: %s", rec.Body.String())
	}
	hasTaskWrite := false
	for _, s := range scopes {
		if s == "task:write" {
			hasTaskWrite = true
		}
	}
	if !hasTaskWrite {
		t.Fatalf("权限集必须包含 task:write（契约里写端点 scope 的并集），实际 %v", scopes)
	}
	expectNoAuthCookie(t, rec, "签发托管密钥")

	// ② 重复签发（rotate=false）：同一把，且**不得**再次返回明文。
	// 若这里返回明文，接入方会以为"每次调用都能拿到凭证"，从而不在本地持久化。
	rec = doAPI(t, r, http.MethodPost, "/api/user/apps/demo-app/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("重复签发应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	second := dataOf(t, rec)
	if p, _ := second["plaintext"].(string); p != "" {
		t.Fatalf("重复签发不得返回明文，实际 %q", p)
	}
	if id, _ := second["key"].(map[string]any)["id"].(float64); id != keyID {
		t.Fatalf("重复签发必须返回同一把密钥，期望 id=%v，实际 %v", keyID, id)
	}

	// ③ 托管密钥可以写：取消一个不存在的任务 = 404（404 证明请求穿过了写面守卫）。
	rec = doAPI(t, r, http.MethodPost, "/api/user/tasks/task_00000000000000000000000000/cancel", plain, nil)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("托管密钥不应被写面守卫拒绝: %s", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("取消不存在的任务应 404，实际 %d %s", rec.Code, rec.Body.String())
	}

	// ④ 托管密钥不能创建新凭证：签发 KEY 是 Session 级，必须 session_required。
	rec = doAPI(t, r, http.MethodPost, "/api/user/keys", plain, map[string]any{"name": "self-replicate"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("托管密钥建 KEY 必须 403，实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "session_required" {
		t.Fatalf("错误码应为 session_required，实际 %q", env.ErrorCode)
	}

	// ⑤ 门户不能对托管密钥做重置/回显/删除 —— 否则应用会在毫无感知的情况下断流，
	//    或者应用凭证明文被导出到用户浏览器。
	keyPath := "/api/user/keys/" + strconv.FormatUint(uint64(keyID), 10)
	for _, tc := range []struct {
		name, method, path string
	}{
		{"重置", http.MethodPost, keyPath + "/reset"},
		{"回显明文", http.MethodGet, keyPath + "/plaintext"},
		{"删除", http.MethodDelete, keyPath},
		{"改状态", http.MethodPatch, keyPath + "/status"},
	} {
		rec := doAPI(t, r, tc.method, tc.path, token, map[string]any{"status": 2})
		if rec.Code != http.StatusConflict {
			t.Fatalf("门户%s托管密钥应 409，实际 %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if env := decodeEnv(t, rec); env.ErrorCode != "hosted_key_managed" {
			t.Fatalf("%s：错误码应为 hosted_key_managed，实际 %q", tc.name, env.ErrorCode)
		}
	}

	// ⑥ 轮换：返回新明文，**旧明文在重叠窗口内仍可用**（窗口终点由 previous_expires_at 给出）。
	//
	// 这条断言是本模块的可用性核心：轮换是"新旧过渡"，不是"立刻掐死旧钥"。
	// 旧实现断言的是 401（立即失效），那在单实例手工换配置时成立，但在多实例
	// 滚动发布下等于每次轮换都制造一次故障。
	rec = doAPI(t, r, http.MethodPost, "/api/user/apps/demo-app/hosted-key", token,
		map[string]any{"rotate": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("轮换应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	rotated := dataOf(t, rec)
	plain2, _ := rotated["plaintext"].(string)
	if plain2 == "" || plain2 == plain {
		t.Fatalf("轮换必须返回新的明文")
	}
	keyObj, _ = rotated["key"].(map[string]any)
	if prev, ok := keyObj["previous_expires_at"].(string); !ok || prev == "" {
		t.Fatalf("轮换必须回显旧钥窗口终点，实际 %v", keyObj["previous_expires_at"])
	}
	if rec := doAPI(t, r, http.MethodGet, "/api/user/self", plain, nil); rec.Code != http.StatusOK {
		t.Fatalf("重叠窗口内旧明文应仍可用，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := doAPI(t, r, http.MethodGet, "/api/user/self", plain2, nil); rec.Code != http.StatusOK {
		t.Fatalf("轮换后新明文应可用，实际 %d %s", rec.Code, rec.Body.String())
	}

	// ⑦ GET /apps 归因：只返回已持有托管密钥的应用，app_id 与库内一致。
	rec = doAPI(t, r, http.MethodGet, "/api/user/apps", plain2, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询已接入应用应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	items, _ := dataOf(t, rec)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("应只有 1 个已接入应用，实际 %d: %s", len(items), rec.Body.String())
	}
	row, _ := items[0].(map[string]any)
	if row["slug"] != app.Slug {
		t.Fatalf("slug 应为 %q，实际 %v", app.Slug, row["slug"])
	}
	if id, _ := row["app_id"].(float64); uint64(id) != app.ID {
		t.Fatalf("app_id 应为 %d，实际 %v", app.ID, row["app_id"])
	}
}

// TestHostedKeyIssueRejectsDisabledApp 停用的应用拒绝签发。
//
// 停用语义是"停止新接入"，不是"立刻断流"：已签发的密钥仍可用。
// 这条用例只覆盖"签发被拒"，避免有人把停用实现成"顺带禁用全部密钥"。
//
// 错误码与"应用不存在"**合并**为 app_not_found：分开会让调用方用两次请求
// 就能判断"这个 slug 存在但被停用了"，而 slug 是管理员手填的可读串、无随机化，
// 枚举成本只有一次请求。真正接了该应用的一方拿到 app_not_found 也够用
// （去找平台管理员），不需要知道是"没有"还是"停了"。
func TestHostedKeyIssueRejectsDisabledApp(t *testing.T) {
	r, token := setupUserAPI(t)
	app := seedApp(t, "frozen-app")
	if err := db.DB.Model(&model.App{}).Where("id = ?", app.ID).
		Update("status", model.AppStatusDisabled).Error; err != nil {
		t.Fatalf("停用应用失败: %v", err)
	}
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/frozen-app/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("停用应用签发应 404（与不存在同码），实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "app_not_found" {
		t.Fatalf("错误码应为 app_not_found（与不存在合并），实际 %q", env.ErrorCode)
	}
}

// TestHostedKeyIssueUnknownApp 未知 slug 返回 app_not_found（不泄漏"应用是否存在"之外的信息）。
func TestHostedKeyIssueUnknownApp(t *testing.T) {
	r, token := setupUserAPI(t)
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/nope-app/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知应用应 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "app_not_found" {
		t.Fatalf("错误码应为 app_not_found，实际 %q", env.ErrorCode)
	}
}

// TestHostedKeyPortalCookieFace 门户必须能用会话 Cookie 读到托管密钥清单。
//
// 这条守的是「凭证面不能混用」的另一侧：`/api/user/apps` 只认 Bearer（本文件
// 其余用例已从 Bearer 侧验证过），而门户浏览器手里只有 httpOnly 会话 Cookie，
// 前端 API 客户端也**只会**带 Cookie。若门户没有自己的路径，浏览器那条请求
// 必然 401，而 401 在前端的语义是"会话失效"（清登录态 + 守卫跳登录），
// 于是"读一次应用清单"被放大成"登录后立刻退出"——症状与真实掉线一模一样。
func TestHostedKeyPortalCookieFace(t *testing.T) {
	r, ticket := setupUserAPI(t)
	app := seedApp(t, "portal-app")

	// 用对外面（Bearer）签发，模拟用户此前已通过 API 接入该应用。
	issue := doAPI(t, r, http.MethodPost, "/api/user/apps/"+app.Slug+"/hosted-key", ticket, map[string]any{})
	if issue.Code != http.StatusOK {
		t.Fatalf("签发应 200，实际 %d %s", issue.Code, issue.Body.String())
	}

	// 同一张会话票作为门户 Cookie。
	cookieGet := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil)
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: ticket})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// 同一张会话票作为门户 Cookie：轮换必须可用（这是浏览器唯一的凭证）。
	// 读面（应用清单）已由密钥列表直出 app_slug，门户不再有第二条 Cookie 读端点。
	const csrf = "csrf-portal-face"
	req := httptest.NewRequest(http.MethodPost,
		"http://example.com/api/portal/apps/"+app.Slug+"/hosted-key",
		strings.NewReader(`{"rotate":true}`))
	req.Header.Set("Content-Type", "application/json")
	// 门户写面是 Cookie + CSRF 双提交：与浏览器的请求形状保持一致。
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set(session.CSRFHeaderName, csrf)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: ticket})
	req.AddCookie(&http.Cookie{Name: session.CSRFCookieName, Value: csrf})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("门户轮换应 200（浏览器只有 Cookie），实际 %d %s", rec.Code, rec.Body.String())
	}
	if plain, _ := dataOf(t, rec)["plaintext"].(string); plain == "" {
		t.Fatal("门户轮换必须返回新明文")
	}

	// 反证：同一凭证打对外面仍然 401 —— 门户路径不是"把 /api/user 改成读 Cookie"。
	if rec := cookieGet("/api/user/apps"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/user/apps 只带 Cookie 必须 401，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// ---- 应用停用：只拦首次接入 ----

// TestHostedKeyAppDisabledKeepsExisting 断言应用停用**只挡新接入**：
// 已持有密钥的用户仍能读取与轮换。
//
// 回归 A6：旧实现无条件 404（且检查位置在 allow_self_issue 之前），于是应用一停用，
// 已跑流量的接入方在窗口到期后无法换钥 —— 一个"暂停新接入"的动作变成断服。
func TestHostedKeyAppDisabledKeepsExisting(t *testing.T) {
	r, token := setupUserAPI(t)
	app := seedApp(t, "freeze-app")

	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/freeze-app/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("首次签发应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	firstPlain, _ := dataOf(t, rec)["plaintext"].(string)
	if firstPlain == "" {
		t.Fatal("首次签发必须返回明文")
	}

	if err := db.DB.Model(&model.App{}).Where("id = ?", app.ID).
		Update("status", model.AppStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}

	// ① 已持有 + rotate=false：仍可读元数据，且不返回明文。
	rec = doAPI(t, r, http.MethodPost, "/api/user/apps/freeze-app/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("应用停用后已持有者仍应可读，实际 %d %s", rec.Code, rec.Body.String())
	}
	if p, _ := dataOf(t, rec)["plaintext"].(string); p != "" {
		t.Fatalf("重复调用不得返回明文，实际 %q", p)
	}

	// ② 已持有 + rotate=true：仍可轮换 —— 这是"不停服"的关键。
	rec = doAPI(t, r, http.MethodPost, "/api/user/apps/freeze-app/hosted-key", token,
		map[string]any{"rotate": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("应用停用后已持有者仍应可轮换，实际 %d %s", rec.Code, rec.Body.String())
	}
	if p, _ := dataOf(t, rec)["plaintext"].(string); p == "" || p == firstPlain {
		t.Fatal("轮换必须返回新的明文")
	}
}

// TestHostedKeyAppDisabledBlocksFirstIssue 断言停用应用阻断**首次接入**，
// 且带 rotate=true 也绕不过去。
//
// 回归 A7：旧实现只在 rotate=false 时检查开关，于是任意登录用户传
// {"rotate":true} 就能凭空首次签发。
func TestHostedKeyAppDisabledBlocksFirstIssue(t *testing.T) {
	r, token := setupUserAPI(t)
	app := seedApp(t, "blocked-app")
	if err := db.DB.Model(&model.App{}).Where("id = ?", app.ID).
		Update("status", model.AppStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}

	for _, body := range []map[string]any{{}, {"rotate": true}} {
		rec := doAPI(t, r, http.MethodPost, "/api/user/apps/blocked-app/hosted-key", token, body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("停用应用首次接入应 404（body=%v），实际 %d %s", body, rec.Code, rec.Body.String())
		}
		if env := decodeEnv(t, rec); env.ErrorCode != "app_not_found" {
			t.Fatalf("错误码应为 app_not_found，实际 %q", env.ErrorCode)
		}
	}
}

// ---- 自举轮换（接入方无人值守换钥） ----

// TestHostedKeySelfRotate 覆盖闭环：用当前钥换新钥 → 新钥可用 + 旧钥窗口内仍可用
// → 10 分钟内重复被拒。
func TestHostedKeySelfRotate(t *testing.T) {
	r, token := setupUserAPI(t)
	seedApp(t, "self-app")

	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/self-app/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("签发应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	oldPlain, _ := dataOf(t, rec)["plaintext"].(string)
	if oldPlain == "" {
		t.Fatal("签发必须返回明文")
	}

	// ① 自举轮换：只带托管密钥本身，不需要会话票。
	rec = doAPI(t, r, http.MethodPost, "/api/user/key/rotate", oldPlain, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("自举轮换应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	data := dataOf(t, rec)
	newPlain, _ := data["plaintext"].(string)
	if newPlain == "" || newPlain == oldPlain {
		t.Fatalf("必须返回新的明文，实际 %q", newPlain)
	}
	keyObj, _ := data["key"].(map[string]any)
	if prev, ok := keyObj["previous_expires_at"]; !ok || prev == nil {
		t.Fatal("默认窗口下必须给出上一代终点（否则接入方无法排期替换）")
	}

	// ② 新明文立即可用。
	if rec := doAPI(t, r, http.MethodGet, "/api/user/self", newPlain, nil); rec.Code != http.StatusOK {
		t.Fatalf("新明文应立即可用，实际 %d %s", rec.Code, rec.Body.String())
	}
	// ③ 旧明文在窗口内仍可用 —— 零中断的关键。
	if rec := doAPI(t, r, http.MethodGet, "/api/user/self", oldPlain, nil); rec.Code != http.StatusOK {
		t.Fatalf("窗口内旧明文应仍可用，实际 %d %s", rec.Code, rec.Body.String())
	}

	// ④ 10 分钟内再换 → 409（否则重启风暴会把刚发的钥挤掉）。
	rec = doAPI(t, r, http.MethodPost, "/api/user/key/rotate", newPlain, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("10 分钟内重复轮换应 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "key_rotate_busy" {
		t.Fatalf("错误码应为 key_rotate_busy，实际 %q", env.ErrorCode)
	}
}

// TestHostedKeySelfRotateRejectsWrongCredential 断言自举端点只认托管密钥：
// 会话票 401、自建 KEY 403（会话票无法表达"操作哪一把钥"）。
func TestHostedKeySelfRotateRejectsWrongCredential(t *testing.T) {
	r, token := setupUserAPI(t)

	// 会话票：HostedKeyAuth 只认 API KEY 形态。
	rec := doAPI(t, r, http.MethodPost, "/api/user/key/rotate", token, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("会话票应 401，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 自建 KEY：形态合法但不是托管密钥 → 403 scope_denied。
	krec := doAPI(t, r, http.MethodPost, "/api/user/keys", token, map[string]any{"name": "自建"})
	if krec.Code != http.StatusOK {
		t.Fatalf("建自建 KEY 应 200，实际 %d %s", krec.Code, krec.Body.String())
	}
	selfPlain, _ := dataOf(t, krec)["plaintext"].(string)
	if selfPlain == "" {
		t.Fatal("建 KEY 必须返回明文")
	}
	rec = doAPI(t, r, http.MethodPost, "/api/user/key/rotate", selfPlain, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("自建 KEY 应 403，实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "scope_denied" {
		t.Fatalf("错误码应为 scope_denied，实际 %q", env.ErrorCode)
	}
}

// TestHostedKeyHealth 断言自检端点给出"还剩多久"与"是否在窗口期"，
// 且**不**依赖应用本机时钟、**不**含恒真的 healthy 布尔。
func TestHostedKeyHealth(t *testing.T) {
	r, token := setupUserAPI(t)
	seedApp(t, "health-app")
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/health-app/hosted-key", token, map[string]any{})
	plain, _ := dataOf(t, rec)["plaintext"].(string)
	if plain == "" {
		t.Fatal("签发必须返回明文")
	}

	rec = doAPI(t, r, http.MethodGet, "/api/user/key/health", plain, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("自检应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	data := dataOf(t, rec)
	if _, ok := data["server_time"]; !ok {
		t.Fatal("必须返回 server_time")
	}
	if _, ok := data["healthy"]; ok {
		t.Fatal("不应有 healthy 字段：能调通就说明已通过鉴权，该字段恒为真")
	}
	keyObj, _ := data["key"].(map[string]any)
	secs, ok := keyObj["expires_in_seconds"].(float64)
	if !ok || secs <= 0 {
		t.Fatalf("必须给出正的剩余秒数，实际 %v", keyObj["expires_in_seconds"])
	}
	// 刚签发没有任何上一代。
	if v, ok := keyObj["previous_expires_in_seconds"]; !ok || v != nil {
		t.Fatalf("未轮换时窗口剩余应为 null，实际 %v", v)
	}

	// 轮换后：窗口剩余应出现且为正。
	rec = doAPI(t, r, http.MethodPost, "/api/user/key/rotate", plain, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("自举轮换应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	newPlain, _ := dataOf(t, rec)["plaintext"].(string)
	if newPlain == "" {
		t.Fatal("轮换必须返回新明文")
	}
	rec = doAPI(t, r, http.MethodGet, "/api/user/key/health", newPlain, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("轮换后自检应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	keyObj, _ = dataOf(t, rec)["key"].(map[string]any)
	prevSecs, ok := keyObj["previous_expires_in_seconds"].(float64)
	if !ok || prevSecs <= 0 {
		t.Fatalf("轮换后窗口剩余应为正，实际 %v", keyObj["previous_expires_in_seconds"])
	}
}

// TestHostedKeyBlockedRotateReturns409 断言：对一把被平台封禁的托管密钥发起
// 轮换，返回 409 key_rotate_blocked，**不是 401**。
//
// 为什么这不是"码好听一点"的问题：调用方（门户）带的是会话 Cookie，而前端把
// 任何非 invalid_credential 的 401 当成"你的会话失效"（清空本地态 + 守卫裁决）。
// 用 401 表达"目标钥被封"，表现就是用户点一次轮换被踢回登录页 —— 故障现象与
// 真实掉线完全一样。409 才是对的：调用方凭证有效，冲突在**目标资源**的状态上。
func TestHostedKeyBlockedRotateReturns409(t *testing.T) {
	r, token := setupUserAPI(t)
	seedApp(t, "blocked-app")
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/blocked-app/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("签发应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	keyObj, _ := dataOf(t, rec)["key"].(map[string]any)
	keyID := uint64(keyObj["id"].(float64))
	if err := db.DB.Model(&model.APIKey{}).Where("id = ?", keyID).
		Updates(map[string]any{
			"status":          int16(2),
			"disabled_reason": model.KeyDisabledAdminBlock,
		}).Error; err != nil {
		t.Fatal(err)
	}

	rec = doAPI(t, r, http.MethodPost, "/api/user/apps/blocked-app/hosted-key", token,
		map[string]any{"rotate": true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("封禁钥轮换应 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "key_rotate_blocked" {
		t.Fatalf("错误码应为 key_rotate_blocked，实际 %q", env.ErrorCode)
	}
	// 会话必须仍然有效：同一张票继续读自己（这正是 409 与 401 的分界）。
	if rec := doAPI(t, r, http.MethodGet, "/api/user/self", token, nil); rec.Code != http.StatusOK {
		t.Fatalf("轮换失败不得影响会话，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestHostedKeyHealthReportsDisabled 断言自检能在"钥不可用"时回答原因。
//
// 这是自检端点存在的唯一理由：若它也要求"先能用"，status / disabled_reason
// 就永远是 1 / 空串（挂诊断的名义、拿不到诊断的信息）。
func TestHostedKeyHealthReportsDisabled(t *testing.T) {
	r, token := setupUserAPI(t)
	seedApp(t, "diag-app")
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/diag-app/hosted-key", token, map[string]any{})
	plain, _ := dataOf(t, rec)["plaintext"].(string)
	if plain == "" {
		t.Fatal("签发必须返回明文")
	}
	keyID := uint64(dataOf(t, rec)["key"].(map[string]any)["id"].(float64))
	if err := db.DB.Model(&model.APIKey{}).Where("id = ?", keyID).
		Updates(map[string]any{
			"status":          int16(2),
			"disabled_reason": model.KeyDisabledAdminBlock,
		}).Error; err != nil {
		t.Fatal(err)
	}

	rec = doAPI(t, r, http.MethodGet, "/api/user/key/health", plain, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("被禁的钥也必须能自检，实际 %d %s", rec.Code, rec.Body.String())
	}
	data := dataOf(t, rec)
	keyObj, _ := data["key"].(map[string]any)
	if v, _ := keyObj["status"].(float64); int(v) != 2 {
		t.Fatalf("自检必须如实给出 status=2，实际 %v", keyObj["status"])
	}
	if v, _ := keyObj["disabled_reason"].(string); v != model.KeyDisabledAdminBlock {
		t.Fatalf("自检必须给出封禁成因，实际 %v", keyObj["disabled_reason"])
	}
	acct, _ := data["account"].(map[string]any)
	if v, _ := acct["status"].(float64); int(v) != 1 {
		t.Fatalf("必须下发账号状态（它决定 403），实际 %v", data["account"])
	}
	// 但"能不能用来调模型"仍然是 401 —— 自检只解释原因，不放行任何数据面。
	if rec := doAPI(t, r, http.MethodGet, "/api/user/self", plain, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("被禁的钥不得因自检而可用，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestHostedKeyHealthNormalizesDeadWindow 断言窗口结束后 previous_* 归 null。
//
// prev_key_expires_at 不会在窗口结束时被清理（唯一写点是轮换），直接下发会让
// "非 null = 还在窗口期"这条承诺永久失真：字段挂着过去的时间、剩余秒数为负。
func TestHostedKeyHealthNormalizesDeadWindow(t *testing.T) {
	r, token := setupUserAPI(t)
	seedApp(t, "dead-window-app")
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/dead-window-app/hosted-key", token, map[string]any{})
	plain, _ := dataOf(t, rec)["plaintext"].(string)
	keyID := uint64(dataOf(t, rec)["key"].(map[string]any)["id"].(float64))
	past := time.Now().Add(-time.Hour)
	if err := db.DB.Model(&model.APIKey{}).Where("id = ?", keyID).
		Updates(map[string]any{
			"prev_key_hash":       "deadbeef",
			"prev_key_expires_at": past,
		}).Error; err != nil {
		t.Fatal(err)
	}

	rec = doAPI(t, r, http.MethodGet, "/api/user/key/health", plain, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("自检应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	keyObj, _ := dataOf(t, rec)["key"].(map[string]any)
	if v, ok := keyObj["previous_expires_at"]; !ok || v != nil {
		t.Fatalf("窗口已结束必须回 null，实际 %v", v)
	}
	if v, ok := keyObj["previous_expires_in_seconds"]; !ok || v != nil {
		t.Fatalf("窗口已结束剩余秒数必须回 null，实际 %v", v)
	}
}

// TestHostedKeyIssueRejectsLongName 断言备注名超长返回 400 而不是 500。
// 列宽 64，PG 会以 22001 拒绝；不校验就是把"你填错了"报成"服务端异常"。
func TestHostedKeyIssueRejectsLongName(t *testing.T) {
	r, token := setupUserAPI(t)
	seedApp(t, "long-name-app")
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/long-name-app/hosted-key", token,
		map[string]any{"name": strings.Repeat("长", 31)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超长备注名应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestHostedKeyIssueAcceptsChunkedBody 断言 chunked 请求体不会被当成空体。
//
// 旧实现用 `ContentLength > 0` 判断"有没有体"：chunked 下它是 -1，于是一个
// {"rotate":true} 被静默忽略，调用方拿到 plaintext="" 还以为"没有新明文"。
func TestHostedKeyIssueAcceptsChunkedBody(t *testing.T) {
	r, token := setupUserAPI(t)
	seedApp(t, "chunked-app")
	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/chunked-app/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("签发应 200，实际 %d %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost,
		"http://example.com/api/user/apps/chunked-app/hosted-key",
		bytes.NewBufferString(`{"rotate":true}`))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("chunked 轮换应 200，实际 %d %s", w.Code, w.Body.String())
	}
	if plain, _ := dataOf(t, w)["plaintext"].(string); plain == "" {
		t.Fatal("chunked 请求体必须被解析：rotate=true 应返回新明文")
	}
}

package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/router"
)

// 本文件守的是本轮「开放 API 补全 + 信任边界收口」的**行为**。
//
// 每一条都对应一个真实存在过的缺陷；不写这些用例的话，下一次有人"顺手简化"
// 就会把它们改回去，而全量测试仍然是绿的（历史教训：把 captcha.Required
// 改成恒真/恒假时，当时所有用例都通过）。

// TestCapabilitiesIsPublicAndSlim 能力探测必须公开可读，且**只**回准入字段。
//
// 把门户配置整包登记成接入契约是不行的：它同时下发品牌、公告、站点正文、
// 价格口径 —— 改一次门户文案就会让第三方以为接口变了。
func TestCapabilitiesIsPublicAndSlim(t *testing.T) {
	authFlowDB(t)
	authFlowRedis(t)
	setupTestCaptcha(t)
	r := router.New()

	rec := doAPI(t, r, http.MethodGet, "/api/auth/capabilities", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("能力探测应公开可读（200），实际 %d %s", rec.Code, rec.Body.String())
	}
	data := dataOf(t, rec)

	// 准入决策需要的字段一个都不能少。
	for _, k := range []string{
		"register_mode", "invite_required", "contact_verify", "contact_channels",
		"available_contact_channels",
		"captcha_register", "captcha_login", "captcha_reset", "login_fail_threshold",
		"email_enabled", "sms_enabled", "contact_bind_enabled",
		"custom_topup_enabled", "redeem_enabled", "min_topup", "usage_default_window_days",
	} {
		if _, ok := data[k]; !ok {
			t.Fatalf("能力探测缺少字段 %q", k)
		}
	}

	// 门户渲染专属内容一个都不能出现（出现即意味着第三方会依赖它）。
	for _, k := range []string{"site", "brand", "notice", "announcement", "pricing", "currency", "models"} {
		if _, ok := data[k]; ok {
			t.Fatalf("能力探测不该下发门户专属字段 %q", k)
		}
	}

	// 两个渠道数组都必须是数组（JSON 里不能是 null，否则客户端 forEach 直接炸）。
	// available_contact_channels 是「策略 ∩ 就绪」，注册/找回只该读它。
	for _, k := range []string{"contact_channels", "available_contact_channels"} {
		raw, err := json.Marshal(data[k])
		if err != nil {
			t.Fatalf("序列化 %s 失败: %v", k, err)
		}
		if string(raw) == "null" {
			t.Fatalf("%s 必须是数组，不能是 null", k)
		}
	}
}

// TestRegisterContactCodeHonorsChannelPolicy 注册**发码**必须受渠道策略约束。
//
// 缺陷原型：RegisterContactCode 只调 verify.Issue（只判"发不发得出去"），
// 不看 auth_register.contact_channels。于是运营在后台取消勾选短信之后，
// 这个端点仍然能真的发出短信 —— 既产生费用，也是一条可绕过的滥用通道。
//
// 判据不能只看 400 错误码：短信未就绪时 Issue 内部也报 bind_method_disabled，
// 两者同码。所以额外断言「没有留下发码冷却键」—— 冷却键是 Issue 的第一个
// 副作用，能区分「被策略挡在门外」与「进了 Issue 后被传输层拒绝」。
func TestRegisterContactCodeHonorsChannelPolicy(t *testing.T) {
	// 发码路由组挂了限流中间件：Redis 不可用时会在进 handler 前就 503，
	// 这时无法验证策略闸门。按本项目惯例显式 skip，不做假绿。
	authFlowRedis(t)
	setupTestCaptcha(t)
	t.Cleanup(seedAuthConfigDefaults)

	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	conf.System().Reload(map[string]json.RawMessage{
		"auth_register": raw(map[string]any{
			"mode": "open", "contact_verify": "off", "contact_channels": []string{"email"},
		}),
	})
	r := router.New()

	clearCoolKeys := func() []string {
		if db.RDB == nil {
			return nil
		}
		ctx := context.Background()
		keys, err := db.RDB.Keys(ctx, "vc:cool:register:*").Result()
		if err != nil || len(keys) == 0 {
			return nil
		}
		db.RDB.Del(ctx, keys...)
		return keys
	}
	clearCoolKeys()
	t.Cleanup(func() { clearCoolKeys() })

	// 该端点挂在公开写组（CSRFPublic）：只校验来源，不要求双提交令牌。
	// 必须带 Origin，否则请求会在中间件层就被 403 挡掉，测不到被测逻辑。
	rawBody, _ := json.Marshal(map[string]any{"channel": "sms", "target": "13800138000"})
	req := httptest.NewRequest(http.MethodPost,
		"http://example.com/api/portal/auth/register/contact-code", bytes.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("策略未允许的渠道发码应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if code := decodeEnv(t, rec).ErrorCode; code != "bind_method_disabled" {
		t.Fatalf("应返回 bind_method_disabled，实际 %q", code)
	}
	// db.RDB 为空说明该环境没起 Redis：此时无法验证副作用，显式跳过而不是当绿。
	if db.RDB == nil {
		t.Skip("未初始化 Redis，跳过「无冷却键」断言")
	}
	keys, err := db.RDB.Keys(context.Background(), "vc:cool:register:*").Result()
	if err != nil {
		t.Fatalf("读取冷却键失败: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("被策略拒绝的发码请求不该写冷却键（说明已经进了 verify.Issue），实际 %v", keys)
	}
}

// TestContactAndRedeemsRequireSession 换绑与兑换记录**只认会话票**。
//
// 托管密钥代表的是"某个应用"，它连"用户本人是否在场"都证明不了；
// 让它换绑等于把账号接管能力交给接入方。所以 API KEY 必须是 403
// session_required，而不是"悄悄当成用户操作"。
func TestContactAndRedeemsRequireSession(t *testing.T) {
	r, token := setupUserAPI(t)

	keyRec := doAPI(t, r, http.MethodPost, "/api/user/keys", token, map[string]any{"name": "app-key"})
	if keyRec.Code != http.StatusOK {
		t.Fatalf("建 KEY 失败: %d %s", keyRec.Code, keyRec.Body.String())
	}
	plain, _ := dataOf(t, keyRec)["plaintext"].(string)
	if plain == "" {
		t.Fatal("建 KEY 必须回明文")
	}

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/user/contact/code", map[string]any{"channel": "email", "target": "a@b.com"}},
		{http.MethodPost, "/api/user/contact/bind", map[string]any{"channel": "email", "target": "a@b.com", "code": "123456"}},
		{http.MethodGet, "/api/user/redeems", nil},
	}
	for _, c := range cases {
		rec := doAPI(t, r, c.method, c.path, plain, c.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 用 API KEY 应 403，实际 %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
		if env := decodeEnv(t, rec); env.ErrorCode != "session_required" {
			t.Fatalf("%s %s 错误码应为 session_required，实际 %q", c.method, c.path, env.ErrorCode)
		}
	}
}

// TestKeyGroupCannotBeSelfSet 分组是**账号**属性，密钥不能自填。
//
// 历史上这里直接落库，而 groupDiscount 用组名查折扣、groupMatches 用组名筛渠道，
// 默认种子又把 vip 配成 9 折 —— 任何登录用户 PUT 一次就能给自己打折并进 VIP 渠道池。
func TestKeyGroupCannotBeSelfSet(t *testing.T) {
	r, token := setupUserAPI(t)

	rec := doAPI(t, r, http.MethodPost, "/api/user/keys",
		token, map[string]any{"name": "sneaky", "group": "vip"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("自填 group 应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "invalid_param" {
		t.Fatalf("错误码应为 invalid_param，实际 %q", env.ErrorCode)
	}

	// 不传 group 时，视图里的 group 必须是**账号的实际分组**（与计费同口径），
	// 而不是空串 —— 空串会让前端显示一个计费侧并不存在的分组名。
	okRec := doAPI(t, r, http.MethodPost, "/api/user/keys", token, map[string]any{"name": "normal"})
	if okRec.Code != http.StatusOK {
		t.Fatalf("正常建 KEY 失败: %d %s", okRec.Code, okRec.Body.String())
	}
	key, _ := dataOf(t, okRec)["key"].(map[string]any)
	if key == nil {
		t.Fatalf("响应缺少 key: %s", okRec.Body.String())
	}
	if g, _ := key["group"].(string); g == "" {
		t.Fatal("密钥视图的 group 必须是生效分组（账号分组），不能是空串")
	}
}

// TestKeyLimitReached 单用户 KEY 数量上限：先刷几千把再拉列表是现成的放大器。
func TestKeyLimitReached(t *testing.T) {
	r, token := setupUserAPI(t)

	raw, err := json.Marshal(1)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	conf.System().Set("key_limit_per_user", raw)
	t.Cleanup(func() {
		// 复位成默认值：同进程内其它用例也会建 KEY。
		def, _ := json.Marshal(20)
		conf.System().Set("key_limit_per_user", def)
	})

	if rec := doAPI(t, r, http.MethodPost, "/api/user/keys", token, map[string]any{"name": "k1"}); rec.Code != http.StatusOK {
		t.Fatalf("第 1 把应成功，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec := doAPI(t, r, http.MethodPost, "/api/user/keys", token, map[string]any{"name": "k2"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("超过上限应 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "key_limit_reached" {
		t.Fatalf("错误码应为 key_limit_reached，实际 %q", env.ErrorCode)
	}
}

// TestHostedKeyScopesOrders 托管密钥只看**本应用**的订单。
//
// 订单的 app_id 就是"这笔充值归因给谁"。A 应用的密钥能读到用户在 B 应用的充值，
// 等于把接入方的经营数据摊给同行。
func TestHostedKeyScopesOrders(t *testing.T) {
	r, token := setupUserAPI(t)

	appA := seedApp(t, "app-a")
	appB := seedApp(t, "app-b")

	rec := doAPI(t, r, http.MethodPost, "/api/user/apps/"+appA.Slug+"/hosted-key", token, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("签发 %s 托管密钥失败: %d %s", appA.Slug, rec.Code, rec.Body.String())
	}
	keyA, _ := dataOf(t, rec)["plaintext"].(string)
	if keyA == "" {
		t.Fatalf("首签必须回明文: %s", rec.Body.String())
	}

	var user model.User
	if err := db.DB.Where("username = ?", "apiuser").First(&user).Error; err != nil {
		t.Fatalf("读取测试用户失败: %v", err)
	}
	now := time.Now()
	rows := []model.Order{
		{UserID: user.ID, AppID: appA.ID, OutTradeNo: "HOSTED-A", AmountCent: 100, Currency: "CNY", Status: "pending", CreatedAt: now},
		{UserID: user.ID, AppID: appB.ID, OutTradeNo: "HOSTED-B", AmountCent: 200, Currency: "CNY", Status: "pending", CreatedAt: now},
	}
	if err := db.DB.Create(&rows).Error; err != nil {
		t.Fatalf("插入订单失败: %v", err)
	}

	listRec := doAPI(t, r, http.MethodGet, "/api/user/orders", keyA, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("订单列表失败: %d %s", listRec.Code, listRec.Body.String())
	}
	var payload struct {
		Items []struct {
			OutTradeNo string `json:"out_trade_no"`
		} `json:"items"`
	}
	if err := json.Unmarshal(decodeEnv(t, listRec).Data, &payload); err != nil {
		t.Fatalf("解析订单列表失败: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("托管密钥只应看到本应用的 1 笔订单，实际 %d 笔", len(payload.Items))
	}
	if payload.Items[0].OutTradeNo != "HOSTED-A" {
		t.Fatalf("看到的订单是 %q，应为 HOSTED-A（跨应用泄漏）", payload.Items[0].OutTradeNo)
	}
}

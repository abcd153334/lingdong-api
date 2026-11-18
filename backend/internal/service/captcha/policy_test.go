package captcha

import (
	"encoding/json"
	"testing"

	"lingdong-api/internal/conf"
)

// setPolicy 写入 auth_captcha / auth_login 两个键（这是 Required 仅有的两个输入）。
func setPolicy(t *testing.T, login string, failThreshold int) {
	t.Helper()
	captchaRaw, err := json.Marshal(map[string]any{
		"scenes": map[string]any{"login": login},
	})
	if err != nil {
		t.Fatalf("构造 auth_captcha 失败: %v", err)
	}
	loginRaw, err := json.Marshal(map[string]any{"fail_threshold": failThreshold})
	if err != nil {
		t.Fatalf("构造 auth_login 失败: %v", err)
	}
	conf.System().Set("auth_captcha", captchaRaw)
	conf.System().Set("auth_login", loginRaw)
	// 不在这里注册 cleanup：cleanup 里再调 setPolicy 会无限递归注册，
	// 表现是"用例跑 50 秒然后超时"。每个用例开头都会重新写这两个键，无需复位。
}

// 注册与找回恒要求验证：这是代码常量，不读配置。
// 就算有人把 scenes 里塞了 register=off，也不能生效。
func TestRegisterAndResetAlwaysRequired(t *testing.T) {
	setPolicy(t, "off", 5)

	raw, _ := json.Marshal(map[string]any{
		"scenes": map[string]any{"register": "off", "password_reset": "off", "login": "off"},
	})
	conf.System().Set("auth_captcha", raw)

	if !Required(SceneRegister, 0) {
		t.Fatal("注册场景必须恒要求验证")
	}
	if !Required(ScenePasswordReset, 0) {
		t.Fatal("找回密码场景必须恒要求验证")
	}
}

func TestLoginOff(t *testing.T) {
	setPolicy(t, "off", 5)
	if Required(SceneLogin, 0) {
		t.Fatal("off 时不该要求验证")
	}
	// off 时即使失败 99 次也不要求 —— auth.go 的失败分支据此返回 invalid_credential
	// 而不是 captcha_required（否则前端永不亮图，用户被卡死）。
	if Required(SceneLogin, 99) {
		t.Fatal("off 时不管失败多少次都不该要求验证")
	}
}

func TestLoginAlways(t *testing.T) {
	setPolicy(t, "always", 5)
	if !Required(SceneLogin, 0) {
		t.Fatal("always 时首屏就该要求验证")
	}
}

// after_fails 的阈值只有一个来源：auth_login.fail_threshold。
func TestLoginAfterFailsUsesLoginThreshold(t *testing.T) {
	setPolicy(t, "after_fails", 5)

	if Required(SceneLogin, 4) {
		t.Fatal("失败 4 次（< 阈值 5）不该要求验证")
	}
	if !Required(SceneLogin, 5) {
		t.Fatal("失败 5 次（= 阈值）必须要求验证")
	}
	// 阈值改成 3 后，第 3 次就要 —— 证明读的是 auth_login 而不是别的键。
	setPolicy(t, "after_fails", 3)
	if Required(SceneLogin, 2) {
		t.Fatal("阈值 3 时，失败 2 次不该要求验证")
	}
	if !Required(SceneLogin, 3) {
		t.Fatal("阈值 3 时，失败 3 次必须要求验证")
	}
}

// 未知场景按"要验证"处理：新增入口忘了登记时多要一次，而不是静默放开。
func TestUnknownSceneRequires(t *testing.T) {
	setPolicy(t, "off", 5)
	if !Required("brand_new_scene", 0) {
		t.Fatal("未知场景应默认要求验证")
	}
}

package conf

import "testing"

func TestNodeModeNormalize(t *testing.T) {
	cases := map[string]string{
		"":        NodeModeFull,
		"full":    NodeModeFull,
		"FULL":    NodeModeFull,
		" full ":  NodeModeFull,
		"api":     NodeModeAPI,
		" API \n": NodeModeAPI,
		// 拼错保留原值，交给 ValidateNodeMode 拒绝启动；不静默回落 full。
		"ap":   "ap",
		"only": "only",
	}
	for in, want := range cases {
		if got := nodeMode(in); got != want {
			t.Errorf("nodeMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateNodeMode(t *testing.T) {
	if err := ValidateNodeMode(Env{NodeMode: NodeModeFull}); err != nil {
		t.Errorf("full 应合法: %v", err)
	}
	if err := ValidateNodeMode(Env{NodeMode: NodeModeAPI}); err != nil {
		t.Errorf("api 应合法: %v", err)
	}
	if err := ValidateNodeMode(Env{NodeMode: "ap"}); err == nil {
		t.Error("未知模式必须报错（静默按 full 跑会让运维以为前端已关闭）")
	}
}

func TestIsAPIMode(t *testing.T) {
	if !(Env{NodeMode: NodeModeAPI}).IsAPIMode() {
		t.Error("api 模式判定失败")
	}
	if (Env{NodeMode: NodeModeFull}).IsAPIMode() {
		t.Error("full 模式被误判为 api")
	}
}

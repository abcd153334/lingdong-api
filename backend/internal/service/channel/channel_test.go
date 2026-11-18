package channel

import (
	"context"
	"errors"
	"testing"
	"time"

	"lingdong-api/internal/pkg/channelkeys"
)

// TestApplyTestResultSuccess 校验成功时 success_count++ 且清空 last_error。
func TestApplyTestResultSuccess(t *testing.T) {
	now := time.Unix(1700000000, 0)
	h := applyTestResult(Health{}, nil, now)
	if h.SuccessCount != 1 || h.FailCount != 0 {
		t.Fatalf("计数不符: %+v", h)
	}
	if h.LastTestAt == nil || !h.LastTestAt.Equal(now) {
		t.Fatalf("last_test_at 未刷新: %+v", h.LastTestAt)
	}
	if h.LastError != "" || h.FailRate != 0 {
		t.Fatalf("成功时不应残留错误/失败率: %+v", h)
	}
}

// TestApplyTestResultFailure 校验失败时 fail_count++ 且记录原因与失败率。
func TestApplyTestResultFailure(t *testing.T) {
	h := applyTestResult(Health{SuccessCount: 3}, errors.New("上游返回 401"), time.Unix(1700000000, 0))
	if h.FailCount != 1 || h.SuccessCount != 3 {
		t.Fatalf("计数不符: %+v", h)
	}
	if h.LastError != "上游返回 401" {
		t.Fatalf("last_error 不符: %s", h.LastError)
	}
	if h.FailRate != 0.25 {
		t.Fatalf("失败率应为 1/4=0.25，实际 %v", h.FailRate)
	}
}

// TestScanEntries 校验调度模式决定密钥尝试顺序。
func TestScanEntries(t *testing.T) {
	entries := []channelkeys.Entry{
		{Index: 0, Plain: "k1", ID: "1"},
		{Index: 1, Plain: "k2", ID: "2"},
		{Index: 2, Plain: "k3", ID: "3"},
	}
	// channelID=0 时 keyhealth 直接返回空（Redis 读被跳过），等价于「没有冷却信息」，
	// 此时顺序必须与池序一致 —— 这是"降级不改变行为"的守卫。
	if got := scanEntries(context.Background(), 0, entries, ModePolling); len(got) != 3 || got[0].Plain != "k1" {
		t.Fatalf("polling 应保持顺序: %v", got)
	}
	got := scanEntries(context.Background(), 0, entries, ModeRandom)
	if len(got) != 3 {
		t.Fatalf("random 应返回同长度池: %v", got)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.Plain] = true
	}
	for _, e := range entries {
		if !seen[e.Plain] {
			t.Fatalf("random 打乱不得丢失或新增密钥: %v", got)
		}
	}
}

// TestValidate 校验渠道入参规则（对齐原型表单必填项）。
func TestValidate(t *testing.T) {
	base := func() Req {
		return Req{Name: "渠道A", AdapterID: "openai", BaseURL: "https://api.openai.com",
			MultiKeyMode: ModeRandom, Groups: []string{"default"}, Weight: 5, Priority: 1, Keys: []string{"k"}}
	}

	r := base()
	r.normalize()
	if err := r.validate(true); err != nil {
		t.Fatalf("合法入参被拒: %v", err)
	}

	cases := map[string]func(*Req){
		"空名称":        func(r *Req) { r.Name = "" },
		"非法适配器":      func(r *Req) { r.AdapterID = "Bad Adapter!" },
		"非法 baseurl": func(r *Req) { r.BaseURL = "api.openai.com" },
		"非法调度模式":     func(r *Req) { r.MultiKeyMode = "hash" },
		"空分组":        func(r *Req) { r.Groups = nil },
		"负权重":        func(r *Req) { r.Weight = -1 },
		"零优先级":       func(r *Req) { r.Priority = 0 },
		"新建空密钥":      func(r *Req) { r.Keys = nil },
	}
	for name, mutate := range cases {
		r := base()
		mutate(&r)
		r.normalize()
		if err := r.validate(true); err == nil {
			t.Fatalf("用例 %q 应被拒绝", name)
		}
	}
}

// TestValidateUpdateAllowsEmptyKeys 校验编辑时允许不提交密钥池。
func TestValidateUpdateAllowsEmptyKeys(t *testing.T) {
	r := Req{Name: "渠道A", AdapterID: "openai", BaseURL: "https://x.com",
		MultiKeyMode: ModePolling, Groups: []string{"default"}, Weight: 0, Priority: 1}
	r.normalize()
	if err := r.validate(false); err != nil {
		t.Fatalf("编辑空密钥应被允许: %v", err)
	}
}

// TestAdapterIDPattern 校验适配器标识：内置裸名与 spec:<slug> 两种形态。
func TestAdapterIDPattern(t *testing.T) {
	valid := []string{"openai", "duomiapi", "autodl", "metaso", "spec:kling-v2", "spec:a1"}
	for _, v := range valid {
		if !adapterIDPattern.MatchString(v) {
			t.Fatalf("%q 应合法", v)
		}
	}
	// builtin:/plugin: 命名空间从未有注册项匹配（插件系统已下线），必须被拒。
	invalid := []string{"", "OpenAI", "builtin:doubao", "plugin:my-relay", "builtin:", "spec:", "spec:UPPER", "a b"}
	for _, v := range invalid {
		if adapterIDPattern.MatchString(v) {
			t.Fatalf("%q 应非法", v)
		}
	}
}

// TestNormalize 校验默认值与清洗（去空白、去空密钥）。
func TestNormalize(t *testing.T) {
	r := Req{Name: " 渠道 ", AdapterID: " openai ", BaseURL: " https://x.com ",
		Groups: []string{" default ", ""}, Keys: []string{" k1 ", "", " k2 "}}
	r.normalize()
	if r.Name != "渠道" || r.AdapterID != "openai" || r.BaseURL != "https://x.com" {
		t.Fatalf("空白未清洗: %+v", r)
	}
	if r.MultiKeyMode != ModeRandom {
		t.Fatalf("调度模式默认值错误: %s", r.MultiKeyMode)
	}
	if len(r.Groups) != 2 || len(r.Keys) != 2 {
		t.Fatalf("空项未剔除: groups=%v keys=%v", r.Groups, r.Keys)
	}
}

// TestDecodeHealth 校验空/非法 health 快照回落为零值。
func TestDecodeHealth(t *testing.T) {
	if h := decodeHealth(nil); h.SuccessCount != 0 || h.FailRate != 0 {
		t.Fatalf("空快照应回零值: %+v", h)
	}
}

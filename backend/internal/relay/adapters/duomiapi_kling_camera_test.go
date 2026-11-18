package adapters

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"lingdong-api/internal/relay"
)

// TestDuomiKlingCamera 锁死可灵运镜的三条硬约束。
//
// 这三条不是"尽量校验"，而是**上游一定会拒**的输入：报错信息里完全不提「运镜」，
// 只会说参数不合法，于是整条请求被归因成渠道故障 —— 一条健康渠道会被熔断，
// 而真实原因（用户同时给了 zoom 和 pan）永远查不到。
func TestDuomiKlingCamera(t *testing.T) {
	cases := []struct {
		name string
		in   *duomiCameraControl
		// wantNil = 不下发（没写或不该下发）
		wantNil bool
		wantErr string
	}{
		{name: "没写", in: nil, wantNil: true},
		{name: "单轴 zoom", in: &duomiCameraControl{Zoom: 5}},
		{name: "单轴反向 horizontal", in: &duomiCameraControl{Horizontal: -10}},
		{name: "上界 10", in: &duomiCameraControl{Tilt: 10}},
		{name: "下界 -10", in: &duomiCameraControl{Roll: -10}},
		{name: "六轴全 0 非法", in: &duomiCameraControl{}, wantErr: "至少要有一个方向非零"},
		{
			name:    "两个方向同时给",
			in:      &duomiCameraControl{Zoom: 3, Pan: -2},
			wantErr: "一次只能指定一个方向",
		},
		{
			// 越界必须拒绝而不是截断：截断后 15 会变成 10，用户以为按最大力度运镜，
			// 实际是他没要的一个值。
			name:    "越界 10.5",
			in:      &duomiCameraControl{Zoom: 10.5},
			wantErr: "必须在 -10 ~ 10 之间",
		},
		{
			name:    "越界 -11",
			in:      &duomiCameraControl{Vertical: -11},
			wantErr: "必须在 -10 ~ 10 之间",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := duomiKlingCamera(c.in)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("应报错，实际通过：%+v", got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("错误文案应包含 %q，实际 %v", c.wantErr, err)
				}
				// 必须是「调用方参数错」（400），不能是上游错误（503）：
				// 前者用户能自己改，后者会让运维去查一条没问题的渠道。
				var cp *relay.CallerParamError
				if !errors.As(err, &cp) {
					t.Fatalf("应为 CallerParamError，实际 %T", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if c.wantNil {
				if got != nil {
					t.Fatalf("不该下发，实际 %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("应下发运镜参数")
			}
			if got.Type != duomiKlingCameraTypeSimple {
				t.Fatalf("出站 type 必须是 %q，实际 %q", duomiKlingCameraTypeSimple, got.Type)
			}
			// 出站必须是六键齐全（缺键会被上游判非法），且值不得被改动。
			out := got.Config
			if out == nil {
				t.Fatal("出站缺少 config")
			}
			if out.Horizontal != c.in.Horizontal || out.Vertical != c.in.Vertical ||
				out.Pan != c.in.Pan || out.Tilt != c.in.Tilt ||
				out.Roll != c.in.Roll || out.Zoom != c.in.Zoom {
				t.Fatalf("六轴取值被改动：in=%+v out=%+v", c.in, out)
			}
		})
	}
}

// TestDuomiKlingCameraOutboundShape 锁死**出站 JSON 形状**。
//
// 可灵契约要 `{"type":"simple","config":{六轴}}`：两层都是必填。
// 六轴平铺（早期实现）会让上游回「参数不合法」而 ERROR 里没有「运镜」二字，
// 于是一条健康渠道会被判故障熔断 —— 这类错误单看适配器代码永远发现不了，
// 只有把出站字节钉住才能防回归。
func TestDuomiKlingCameraOutboundShape(t *testing.T) {
	got, err := duomiKlingCamera(&duomiCameraControl{Zoom: 5})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	b, merr := json.Marshal(got)
	if merr != nil {
		t.Fatalf("序列化失败：%v", merr)
	}
	var probe struct {
		Type   *string                    `json:"type"`
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if probe.Type == nil || *probe.Type != "simple" {
		t.Fatalf("顶层必须有 type=simple，实际 %s", b)
	}
	if probe.Config == nil {
		t.Fatalf("顶层必须有 config，实际 %s", b)
	}
	for _, k := range []string{"horizontal", "vertical", "pan", "tilt", "roll", "zoom"} {
		if _, ok := probe.Config[k]; !ok {
			t.Fatalf("config 缺轴 %q（上游按固定六键解析）：%s", k, b)
		}
	}
	if string(probe.Config["zoom"]) != "5" {
		t.Fatalf("zoom 取值不符：%s", b)
	}
}

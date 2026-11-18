package storage

import (
	"encoding/json"
	"errors"
	"testing"

	"lingdong-api/internal/conf"
)

// TestActiveRejectsBrokenConfig 选中「未知后端 / 配置残缺 / 格式非法」时返回 ErrUnavailable，
// 且**不静默回落**到本地存储。
//
// 静默回落更危险：本地写成功、配置页显示 oss，运维以为对象进了云，实际全在容器磁盘上，
// 一次重建就全丢。所以这里逐项断言「返回 nil Store」。
func TestActiveRejectsBrokenConfig(t *testing.T) {
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })

	cases := []struct{ name, raw string }{
		{"未知后端", `{"active":"unknown-backend"}`},
		{"oss 缺通道配置", `{"active":"oss","channels":{}}`},
		{"oss 通道缺密钥", `{"active":"oss","channels":{"oss":{"endpoint":"https://oss-cn-hangzhou.aliyuncs.com","bucket":"b"}}}`},
		{"oss endpoint 指向环回", `{"active":"oss","channels":{"oss":{"endpoint":"http://127.0.0.1:9000","bucket":"b","access_key_id":"ak","access_key_secret":"sk"}}}`},
		{"cos 缺通道配置", `{"active":"cos","channels":{}}`},
		{"s3 缺密钥", `{"active":"s3","channels":{"s3":{"endpoint":"https://s3.example.com","bucket":"b"}}}`},
		{"配置不是对象", `[]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conf.System().Set("storage", json.RawMessage(c.raw))
			st, err := Active()
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("应返回 ErrUnavailable，实际 %v", err)
			}
			if st != nil {
				t.Fatalf("不得静默回落（应返回 nil Store），实际 %q", st.Name())
			}
		})
	}
}

// TestActiveRequiresConfigured 未配置 storage 时**拒绝写入**，不回落本地磁盘。
//
// 这条判据是本次整改的安全闸：配置丢失/新库没跑 seed/启动加载失败时，
// 静默写本地会让对象落在容器层 —— 管理台一切正常，一次重建全部消失。
// 显式 `active:"none"` 才是「我要用本地磁盘」，它是配置，不是缺省。
func TestActiveRequiresConfigured(t *testing.T) {
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })

	cases := []struct{ name, raw string }{
		{"缺 storage 键", ``},
		{"空对象", `{}`},
		{"active 为空串", `{"active":"","channels":{"none":{}}}`},
		{"显式 null", `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conf.System().Reload(map[string]json.RawMessage{})
			if c.raw != "" {
				conf.System().Set("storage", json.RawMessage(c.raw))
			}
			st, err := Active()
			if !errors.Is(err, ErrNotConfigured) {
				t.Fatalf("应判 ErrNotConfigured，实际 %v", err)
			}
			// 对外是 503（ErrUnavailable 必须同时成立），运维才知道是"写不了"而非"参数错"。
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("应同时归 ErrUnavailable，实际 %v", err)
			}
			if st != nil {
				t.Fatalf("不得回落本地磁盘，实际 %q", st.Name())
			}
		})
	}

	// 显式选本地：这是唯一写本地的方式，必须照常可用。
	conf.System().Set("storage", json.RawMessage(`{"active":"none","channels":{"none":{}}}`))
	st, err := Active()
	if err != nil {
		t.Fatalf("显式 none 应可用，实际 %v", err)
	}
	if st.Name() != "none" {
		t.Fatalf("应构建 none，实际 %q", st.Name())
	}
}

// TestForRequiresConfigForCloud 记录指向云后端但配置整份丢失时，读路径判「后端已移除」。
//
// 不能判「回落到本地试试」：那会把配置事故伪装成「文件被删」，
// 用户会去重传、重跑任务，而对象其实还在桶里。
func TestForRequiresConfigForCloud(t *testing.T) {
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })

	conf.System().Reload(map[string]json.RawMessage{})
	if _, err := For("oss"); !errors.Is(err, ErrBackendGone) {
		t.Fatalf("缺配置时云后端应判 ErrBackendGone，实际 %v", err)
	}
	// 本地后端不依赖配置：配置坏掉时历史本地对象仍然可读。
	if st, err := For("none"); err != nil || st.Name() != "none" {
		t.Fatalf("none 不应依赖配置: st=%v err=%v", st, err)
	}
}

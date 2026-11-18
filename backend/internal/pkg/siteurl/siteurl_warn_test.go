package siteurl

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/conf"
)

// 本文件覆盖 P1 缓解：本地/测试环境 + 公网 API 地址的错配预警。
//
// 场景：本地 dev 在管理台临时填生产域名排障后忘改，/f/ 直链 origin 指向
// 生产域名，上游回取素材必然失败而日志无任何线索。判定只认配置
// （ConfiguredBase），未配置时回落请求 Host、本地反而正常——不提示。

// setSiteBase 用单条 API 地址覆盖内存配置（空串表示未配置）。
func setSiteBase(t *testing.T, base string) {
	t.Helper()
	if base == "" {
		setSite(t, map[string]any{})
		return
	}
	setSite(t, map[string]any{"api_base_urls": []string{base}})
}

// setSite 是 site 子树的唯一注入点：一处构造，避免每个用例各拼一份 JSON。
func setSite(t *testing.T, site map[string]any) {
	t.Helper()
	b, err := json.Marshal(site)
	if err != nil {
		t.Fatalf("构造 site 配置失败：%v", err)
	}
	conf.System().Reload(map[string]json.RawMessage{"site": json.RawMessage(b)})
	t.Cleanup(func() { conf.System().Reload(map[string]json.RawMessage{}) })
}

func TestPublicBaseMisfitWarning(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		base   string
		warned bool
	}{
		{"dev+公网域名", "dev", "https://api.example.com", true},
		{"test+公网域名", "test", "https://api.example.com", true},
		{"dev+localhost", "dev", "http://localhost:8080", false},
		{"dev+回环", "dev", "http://127.0.0.1:8080", false},
		{"dev+内网地址", "dev", "http://192.168.1.10:8080", false},
		{"dev+未配置", "dev", "", false},
		{"生产环境不提示", "production", "https://api.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setSiteBase(t, tc.base)
			got := PublicBaseMisfitWarning(tc.env)
			if tc.warned && got == "" {
				t.Fatalf("%s：应提示", tc.name)
			}
			if !tc.warned && got != "" {
				t.Fatalf("%s：不应提示，实际 %q", tc.name, got)
			}
			// 提示必须指向**当前**的配置位置，否则运维照着旧键名找不到
			if tc.warned && !strings.Contains(got, "API 调用地址") {
				t.Errorf("提示必须指名配置位置：%q", got)
			}
		})
	}
}

package storage

import (
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"lingdong-api/internal/conf"
)

// TestBuildersCoverAllSchemes 锁死「代码能构建哪些后端」这一份清单。
//
// 增删驱动必须同时改 builders 与配置页的 storageSchemes；这张用例的作用是让
// 「新增了驱动但忘了登记」与「悄悄删掉一个驱动」都变成显式改动而不是静默漂移。
func TestBuildersCoverAllSchemes(t *testing.T) {
	want := []string{"cos", "minio", "none", "oss", "s3"}
	got := make([]string, 0, len(builders))
	for k := range builders {
		got = append(got, k)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("后端集合不符：want %v，got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("后端集合不符：want %v，got %v", want, got)
		}
	}
}

// TestValidateChannelAcceptsAllSchemes 每个登记方案的「字段填全」配置都必须能构建。
//
// 这是配置保存期的判据（systemconfig.validateStorage 直接调它），也是
// 「配置页能选」与「运行期真能构建」保持一致的唯一保证：只要有一项在这里构建失败，
// 运营保存成功之后每一次产物转存都会失败，而配置页不会有任何提示。
func TestValidateChannelAcceptsAllSchemes(t *testing.T) {
	cases := []struct {
		backend string
		ch      map[string]interface{}
	}{
		{"none", nil},
		{"s3", map[string]interface{}{
			"endpoint": "https://s3.example.com", "bucket": "lingdong",
			"access_key": "ak", "secret_key": "sk",
		}},
		{"minio", map[string]interface{}{
			"endpoint": "https://minio.example.com", "bucket": "lingdong",
			"access_key": "ak", "secret_key": "sk", "path_style": "true",
		}},
		{"oss", map[string]interface{}{
			"endpoint": "oss-cn-hangzhou.aliyuncs.com", "bucket": "lingdong",
			"access_key_id": "ak", "access_key_secret": "sk",
		}},
		{"cos", map[string]interface{}{
			"region": "ap-guangzhou", "bucket": "lingdong-1250000000",
			"secret_id": "id", "secret_key": "sk",
		}},
	}
	for _, c := range cases {
		t.Run(c.backend, func(t *testing.T) {
			if err := ValidateChannel(c.backend, c.ch); err != nil {
				t.Fatalf("%s 应可构建，实际: %v", c.backend, err)
			}
		})
	}
}

// TestValidateChannelRejectsBadEndpoint endpoint 是运营可配置的半可信输入，
// 指向环回/私网时必须在保存期就被拒，而不是等到第一次写对象才超时。
func TestValidateChannelRejectsBadEndpoint(t *testing.T) {
	err := ValidateChannel("oss", map[string]interface{}{
		"endpoint": "http://127.0.0.1:9000", "bucket": "b",
		"access_key_id": "ak", "access_key_secret": "sk",
	})
	if err == nil {
		t.Fatal("环回 endpoint 应被拒绝")
	}
}

// TestForRejectsMissingBackend 记录里没有后端标识时不猜、不回落到本地。
//
// 「先试本地再试当前后端」的懒回填会把 bug 静默成"正确"：写路径漏记后端，
// 读路径却总能在本地找到文件，于是没人发现切换后端之后这批记录全丢。
func TestForRejectsMissingBackend(t *testing.T) {
	if _, err := For(""); !errors.Is(err, ErrBackendMissing) {
		t.Fatalf("空后端应返回 ErrBackendMissing，实际 %v", err)
	}
}

// TestForRejectsUnknownBackend 记录指向一个代码里不存在的后端 → ErrBackendGone。
func TestForRejectsUnknownBackend(t *testing.T) {
	if _, err := For("gcs"); !errors.Is(err, ErrBackendGone) {
		t.Fatalf("未知后端应返回 ErrBackendGone，实际 %v", err)
	}
}

// TestForLocalIgnoresStorageConfig 本地后端不读 storage 配置：
// 配置写坏了，历史本地对象仍然要能读出来（否则改坏一次配置等于全站产物 404）。
func TestForLocalIgnoresStorageConfig(t *testing.T) {
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })

	conf.System().Set("storage", json.RawMessage(`[]`))
	st, err := For("none")
	if err != nil {
		t.Fatalf("本地后端不应依赖 storage 配置，实际 %v", err)
	}
	if st.Name() != "none" {
		t.Fatalf("应构建出本地后端，实际 %q", st.Name())
	}
}

// TestUsableBackendsIncludesLocal 清理任务靠它过滤候选行，本地后端必须恒在其中。
func TestUsableBackendsIncludesLocal(t *testing.T) {
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })

	conf.System().Set("storage", json.RawMessage(`{"active":"none","channels":{}}`))
	got := UsableBackends()
	if len(got) == 0 || got[0] != "none" {
		t.Fatalf("可用后端应至少含 none 且排在首位，实际 %v", got)
	}
	// 没有通道配置的云后端不可构建，不能出现在结果里 —— 否则清理任务会选中
	// 一批「后端不存在」的行，逐条跳过、每轮重复选中，把队列饿死。
	for _, name := range got {
		if name != "none" {
			t.Fatalf("未配置通道的 %s 不应被视为可用，实际 %v", name, got)
		}
	}
}

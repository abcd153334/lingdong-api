package storage

import (
	"encoding/json"
	"testing"

	"lingdong-api/internal/conf"
)

// bucketOf 从配置里取某后端的桶（本地返回空串）。
func bucketOf(t *testing.T, cfg, backend string) string {
	t.Helper()
	var parsed struct {
		Channels map[string]map[string]interface{} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}
	if v, ok := parsed.Channels[backend]["bucket"].(string); ok {
		return v
	}
	return ""
}

// TestPublicURLByBackend 把「记录后端 → 对外地址」这一条规则钉死。
//
// 这是本次整改的唯一地址真相源：三个业务出口（上传件、任务产物、参考图）都调
// storage.PublicURL，任何一处拼错都会让用户拿到一个 404 的地址，而库里一切正常。
func TestPublicURLByBackend(t *testing.T) {
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })

	cases := []struct {
		name    string
		cfg     string
		backend string
		key     string
		want    string
	}{
		{
			name:    "s3 path-style",
			cfg:     `{"active":"s3","channels":{"s3":{"endpoint":"https://s3.example.com","bucket":"ld-bucket","access_key":"ak","secret_key":"sk","path_style":"true"}}}`,
			backend: "s3",
			key:     "files/1/x.png",
			want:    "https://s3.example.com/ld-bucket/files/1/x.png",
		},
		{
			name:    "s3 virtual-host",
			cfg:     `{"active":"s3","channels":{"s3":{"endpoint":"https://s3.example.com","bucket":"ld-bucket","access_key":"ak","secret_key":"sk","path_style":"false"}}}`,
			backend: "s3",
			key:     "files/1/x.png",
			want:    "https://ld-bucket.s3.example.com/files/1/x.png",
		},
		{
			// 生产实际形态：endpoint 是 region 域名，桶走子域。
			name:    "oss 标准 endpoint",
			cfg:     `{"active":"oss","channels":{"oss":{"endpoint":"https://oss-cn-shanghai.aliyuncs.com","bucket":"ling-ai","access_key_id":"ak","access_key_secret":"sk"}}}`,
			backend: "oss",
			key:     "files/1/x.png",
			want:    "https://ling-ai.oss-cn-shanghai.aliyuncs.com/files/1/x.png",
		},
		{
			// 运营填裸域名（不带 scheme）也必须拼对。
			name:    "oss 裸 endpoint",
			cfg:     `{"active":"oss","channels":{"oss":{"endpoint":"oss-cn-shanghai.aliyuncs.com","bucket":"ling-ai","access_key_id":"ak","access_key_secret":"sk"}}}`,
			backend: "oss",
			key:     "artifacts/1/35/0",
			want:    "https://ling-ai.oss-cn-shanghai.aliyuncs.com/artifacts/1/35/0",
		},
		{
			name:    "cos 默认域名",
			cfg:     `{"active":"cos","channels":{"cos":{"region":"ap-shanghai","bucket":"ld-bucket","secret_id":"id","secret_key":"sk"}}}`,
			backend: "cos",
			key:     "icons/01X.png",
			want:    "https://ld-bucket.cos.ap-shanghai.myqcloud.com/icons/01X.png",
		},
		{
			name:    "cos 自定义域名",
			cfg:     `{"active":"cos","channels":{"cos":{"region":"ap-shanghai","bucket":"ld-bucket","secret_id":"id","secret_key":"sk","endpoint":"https://cdn.example.com"}}}`,
			backend: "cos",
			key:     "a/b c.png",
			want:    "https://cdn.example.com/a/b%20c.png",
		},
		{
			// 本地后端没有对外地址：调用方必须回落平台域名，而不是拿到一个空串。
			name:    "本地无地址",
			cfg:     `{"active":"none","channels":{"none":{}}}`,
			backend: "none",
			key:     "files/1/x.png",
			want:    "",
		},
		{
			name:    "空 key 不生成地址",
			cfg:     `{"active":"oss","channels":{"oss":{"endpoint":"oss-cn-shanghai.aliyuncs.com","bucket":"ling-ai","access_key_id":"ak","access_key_secret":"sk"}}}`,
			backend: "oss",
			key:     "",
			want:    "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conf.System().Set("storage", json.RawMessage(c.cfg))
			// 记录里的桶与配置里的桶是两件事：这里取配置里的那个，等价于「对象写在当前桶」。
			// 换桶场景由 TestForRefRoutesByRecordBucket 覆盖。
			got, ok := PublicURL(Ref{Backend: c.backend, Bucket: bucketOf(t, c.cfg, c.backend), Key: c.key})
			if c.want == "" {
				if ok || got != "" {
					t.Fatalf("应返回无地址，实际 %q ok=%v", got, ok)
				}
				return
			}
			if !ok {
				t.Fatalf("应返回地址，实际 ok=false")
			}
			if got != c.want {
				t.Fatalf("地址不符\n got=%s\nwant=%s", got, c.want)
			}
		})
	}
}

// TestPublicURLBackendGone 记录指向已不存在的后端时不给地址（读路径本身会 503）。
func TestPublicURLBackendGone(t *testing.T) {
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })

	conf.System().Set("storage", json.RawMessage(`{"active":"none","channels":{"none":{}}}`))
	if u, ok := PublicURL(Ref{Backend: "s3", Key: "files/1/x.png"}); ok || u != "" {
		t.Fatalf("后端已消失时应返回无地址，实际 %q", u)
	}
}

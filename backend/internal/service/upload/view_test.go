package upload

import (
	"encoding/json"
	"testing"
	"time"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
)

// TestViewURLFollowsRecordBackend 钉死「对外地址由记录的后端派生」。
//
// 这是用户实际尖叫的那个点：配置了云存储，上传接口却返回平台域名，
// 于是所有人（包括运维）都以为文件落在本地磁盘。地址是位置的证据，
// 不能让「桶里有对象、地址像本地」这种组合继续存在。
func TestViewURLFollowsRecordBackend(t *testing.T) {
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })
	conf.System().Set("storage", json.RawMessage(`{"active":"oss","channels":{
		"none":{},
		"oss":{"endpoint":"oss-cn-shanghai.aliyuncs.com","bucket":"ling-ai",
		       "access_key_id":"ak","access_key_secret":"sk"}}}`))

	const base = "https://api.example.com"
	expires := time.Now().Add(time.Hour)

	cases := []struct {
		name    string
		backend string
		bucket  string
		key     string
		want    string
	}{
		{
			name: "云后端给对象存储地址", backend: "oss", bucket: "ling-ai",
			key:  "files/1/01J8/猫.png",
			want: "https://ling-ai.oss-cn-shanghai.aliyuncs.com/files/1/01J8/%E7%8C%AB.png",
		},
		{
			name: "本地后端给平台直链", backend: "none",
			key:  "files/1/01J8/cat.png",
			want: base + "/f/01J8ZQ7W6Y5X4V3T2S1R0Q9P8N/cat.png",
		},
		{
			// 记录指向一个已从配置里移除的后端：回落平台地址（读路径会 503，
			// 语义一致），不能返回空串让前端渲染成裂图。
			name: "后端已移除回落平台地址", backend: "s3", bucket: "ld",
			key:  "files/1/01J8/cat.png",
			want: base + "/f/01J8ZQ7W6Y5X4V3T2S1R0Q9P8N/cat.png",
		},
		{
			// 历史空 backend：直接拒绝派生，回落平台地址。
			name: "空后端回落平台地址", backend: "",
			key:  "files/1/01J8/cat.png",
			want: base + "/f/01J8ZQ7W6Y5X4V3T2S1R0Q9P8N/cat.png",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := View(&model.Upload{
				ID: "01J8ZQ7W6Y5X4V3T2S1R0Q9P8N", Filename: "cat.png", SafeName: "cat.png",
				SizeBytes: 10, ContentType: "image/png",
				StorageBackend: c.backend, StorageBucket: c.bucket, StorageKey: c.key,
				ExpiresAt: expires,
			}, "", base)
			if got.URL != c.want {
				t.Fatalf("地址不符\n got=%s\nwant=%s", got.URL, c.want)
			}
		})
	}
}

// TestStorageKeyCarriesSafeName 守住「下载名来自 URL 末段」这条前提。
//
// 云端地址是跨域直链，`<a download>` 会被浏览器忽略，对象存储上的
// Content-Disposition 又需要签名参数 —— 文件名只能来自 key 的最后一段。
func TestStorageKeyCarriesSafeName(t *testing.T) {
	cases := []struct{ name, safe, want string }{
		{"普通文件名", "report.pdf", "files/7/01J8/report.pdf"},
		{"空名兜底", "", "files/7/01J8/file"},
		// ".." 会被对象存储/网关折叠成上级路径：必须换成固定段，
		// 否则「写进去的 key」≠「库里记的 key」。
		{"上级目录", "..", "files/7/01J8/file"},
		{"当前目录", ".", "files/7/01J8/file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := storageKey(7, "01J8", c.safe); got != c.want {
				t.Fatalf("key 不符: got=%s want=%s", got, c.want)
			}
		})
	}
	// 超长名字必须截断，否则 key 会超过列宽（storage_key size:512）。
	long := make([]rune, 200)
	for i := range long {
		long[i] = 'a'
	}
	if got := storageKey(7, "01J8", string(long)); len(got) > len("files/7/01J8/")+80 {
		t.Fatalf("超长名未截断: %d", len(got))
	}
}

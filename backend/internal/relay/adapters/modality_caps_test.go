package adapters

import (
	"testing"

	"lingdong-api/internal/relay"
)

// TestAdapterSatisfiesModalityUsesCaps 把「声明优先于端点探针」这条规则钉死。
//
// 回归的是真实缺陷：多米API 的图像 / 音乐是**异步任务**，它的 SupportsEndpoint
// 全 false（不承接任何同步端点，否则同步网关会把 chat 请求派给它）。
// 改造前 AdapterSatisfiesModality 只看端点探针，于是 image 模态被判成「不支持」——
// 模型建得出来、绑定却 400，而同一个模型由迁移直接写库却能正常出图。
//
// 这个用例必须放在 adapters 包里：适配器注册靠本包的 init，
// relay 包的测试二进制里一个适配器都没有（import 循环）。
func TestAdapterSatisfiesModalityUsesCaps(t *testing.T) {
	cases := []struct {
		adapter  string
		modality string
		want     bool
		why      string
	}{
		{"duomiapi", "image", true, "Caps 声明了 image（异步图像任务，没有同步端点）"},
		{"duomiapi", "video", true, "Caps 声明了 video"},
		{"duomiapi", "music", true, "Caps 声明了 music（决策 H 的第 8 类模态）"},
		{"duomiapi", "embedding", false, "多米不做 embedding，Caps 与探针都不命中"},
		{"duomiapi", "audio", false, "SUNO 是音乐不是 TTS，不能吃掉 /v1/audio/speech"},
		{"autodl", "video", true, "AutoDL 只有视频"},
		{"autodl", "image", false, "AutoDL 不做图像"},
		{"metaso", "video", true, "秘塔只有视频"},
		{"metaso", "image", false, "秘塔不做图像"},
		{"openai", "chat", true, "同步 chat 端点 + Caps text 双命中"},
		{"openai", "image", true, "同步 images/generations"},
		{"nosuchadapter", "image", false, "未注册适配器一律不满足"},
	}
	for _, c := range cases {
		if got := relay.AdapterSatisfiesModality(c.adapter, c.modality); got != c.want {
			t.Fatalf("%s × %s = %v，期望 %v（%s）", c.adapter, c.modality, got, c.want, c.why)
		}
	}
}

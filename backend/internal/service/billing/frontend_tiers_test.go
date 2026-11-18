package billing

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestFrontendTierKeysMatchBackend 守住前端兜底档位与后端白名单的一致性。
//
// 为什么需要它：`web/src/lib/specTiers.ts` 的数组只在「目录接口失败」时生效，
// 平时完全用不到 —— 于是它落后于后端也不会有人发现，直到某天目录挂了：
// 图片缺 3K、视频缺 4k，运营在表单里**根本看不到那一档的输入行**，
// 结论会变成「这个档位不支持」，而不是「前端漏了一个值」。
//
// 判据只能来自本文件的 ImageTierKeys / VideoTierKeys：手写期望值等于又造一份真相源。
func TestFrontendTierKeysMatchBackend(t *testing.T) {
	raw, err := os.ReadFile("../../../../web/src/lib/specTiers.ts")
	if err != nil {
		t.Fatalf("读不到前端兜底档位文件（路径变了要同步改这里）：%v", err)
	}
	src := string(raw)
	cases := []struct {
		name string
		want []string
	}{
		{"FALLBACK_IMAGE_TIERS", ImageTierKeys},
		{"FALLBACK_VIDEO_TIERS", VideoTierKeys},
	}
	for _, c := range cases {
		got := tsStringArray(t, src, c.name)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Fatalf("%s 与后端白名单不一致：\n前端 = %v\n后端 = %v", c.name, got, c.want)
		}
	}
}

// tsStringArray 从 `export const NAME = ["a", "b"];` 里取出字符串数组。
func tsStringArray(t *testing.T, src, name string) []string {
	t.Helper()
	re := regexp.MustCompile(name + `\s*=\s*\[([^\]]*)\]`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("在 specTiers.ts 里找不到 %s（改写法要同步改本测试）", name)
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		v := strings.Trim(strings.TrimSpace(part), `"'`)
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s 解析为空：数组写法变了？", name)
	}
	return out
}

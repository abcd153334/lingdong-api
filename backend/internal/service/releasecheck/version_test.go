package releasecheck

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"v2.1.0", Version{2, 1, 0}, true},
		{"2.1.0", Version{2, 1, 0}, true},
		{"v2.2.0-rc1", Version{2, 2, 0}, true}, // 后缀不参与排序
		{"v2.2.0+build.7", Version{2, 2, 0}, true},
		{" v2.2.0 ", Version{2, 2, 0}, true},
		{"dev", Version{}, false},
		{"local", Version{}, false},
		{"v2.1", Version{}, false},
		{"v2.1.0.1", Version{}, false},
		{"v2.1.0; rm -rf /", Version{}, false},
		{"v1234567890.0.0", Version{}, false}, // 10 位数字：拒绝，别让它溢出
		{"", Version{}, false},
	}
	for _, c := range cases {
		got, ok := ParseVersion(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("ParseVersion(%q) = %+v,%v；期望 %+v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v2.2.0", "v2.1.0", 1, true},
		{"v2.1.0", "v2.2.0", -1, true},
		{"v2.1.0", "2.1.0", 0, true},
		{"v2.1.0", "v2.1.0-rc1", 0, true}, // 同核心：不算更新
		{"v10.0.0", "v9.9.9", 1, true},    // 数值比较，不是字符串比较
		{"dev", "v2.1.0", 0, false},
		{"v2.1.0", "local", 0, false},
	}
	for _, c := range cases {
		got, ok := Compare(c.a, c.b)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("Compare(%q,%q) = %d,%v；期望 %d,%v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

// TestIsNewerNotComparable 守住"判断不了"与"没有更新"必须是两件事。
//
// 本地构建的 dev 版本、或清单里写了非版本串时，若把二者混为一谈，
// 管理台就会显示"已是最新"——那是**编造结论**。
func TestIsNewerNotComparable(t *testing.T) {
	if newer, comparable := IsNewer("v2.2.0", "dev"); comparable || newer {
		t.Fatalf("dev 当前版本必须判为不可比较，实际 newer=%v comparable=%v", newer, comparable)
	}
	if newer, comparable := IsNewer("v2.2.0", "v2.1.0"); !comparable || !newer {
		t.Fatal("v2.2.0 > v2.1.0 应判为有更新")
	}
	if newer, comparable := IsNewer("v2.1.0", "v2.1.0"); !comparable || newer {
		t.Fatal("同版本不应判为有更新")
	}
}

func TestValidTagRejectsShellMeta(t *testing.T) {
	for _, bad := range []string{"v2.1.0;ls", "v2.1.0&&rm", "$(id)", "../../etc", "v2.1.0/../../x"} {
		if ValidTag(bad) {
			t.Fatalf("危险 tag 必须被拒：%q", bad)
		}
	}
	if !ValidTag("v2.1.0") {
		t.Fatal("正常版本 tag 应通过")
	}
}

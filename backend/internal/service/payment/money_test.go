package payment

import "testing"

// TestParseCNYToCent 覆盖合法写法与所有会被 ParseFloat 误接受的写法。
func TestParseCNYToCent(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"0.01", 1, true},
		{"0.1", 10, true},
		{"72", 7200, true},
		{"72.00", 7200, true},
		{"72.5", 7250, true},
		{"00012.30", 1230, true}, // 前导零合法
		{"999999999.99", 99999999999, true},
		{" 72 ", 7200, true},     // 两端空白被裁掉
		{"", 0, false},           // 空串
		{"-1", 0, false},         // 负数
		{"+72", 0, false},        // 正号
		{"1.234", 0, false},      // 三位小数
		{"1e3", 0, false},        // 科学计数法
		{"NaN", 0, false},        // ParseFloat 能接受
		{"Inf", 0, false},        //
		{"1.2.3", 0, false},      //
		{"1,000", 0, false},      // 千分位
		{"1234567890", 0, false}, // 10 位整数超限
		{"0.001", 0, false},      // 亚分精度
		{"￥72", 0, false},        // 带货币符号
	}
	for _, c := range cases {
		got, err := ParseCNYToCent(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("ParseCNYToCent(%q) 不应报错: %v", c.in, err)
			} else if got != c.want {
				t.Errorf("ParseCNYToCent(%q)=%d want %d", c.in, got, c.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseCNYToCent(%q) 应被拒绝，实际 %d", c.in, got)
		}
	}
}

// TestFormatCentToCNY 格式化不得经 float（0.1+0.2 之类的误差不能出现在金额字符串里）。
func TestFormatCentToCNY(t *testing.T) {
	cases := map[int64]string{
		0:           "0.00",
		1:           "0.01",
		10:          "0.10",
		100:         "1.00",
		7200:        "72.00",
		99999999999: "999999999.99",
		-1:          "-0.01",
		-7200:       "-72.00",
	}
	for in, want := range cases {
		if got := FormatCentToCNY(in); got != want {
			t.Errorf("FormatCentToCNY(%d)=%q want %q", in, got, want)
		}
	}
}

// TestAmountRoundTrip 解析与格式化必须互为逆运算（这是「下单金额 = 回调校验金额」的前提）。
func TestAmountRoundTrip(t *testing.T) {
	for _, s := range []string{"0.01", "0.10", "1.00", "72.00", "999999999.99"} {
		cent, err := ParseCNYToCent(s)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", s, err)
		}
		if got := FormatCentToCNY(cent); got != s {
			t.Fatalf("往返不一致: %q → %d → %q", s, cent, got)
		}
	}
}

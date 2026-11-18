package money

import (
	"errors"
	"math"
	"testing"
)

func TestParse(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"0.00", 0},
		{"1", 1_000_000},
		{"1.", 1_000_000},
		{"2.50", 2_500_000},
		{" 12.34 ", 12_340_000},
		{"0.000001", 1},
		{"0.0000001" /* 7 位 */, -1}, // 见 rejected
		{"000123.45", 123_450_000},
		{"2000000000", MaxAmount},
		{"100.00", 100_000_000},
	}
	for _, c := range ok {
		if c.want < 0 {
			continue
		}
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) 不应报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q)=%d want %d", c.in, got, c.want)
		}
	}

	bad := []string{
		"", " ", "-1", "+1", "1e3", "NaN", "Inf", "1.2.3", ".5",
		"1,000", "1.2345678", "2000000001", "abc", "--1", "1..2", "0x10",
	}
	for _, s := range bad {
		if got, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) 应被拒绝，实际 %d", s, got)
		} else if !errors.Is(err, ErrInvalidAmount) && !errors.Is(err, ErrAmountTooLarge) {
			t.Errorf("Parse(%q) 错误类型不对: %v", s, err)
		}
	}
}

func TestParseSigned(t *testing.T) {
	if v, err := ParseSigned("-2.50"); err != nil || v != -2_500_000 {
		t.Fatalf("ParseSigned(-2.50)=%d,%v", v, err)
	}
	if _, err := ParseSigned("-"); err == nil {
		t.Fatal("ParseSigned(\"-\") 应被拒绝")
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		m    int64
		dp   int
		want string
	}{
		{0, 2, "0.00"},
		{2_500_000, 2, "2.50"},
		{2_500_000, 4, "2.5000"},
		{1, 6, "0.000001"},
		{-2_500_000, 2, "-2.50"},
		{999_999, 2, "1.00"},  // 0.999999 → 1.00（四舍五入）
		{5_000, 2, "0.01"},    // 0.005 → 0.01（远离零）
		{4_999, 2, "0.00"},    // 0.004999 → 0.00
		{12_340_000, 0, "12"}, // dp=0
		{15_000_000, 0, "15"},
		{1_000_000, 2, "1.00"},
		{100, 2, "0.00"},
	}
	for _, c := range cases {
		if got := Format(c.m, c.dp); got != c.want {
			t.Errorf("Format(%d,%d)=%q want %q", c.m, c.dp, got, c.want)
		}
	}
}

// TestRoundTrip 覆盖「填价 → 落库 → 回显」的逐值相等（方案 §3.1-1）。
func TestRoundTrip(t *testing.T) {
	for _, s := range []string{"0", "0.01", "2.50", "10.00", "1234.5678", "0.000001"} {
		v, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		dp := 2
		if len(s) > 0 && s != "0" && s != "0.01" && s != "2.50" && s != "10.00" {
			dp = 6
		}
		back, err := Parse(Format(v, dp))
		if err != nil {
			t.Fatalf("回读 %q: %v", s, err)
		}
		if dp == 6 && back != v {
			t.Errorf("6 位往返失真: %q → %d → %q → %d", s, v, Format(v, dp), back)
		}
	}
}

func TestMulDiv(t *testing.T) {
	cases := []struct {
		a, b, c, want int64
	}{
		{1_000_000, 900_000, 1_000_000, 900_000}, // 9 折
		{100, 3, 2, 150},
		{5_000_000, 1_500_000, 1_000_000, 7_500_000}, // 视频超时 1.5 倍
		{-1_000_000, 900_000, 1_000_000, -900_000},
		{0, 900_000, 1_000_000, 0},
		{1, 1, 2, 1}, // 0.5 → 1（远离零）
		{-1, 1, 2, -1},
		{1, 1, 3, 0}, // 0.333 → 0
		{2, 1, 3, 1}, // 0.667 → 1
	}
	for _, c := range cases {
		got, err := MulDiv(c.a, c.b, c.c)
		if err != nil {
			t.Errorf("MulDiv(%d,%d,%d) 报错: %v", c.a, c.b, c.c, err)
			continue
		}
		if got != c.want {
			t.Errorf("MulDiv(%d,%d,%d)=%d want %d", c.a, c.b, c.c, got, c.want)
		}
	}

	// 溢出必须报错，绝不静默回绕（旧 frozen*3 的事故形态）。
	if _, err := MulDiv(math.MaxInt64, 3, 1); !errors.Is(err, ErrAmountTooLarge) {
		t.Errorf("MulDiv 溢出应报 ErrAmountTooLarge，实际 %v", err)
	}
	if _, err := MulDiv(1, 1, 0); !errors.Is(err, ErrDivideByZero) {
		t.Errorf("除零应报 ErrDivideByZero，实际 %v", err)
	}
	if _, err := MulDiv(math.MinInt64, 1, 1); !errors.Is(err, ErrAmountTooLarge) {
		t.Errorf("MinInt64 取反应报错，实际 %v", err)
	}
}

func TestCents(t *testing.T) {
	if c, err := CentsOf(1_000_000); err != nil || c != 100 {
		t.Fatalf("CentsOf(1e6)=%d,%v", c, err)
	}
	if _, err := CentsOf(15_000); !errors.Is(err, ErrPrecision) {
		t.Errorf("非整分应报 ErrPrecision，实际 %v", err)
	}
	if v, err := FromCents(123); err != nil || v != 1_230_000 {
		t.Fatalf("FromCents(123)=%d,%v", v, err)
	}
	if _, err := FromCents(MaxAmount); !errors.Is(err, ErrAmountTooLarge) {
		t.Errorf("超界应报 ErrAmountTooLarge，实际 %v", err)
	}
}

func TestSum(t *testing.T) {
	if v, err := Sum(100_000_000, 20_000_000); err != nil || v != 120_000_000 {
		t.Fatalf("Sum=%d,%v", v, err)
	}
	if _, err := Sum(MaxAmount, 1); err == nil {
		t.Error("Sum 越过 MaxAmount 应报错")
	}
}

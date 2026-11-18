package payment

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// cnyPattern 只接受「元」的正规写法：1–9 位整数 + 最多 2 位小数。
//
// 刻意不接受 `1e3` / `NaN` / `+72` / `1.2.3` / 千分位逗号：
// strconv.ParseFloat 全都接受，但它们不是用户能填出的合法金额，
// 接受它们只会把「解析成功但语义错误」的问题推到计费环节才爆。
var cnyPattern = regexp.MustCompile(`^\d{1,9}(\.\d{1,2})?$`)

// ParseCNYToCent 把「元」字符串转成「分」整数，**全程不经 float**。
//
// 为什么不用 ParseFloat：`0.1*100` 在二进制浮点下不等于 10，
// 用 `int64(f*100)` 取整会得到 9 或 10（取决于具体值），
// 而支付金额差 1 分就是账务错误。字符串拆分是唯一无误差的做法。
func ParseCNYToCent(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if !cnyPattern.MatchString(s) {
		return 0, fmt.Errorf("%w：%q", ErrInvalidAmount, s)
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	yuan, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w：%q", ErrInvalidAmount, s)
	}
	var cents int64
	switch len(fracPart) {
	case 1:
		d, _ := strconv.ParseInt(fracPart, 10, 64)
		cents = d * 10
	case 2:
		cents, _ = strconv.ParseInt(fracPart, 10, 64)
	}
	return yuan*100 + cents, nil
}

// FormatCentToCNY 把「分」格式化成「元」字符串（两位小数，不经 float）。
//
// 负数也会输出成 -0.01 这样的形式（理论上订单金额不会为负，
// 但退款/对账日志会用到，不能因为负数就 panic 或输出乱码）。
func FormatCentToCNY(cent int64) string {
	if cent < 0 {
		return "-" + FormatCentToCNY(-cent)
	}
	return fmt.Sprintf("%d.%02d", cent/100, cent%100)
}

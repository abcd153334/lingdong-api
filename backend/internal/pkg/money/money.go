// Package money 是全系统唯一的金额算术实现。
//
// 三条基石（见 docs/计费与结算口径.md）：
//  1. 一个单位：int64「微」，1 结算货币单位 = 1_000_000 微。Scale 是常量，不可配。
//  2. 一个币种：本包**不认识币种**，只认识数值。币种只影响展示符号与对外文案。
//  3. 价格 = 金额：模型价目直接存「每个计费单位多少微」，不存在倍率/刻度这类中间量。
//
// 为什么不用 float64：0.1+0.2≠0.3 这类误差在金额上是账务事故，而不是显示问题。
// 全包只在 Parse/Format 两个边界接触十进制字符串，内部全程整数。
//
// 换算只允许发生在三个边界：
//
//	HTTP 入参 字符串 → 微   （Parse）
//	HTTP 出参 微 → 字符串   （Format）
//	支付渠道 微 → 分        （CentsOf / FromCents，必须整除）
//
// 其余任何地方出现 /100、*100、/1e6 与金额混用都算缺陷。
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"
)

const (
	// Scale 是 1 个结算货币单位包含的微数。不可配：刻度这个概念已被删除。
	Scale int64 = 1_000_000
	// CentsPerUnit 是 1 个货币单位的分（支付渠道的最小单位）。
	CentsPerUnit int64 = 100
	// MicroPerCent 是 1 分包含的微数。
	MicroPerCent int64 = 10_000
	// MaxAmount 是单笔金额的绝对上界（2e15 微 = 20 亿货币单位）。
	//
	// 它同时是 int64 的安全边界：任何「金额 × 单价」的中间乘积在此之前就会先被
	// Check/MulDiv 拒掉，不会走到 9.2e18 的回绕区。
	MaxAmount int64 = 2_000_000_000_000_000
	// MaxDecimals 是微能表达的最大小数位。
	MaxDecimals = 6
)

var (
	// ErrInvalidAmount 表示字符串不是合法金额（空、负号、科学计数、超过 6 位小数…）。
	ErrInvalidAmount = errors.New("money: 金额格式非法")
	// ErrAmountTooLarge 表示金额越过 MaxAmount 或算术溢出。
	ErrAmountTooLarge = errors.New("money: 金额超出范围")
	// ErrPrecision 表示达不到目标精度（例如「微 → 分」时除不尽）。
	ErrPrecision = errors.New("money: 精度不足")
	// ErrDivideByZero 表示除数为 0。
	ErrDivideByZero = errors.New("money: 除数为 0")
)

var pow10 = [...]int64{
	1, 10, 100, 1_000, 10_000, 100_000, 1_000_000,
}

func pow10Of(n int) int64 {
	if n < 0 || n >= len(pow10) {
		return 0
	}
	return pow10[n]
}

// Parse 把十进制金额字符串解析为微。
//
// 严格规则（刻意不接受 ParseFloat 能接受的一切）：
//   - 只允许 ASCII 数字与至多一个小数点，不接受正负号、指数、千分位、空格之外任何字符；
//   - 整数部分 1~15 位，小数部分 0~6 位（"1." 合法 = 1，".5" 非法）；
//   - 结果必须 ≤ MaxAmount。
//
// 金额一律非负：负数只可能由系统内部运算产生（退款差额），不会从 HTTP 入参进来。
func Parse(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrInvalidAmount
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if hasDot && fracPart == "" {
		// "1." 视为 "1"（用户手输的常见形态），但 "." 本身非法。
		if intPart == "" {
			return 0, ErrInvalidAmount
		}
	}
	if intPart == "" || len(intPart) > 15 {
		return 0, ErrInvalidAmount
	}
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
		}
	}
	if len(fracPart) > MaxDecimals {
		return 0, fmt.Errorf("%w: %q 最多 %d 位小数", ErrInvalidAmount, s, MaxDecimals)
	}
	for i := 0; i < len(fracPart); i++ {
		if fracPart[i] < '0' || fracPart[i] > '9' {
			return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	// 先判上界再乘，避免 15 位 × 1e6 越过 int64。
	if whole > MaxAmount/Scale {
		return 0, fmt.Errorf("%w: %q", ErrAmountTooLarge, s)
	}
	frac := int64(0)
	if len(fracPart) > 0 {
		padded := fracPart + strings.Repeat("0", MaxDecimals-len(fracPart))
		frac, err = strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
		}
	}
	v := whole*Scale + frac
	if v > MaxAmount {
		return 0, fmt.Errorf("%w: %q", ErrAmountTooLarge, s)
	}
	return v, nil
}

// ParseSigned 同 Parse，但接受前导负号。仅用于「调账 / 退款差额」这类内部入口，
// 业务金额（价格、充值、余额）一律走 Parse。
func ParseSigned(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "-") {
		v, err := Parse(strings.TrimPrefix(s, "-"))
		if err != nil {
			return 0, err
		}
		return -v, nil
	}
	return Parse(s)
}

// MustParse 仅用于种子数据与测试：解析失败直接 panic（配置写错应当在启动时立刻暴露）。
func MustParse(s string) int64 {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// Format 把微格式化为 dp 位小数的字符串（四舍五入，远离零）。
//
// dp 取值 0..6。负数保留负号 —— 流水里有借方金额，丢符号会让对账读反方向。
func Format(m int64, dp int) string {
	if dp < 0 {
		dp = 0
	}
	if dp > MaxDecimals {
		dp = MaxDecimals
	}
	neg := m < 0
	if neg {
		if m == math.MinInt64 {
			m = math.MaxInt64
		} else {
			m = -m
		}
	}
	unit := pow10Of(MaxDecimals - dp)
	q := m / unit
	if rem := m % unit; rem >= unit-rem { // rem*2 >= unit，避免 rem*2 溢出
		q++
	}
	whole := q / pow10Of(dp)
	frac := q % pow10Of(dp)
	out := strconv.FormatInt(whole, 10)
	if dp > 0 {
		fs := strconv.FormatInt(frac, 10)
		if pad := dp - len(fs); pad > 0 {
			out += "." + strings.Repeat("0", pad) + fs
		} else {
			out += "." + fs
		}
	}
	if neg && q != 0 {
		return "-" + out
	}
	return out
}

// MulDiv 返回 round(a*b/c)，四舍五入远离零，溢出返回 ErrAmountTooLarge。
//
// 这是全包唯一做乘除的底层实现：用 128 位中间值，绝不「先乘再判溢出」。
// 旧实现靠 `if frozen*3 < 0 { 退回默认值 }` 兜底，结果是静默少收，比报错危险得多。
func MulDiv(a, b, c int64) (int64, error) {
	if c == 0 {
		return 0, ErrDivideByZero
	}
	neg := false
	for _, p := range []*int64{&a, &b, &c} {
		if *p < 0 {
			if *p == math.MinInt64 {
				return 0, ErrAmountTooLarge
			}
			*p = -*p
			neg = !neg
		}
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(c) {
		return 0, ErrAmountTooLarge
	}
	q, r := bits.Div64(hi, lo, uint64(c))
	if r >= uint64(c)-r { // 2r >= c
		if q == math.MaxUint64 {
			return 0, ErrAmountTooLarge
		}
		q++
	}
	if q > math.MaxInt64 {
		return 0, ErrAmountTooLarge
	}
	v := int64(q)
	if neg {
		v = -v
	}
	if v > MaxAmount || v < -MaxAmount {
		return 0, ErrAmountTooLarge
	}
	return v, nil
}

// RoundDiv 返回 round(a/b)，溢出返回错误。
func RoundDiv(a, b int64) (int64, error) { return MulDiv(a, 1, b) }

// Mul 返回 round(m*p/Scale)，即「按 p（微比例）打折」。
func Mul(m, p int64) (int64, error) { return MulDiv(m, p, Scale) }

// CentsOf 把微换算为分，除不尽返回 ErrPrecision。
//
// 支付渠道只认分：多出来的微无法被收，静默截断会让「订单金额」与「到账金额」
// 出现无法解释的差额，所以宁可报错要求调用方取整。
func CentsOf(m int64) (int64, error) {
	if m < 0 {
		return 0, ErrInvalidAmount
	}
	if m%MicroPerCent != 0 {
		return 0, fmt.Errorf("%w: %d 微不是整分", ErrPrecision, m)
	}
	return m / MicroPerCent, nil
}

// FromCents 把分换算为微。
func FromCents(c int64) (int64, error) {
	if c < 0 {
		return 0, ErrInvalidAmount
	}
	if c > MaxAmount/MicroPerCent {
		return 0, ErrAmountTooLarge
	}
	return c * MicroPerCent, nil
}

// Check 校验金额量级（绝对值 ≤ MaxAmount）。负数是否合法由调用方按场景判定。
func Check(v int64) error {
	if v > MaxAmount || v < -MaxAmount {
		return fmt.Errorf("%w: %d", ErrAmountTooLarge, v)
	}
	return nil
}

// Product 是一个「数量 × 单价」的计费项（token 数 × 每百万 token 单价）。
type Product struct {
	Qty  int64
	Unit int64
}

// SumProducts 返回 round(Σ Qty×Unit / div)，**只在最后舍入一次**。
//
// 为什么不能用 int64 逐项累加：Qty×Unit 的乘积空间远超 int64
// （1e9 tokens × 1e7 微/1M = 1e16，两项就接近 int64 上限），
// 而"先乘再判溢出"的老写法（frozen*3）正是静默少收的根因。
// 这里用 128 位大整数累加，越界只发生在最终结果，且必然被 Check 拦住。
func SumProducts(div int64, items []Product) (int64, error) {
	return sumProducts(div, items, false)
}

// SumProductsCeil 同 SumProducts，但向上取整（预扣估算用：多冻可退，少冻只能少收）。
func SumProductsCeil(div int64, items []Product) (int64, error) {
	return sumProducts(div, items, true)
}

func sumProducts(div int64, items []Product, ceil bool) (int64, error) {
	if div <= 0 {
		return 0, ErrDivideByZero
	}
	var acc, tmp big.Int
	for _, it := range items {
		if it.Qty == 0 || it.Unit == 0 {
			continue
		}
		q := it.Qty
		u := it.Unit
		neg := false
		if q < 0 {
			q, neg = -q, !neg
		}
		if u < 0 {
			u, neg = -u, !neg
		}
		tmp.SetInt64(q)
		tmp.Mul(&tmp, big.NewInt(u))
		if neg {
			tmp.Neg(&tmp)
		}
		acc.Add(&acc, &tmp)
	}
	d := big.NewInt(div)
	q, r := new(big.Int).QuoRem(&acc, d, new(big.Int))
	if r.Sign() != 0 {
		// 正数：余数 >= div/2 进位；向上取整时任何非零余数都进位。
		// 负数：对称处理（远离零），保证退款与补扣的舍入方向一致。
		half := big.NewInt(div)
		half.Div(half, big.NewInt(2))
		up := ceil
		if !up {
			absR := new(big.Int).Abs(r)
			up = absR.Cmp(half) >= 0
		}
		if up {
			if acc.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
	}
	if !q.IsInt64() {
		return 0, ErrAmountTooLarge
	}
	v := q.Int64()
	if err := Check(v); err != nil {
		return 0, err
	}
	return v, nil
}

// Sum 累加并检查溢出，用于「到账 = 实付 + 赠送」这类纯加法路径。
func Sum(items ...int64) (int64, error) {
	var total int64
	for _, it := range items {
		if it > 0 && total > math.MaxInt64-it {
			return 0, ErrAmountTooLarge
		}
		if it < 0 && total < math.MinInt64-it {
			return 0, ErrAmountTooLarge
		}
		total += it
	}
	return total, Check(total)
}

package billing

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"lingdong-api/internal/pkg/money"
)

// 计费常量（全部是**结算币种的微**，不再有 quota 刻度）。
//
// 这三个数字原先以 quota 计（1e10 / 2×qpc / 500000），含义随 qpc 漂移；
// 改成绝对金额后，"单次最多收 2 万"在任何币种下都是同一句话。
const (
	// DefaultMaxCharge 是单次请求的结算上限（2 万结算币种单位）。
	// 防上游异常用量把单请求算成天文数字；超限按该值截顶并打 charge_capped。
	DefaultMaxCharge int64 = 20_000 * money.Scale
	// DefaultZeroFrozenCap 是冻结额为 0 时算出非零金额的兜底上限（2 单位）。
	// 覆盖"按量短请求估算被舍入成 0、但上游真实用量远大于 0"的场景。
	DefaultZeroFrozenCap int64 = 2 * money.Scale
	// ArrearsBlock 是网关侧的欠费闸阈值（1 单位）：余额低于 -该值后仅网关返回 402，
	// 不修改用户/KEY 状态，充值到账即自动恢复（避免连带锁死登录与充值）。
	ArrearsBlock int64 = 1 * money.Scale
)

var (
	limitOnce        sync.Once
	maxChargeVal     int64
	zeroFrozenCapVal int64
	negativeLimitVal int64
)

func loadLimits() {
	maxChargeVal = envAmount("LD_MAX_CHARGE", DefaultMaxCharge)
	zeroFrozenCapVal = envAmount("LD_ZERO_FROZEN_CAP", DefaultZeroFrozenCap)
	negativeLimitVal = envAmount("LD_NEGATIVE_LIMIT", 0)
}

// envAmount 解析金额类环境变量。
//
// 非法值直接 panic：这类配置错了只会让"少收/多收"在线上静默发生，
// 而它每次结算都要读，事后无从发现。宁可启动即崩，也不能退回默认值
// —— 上一版用 `int64(parse)` 失败即回默认，正是"金额上限悄悄变成旧值"的根因。
func envAmount(name string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := money.Parse(raw)
	if err != nil {
		panic(fmt.Sprintf("billing: 环境变量 %s=%q 不是合法金额（示例：\"20000.00\"）：%v", name, raw, err))
	}
	return v
}

// maxCharge 返回单次结算上限（微）。
func maxCharge() int64 {
	limitOnce.Do(loadLimits)
	return maxChargeVal
}

// zeroFrozenCap 返回零冻结兜底上限（微）。
func zeroFrozenCap() int64 {
	limitOnce.Do(loadLimits)
	return zeroFrozenCapVal
}

// negativeLimit 返回允许的余额透支额度（微，默认 0 = 不允许透支）。
func negativeLimit() int64 {
	limitOnce.Do(loadLimits)
	return negativeLimitVal
}

// ResetLimitsForTest 重置环境变量缓存（仅测试用：t.Setenv 之后必须调用它）。
func ResetLimitsForTest() {
	limitOnce = sync.Once{}
}

// Package affcfg 是邀请返利配置的**唯一声明点**。
//
// 为什么单独成包：这份配置曾在四处各写一遍键表（systemconfig 的校验结构、
// affiliate 的运行时结构、seed 的出厂 map、迁移的重置 map）。键名必须逐字一致
// 才不会出现「保存能过、读取恒为 0」—— 那正是「开关开着、一分不发、还不报错」
// 的成因。这里收敛成一份，其余三处只引用本包。
//
// 两个结构体分工明确，不要合并：
//   - Raw 用指针，唯一用途是「判断运营到底有没有提交这个键」（保存校验）；
//   - Settings 用值，是运行时真正被读取的形态（缺键已由 Defaults 补齐）。
package affcfg

import (
	"bytes"
	"encoding/json"
	"fmt"

	"lingdong-api/internal/pkg/money"
)

// 值域常量。改这些数就是改产品边界，必须同步前端 min/max 与文档。
const (
	// MaxRate 是邀请人返利比例上限 100%（微比例 1e6 = 100%）。
	MaxRate int64 = 1_000_000
	// MinHoldHours 是冷却期下限。**不允许 0**：0 意味着返利签发即到期，
	// 用户可以立刻转入余额再申请退款，作废逻辑再也追不回来。
	// 退款是管理员人工操作、没有固定窗口，冷却期短于退款响应时间就等于把套利窗口留给用户。
	MinHoldHours int64 = 24
	// MaxHoldHours 是冷却期上限 30 天。
	MaxHoldHours int64 = 720
	// MaxRewardsRange 是「前 N 笔」的取值范围。
	MinMaxRewards int64 = 1
	MaxMaxRewards int64 = 100
	// MaxDailyInviteRange 是单邀请人 24h 绑定上限。**不允许 0**：
	// 0 的两种解释（不限 / 禁止）都不可观测，要「不限」请填上限值。
	MinDailyInviteCap int64 = 1
	MaxDailyInviteCap int64 = 1000
)

// Settings 是运行时形态（值语义，缺键已补齐）。
type Settings struct {
	Enabled bool `json:"enabled"`
	// InviterRate 是邀请人的返利比例，微比例（1e6 = 100%），与 group_discount 同口径。
	InviterRate int64 `json:"inviter_rate"`
	// InviteeAmount 是被邀请人**每笔**的赠金（微），且每笔不超过该笔实付。
	InviteeAmount int64 `json:"invitee_amount"`
	// MaxRewards 是参与返利的充值笔数（前 N 笔）。
	MaxRewards int64 `json:"max_rewards"`
	// HoldHours 是返利冷却期（小时）。
	HoldHours int64 `json:"hold_hours"`
	// MinTransfer 是最低转出金额（微）。
	MinTransfer int64 `json:"min_transfer"`
	// DailyInviteCap 是单邀请人 24h 内最多绑定的新用户数。
	DailyInviteCap int64 `json:"daily_invite_cap"`
}

// Raw 是保存校验用的形态：指针的唯一意义是区分「提交了 0」与「没提交」。
type Raw struct {
	Enabled        bool   `json:"enabled"`
	InviterRate    *int64 `json:"inviter_rate"`
	InviteeAmount  *int64 `json:"invitee_amount"`
	MaxRewards     *int64 `json:"max_rewards"`
	HoldHours      *int64 `json:"hold_hours"`
	MinTransfer    *int64 `json:"min_transfer"`
	DailyInviteCap *int64 `json:"daily_invite_cap"`
}

// Keys 返回全部子键（顺序即前端表单顺序）。
//
// 对外只暴露这一份清单：Read 的净化、迁移的重置、测试的断言都从这里取，
// 不允许任何调用方再手写键名。
func Keys() []string {
	return []string{
		"enabled", "inviter_rate", "invitee_amount", "max_rewards",
		"hold_hours", "min_transfer", "daily_invite_cap",
	}
}

// Defaults 是出厂值。seed（全新库）、迁移（老库重置）、Read（缺键补齐）共用。
func Defaults() Settings {
	return Settings{
		Enabled:        true,
		InviterRate:    100_000,    // 10%
		InviteeAmount:  5_000_000,  // 5 元/笔，且不超过该笔实付
		MaxRewards:     3,          // 前 3 笔
		HoldHours:      48,         // 冷却 48 小时
		MinTransfer:    10_000_000, // 10 元
		DailyInviteCap: 20,
	}
}

// Decode 严格解码。
//
// 不启用 DisallowUnknownFields：老库的混合行（旧键 + 新键）必须先能被读出来，
// 再由迁移清掉旧键；在这里报错会让「老库升级」变成「启动失败」。
// 旧键的清理只由 db 的 M42 负责。
func Decode(raw []byte) (Raw, error) {
	var r Raw
	if len(bytes.TrimSpace(raw)) == 0 {
		return r, fmt.Errorf("邀请返利配置为空")
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("邀请返利配置格式不合法: %w", err)
	}
	return r, nil
}

// Resolve 把库里的原始 JSON 解析成运行时形态。
//
// 返回 degraded=true 表示「这份配置不能按原样使用」：缺键、越界、脏 JSON、
// 或比例与赠金同时为 0。调用方**必须**把它暴露出去（Info.degraded）并打一条
// ERROR，否则就退化成旧实现的「静默全零」——开关开着、一分不发、没人知道。
//
// 越界值按 Normalize 收敛而不是回落到出厂值：运营填了 100 小时就该按 100 小时
// 生效（可解释），悄悄换成 48 小时反而更难查。
func Resolve(raw []byte) (Settings, bool, error) {
	r, err := Decode(raw)
	if err != nil {
		return Defaults(), true, err
	}
	s, missing, clamp := fromRaw(r)
	return s, missing || clamp, nil
}

// fromRaw 把 Raw 变成 Settings，并报告是否补过键、是否收敛过值。
func fromRaw(r Raw) (out Settings, missing, clamped bool) {
	d := Defaults()
	raw := Settings{
		Enabled:        r.Enabled,
		InviterRate:    valueOr(r.InviterRate, d.InviterRate),
		InviteeAmount:  valueOr(r.InviteeAmount, d.InviteeAmount),
		MaxRewards:     valueOr(r.MaxRewards, d.MaxRewards),
		HoldHours:      valueOr(r.HoldHours, d.HoldHours),
		MinTransfer:    valueOr(r.MinTransfer, d.MinTransfer),
		DailyInviteCap: valueOr(r.DailyInviteCap, d.DailyInviteCap),
	}
	missing = r.InviterRate == nil || r.InviteeAmount == nil || r.MaxRewards == nil ||
		r.HoldHours == nil || r.MinTransfer == nil || r.DailyInviteCap == nil
	out = Normalize(raw)
	return out, missing, out != raw
}

// valueOr 缺键时取出厂值。
func valueOr(v *int64, def int64) int64 {
	if v == nil {
		return def
	}
	return *v
}

// Normalize 把值收敛到合法域（**只收敛，不补键**）。
//
// 它是运行时的最后一道闸：库被人工改成 rate=5000000 时，返利按 100% 发而不是
// 按 500% 发（后者会让 MulDiv 越界、整笔充值回滚）。
func Normalize(s Settings) Settings {
	s.InviterRate = clamp(s.InviterRate, 0, MaxRate)
	s.InviteeAmount = clamp(s.InviteeAmount, 0, money.MaxAmount)
	s.MaxRewards = clamp(s.MaxRewards, MinMaxRewards, MaxMaxRewards)
	s.HoldHours = clamp(s.HoldHours, MinHoldHours, MaxHoldHours)
	s.MinTransfer = clamp(s.MinTransfer, 0, money.MaxAmount)
	s.DailyInviteCap = clamp(s.DailyInviteCap, MinDailyInviteCap, MaxDailyInviteCap)
	return s
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Validate 校验一次保存请求。
//
// 报错文案必须带「邀请返利」：前端 configTabs 靠这个关键词把后端报错定位回计费页签，
// 换个说法就变成「保存失败但不知道是哪一项」。
//
// 关闭状态仍然校验值域（只是不要求必填、也不要求「比例与赠金不能同时为 0」）：
// 允许「关着改参数」，但不允许把 500% 这种值存进库。
func (r Raw) Validate() error {
	if err := checkRange(r.InviterRate, "邀请人返利比例", 0, MaxRate, "0% ~ 100%", !r.Enabled); err != nil {
		return err
	}
	if err := checkRange(r.InviteeAmount, "被邀请人每笔奖励", 0, money.MaxAmount, "", !r.Enabled); err != nil {
		return err
	}
	if err := checkRange(r.MaxRewards, "参与返利的充值笔数", MinMaxRewards, MaxMaxRewards, "", !r.Enabled); err != nil {
		return err
	}
	if err := checkRange(r.HoldHours, "返利冷却期", MinHoldHours, MaxHoldHours, "", !r.Enabled); err != nil {
		return err
	}
	if err := checkRange(r.MinTransfer, "最低转出金额", 0, money.MaxAmount, "", !r.Enabled); err != nil {
		return err
	}
	// daily_invite_cap 允许缺省（老前端不提交），提交了就必须在域内。
	if r.DailyInviteCap != nil {
		if err := checkRange(r.DailyInviteCap, "每日邀请上限", MinDailyInviteCap, MaxDailyInviteCap, "", false); err != nil {
			return err
		}
	}
	if r.Enabled && r.InviterRate != nil && r.InviteeAmount != nil &&
		*r.InviterRate == 0 && *r.InviteeAmount == 0 {
		return fmt.Errorf("邀请返利「邀请人返利比例」与「被邀请人每笔奖励」不能同时为 0（否则开关开着也一分不发）")
	}
	return nil
}

// checkRange 校验单个数值键；optional=true 时允许缺省。
func checkRange(v *int64, label string, lo, hi int64, unit string, optional bool) error {
	if v == nil {
		if optional {
			return nil
		}
		return fmt.Errorf("邀请返利「%s」不能为空（单位：微，1 货币单位 = 1000000 微）", label)
	}
	if *v < lo || *v > hi {
		rangeText := money.Format(lo, 2) + " ~ " + money.Format(hi, 2)
		if unit != "" {
			rangeText = unit
		}
		return fmt.Errorf("邀请返利「%s」须在 %s 之间", label, rangeText)
	}
	return nil
}

// SanitizeMap 把库里的任意 map 收敛成「只含已知键」的形态，供管理台回显。
//
// 这是「旧键复活」的唯一根治点：前端把 GET /admin/config 的 config 原样
// structuredClone 后整份提交回来，只要回显里还带着 reward_for_inviter，
// 它就会被写回库 —— 迁移清一次、下次保存又回来。
//
// 保留原始值（json.Number）不改写：管理台要看到运营真正填的数字，越界值
// 由前端标红、保存时由 Validate 拒绝。缺失键补出厂值，避免表单显示空白。
func SanitizeMap(m map[string]any) map[string]any {
	d := Defaults()
	base := map[string]any{
		"enabled":          d.Enabled,
		"inviter_rate":     d.InviterRate,
		"invitee_amount":   d.InviteeAmount,
		"max_rewards":      d.MaxRewards,
		"hold_hours":       d.HoldHours,
		"min_transfer":     d.MinTransfer,
		"daily_invite_cap": d.DailyInviteCap,
	}
	out := make(map[string]any, len(base))
	for _, k := range Keys() {
		if v, ok := m[k]; ok && v != nil {
			out[k] = v
			continue
		}
		out[k] = base[k]
	}
	return out
}

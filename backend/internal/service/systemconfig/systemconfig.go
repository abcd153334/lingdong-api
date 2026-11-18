// Package systemconfig 是 system_config 的统一读写入口（S10，step-10 §⑤）。
// 职责：全量读取（敏感字段脱敏回显）、全量保存（键集校验 → 值校验 → 落库 → 热更新 → 审计）。
package systemconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/affcfg"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/pkg/sitecfg"
	"lingdong-api/internal/pkg/uploadcfg"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/configsync"
	"lingdong-api/internal/service/payment"
	"lingdong-api/internal/service/pricing"
	"lingdong-api/internal/service/storage"
)

// Mask 是敏感字段的回显占位符；提交时字段值等于该串表示「保持原值不变」。
const Mask = "******"

// Keys 是冻结的键集（31 个）。
//
// 新增/删除键要同时改：seed、前端 AdminConfigValues、e2e config.spec 的键数断言、
// docs/v1.1 的 system_config 键表。prototype/mock-data.js 是设计稿快照，不参与运行。
//
// 已删除的三个键（不再有对应概念，留痕备查）：
//   - recharge_ratio：额度刻度（qpc）。金额已是绝对微，没有「1 元兑多少额度」这层换算；
//   - recharge_multiplier：充值倍率。到账 = 实付，不再有让利系数；
//   - group_ratios：分组倍率（float）。改为 group_discount（微比例整数，1e6 = 不打折）。
var Keys = []string{
	// portal_enabled 是运营模式总开关（关闭 = 纯服务端：门户站点不可用，仅管理员可登录）。
	// 放第一位：它决定「这套部署对外提供什么」，比任何业务参数都靠前。
	"portal_enabled",
	"site", "group_discount", "cny_per_usd", "balance_limit",
	"min_topup", "custom_topup_enabled", "pay_channels", "retry_times",
	"file_retention_days", "poll_deadline_min", "logs_retention_days",
	"archive_retention_days", "audit_retention_days", "rate_limit_defaults",
	"auth_captcha", "auth_login", "auth_session", "auth_register",
	"auth_mfa", "auth_event_retention_days",
	"storage", "uploads", "email",
	"sms", "aff", "monitor", "announcements", "about_content", "agreement_content",
	"modality_defaults",
	// 发布清单地址：一键更新据此判断"有没有新版本"。放通用页签（与站点品牌同屏，
	// 因为它们都属于"这套部署对外是什么样子"）。
	"release_manifest_url",
}

// 审计子动作映射：键变化时追加对应审计动作（G18 枚举）。
var auditByKey = map[string]string{
	// 门户开关改的是「谁能登录」，与 auth_* 同域；出事时同样要回答「什么时候被谁关的」。
	"portal_enabled": audit.ActionConfigAuth,
	"pay_channels":   audit.ActionConfigPay,
	"storage":        audit.ActionConfigStorage,
	"uploads":        audit.ActionConfigFiles,
	"email":          audit.ActionConfigNotify,
	"sms":            audit.ActionConfigNotify,
	// 认证策略改动会直接改变「谁能注册、能被爆破多久、会话活多久」，
	// 必须留痕：出事时第一个要回答的问题就是「这个阈值什么时候被谁改的」。
	"auth_captcha":              audit.ActionConfigAuth,
	"auth_login":                audit.ActionConfigAuth,
	"auth_session":              audit.ActionConfigAuth,
	"auth_register":             audit.ActionConfigAuth,
	"auth_mfa":                  audit.ActionConfigAuth,
	"auth_event_retention_days": audit.ActionConfigAuth,
}

var keySet = func() map[string]bool {
	m := make(map[string]bool, len(Keys))
	for _, k := range Keys {
		m[k] = true
	}
	return m
}()

// Read 返回全量配置（含敏感字段脱敏）。数字以 json.Number 保留字面量精度的原始形态。
func Read() map[string]any {
	out := make(map[string]any, len(Keys))
	for _, k := range Keys {
		raw, ok := conf.System().Get(k)
		if !ok {
			continue
		}
		// uploads 必须补齐成完整形状再回显：缺 types/limits/compress 的存量值
		// 会让管理台渲染崩溃（压缩开关直接读 compress.enabled）。补齐点只在这里，
		// 前端不维护第二份默认值。
		if k == "uploads" {
			out[k] = uploadcfg.Normalize(raw)
			continue
		}
		v, err := decode(raw)
		if err != nil {
			continue
		}
		// aff 必须收敛成「只含已知键」：前端拿到 config 后 structuredClone 整份提交回来，
		// 回显里只要还带着 reward_for_inviter 之类的旧键，它就会被写回库 ——
		// 迁移清一次、下次保存又回来，而旧键与新键混存正是「保存成功、门户全 0」的现场。
		if k == "aff" {
			m, _ := v.(map[string]any)
			out[k] = affcfg.SanitizeMap(m)
			continue
		}
		out[k] = maskSensitive(v)
	}
	return out
}

// Update 保存配置：键集白名单 → 敏感字段还原 → 逐键语义校验 → 单事务落库 → 热更新 → 审计。
// values 为「键 → JSON 值」的扁平映射；未出现的键保持原值。
func Update(ctx context.Context, gdb *gorm.DB, adminID uint64, values map[string]json.RawMessage) error {
	if len(values) == 0 {
		return errs.InvalidParam
	}
	prev := conf.System().All()

	// ① 键集白名单 + 敏感字段还原
	next := make(map[string]json.RawMessage, len(values))
	for k, raw := range values {
		if !keySet[k] {
			return errs.InvalidParam
		}
		v, err := decode(raw)
		if err != nil {
			return errs.InvalidParam
		}
		var prevV any
		if p, ok := prev[k]; ok {
			prevV, _ = decode(p)
		}
		merged, err := json.Marshal(restoreSensitive(v, prevV, k))
		if err != nil {
			return errs.InvalidParam
		}
		next[k] = merged
	}

	// ② 逐键语义校验
	if err := validate(ctx, gdb, adminID, next, prev); err != nil {
		return err
	}

	// ③ 凭据封印：**只有落库用密文，内存镜像仍放明文**。
	//
	// 内存必须是明文：configsync 每轮用 db.LoadSystemConfig 整表重载，只要装载侧解封、
	// 写入侧封印两头对齐，镜像里就永远是明文；漏掉任何一头，都会出现
	// 「配置页显示正常、几十秒后所有云端写入开始 401 / 回调验签全失败」——
	// 因为镜像里躺的是密文，被当成密钥发去了上游。
	//
	// 哪些键要封由 storage.SecretConfigKeys() 决定（云存储后端 + 支付渠道），
	// 这里不写死键名：写死的那一刻就注定了「新加的凭据配置忘了封印」。
	stored := next
	copied := false
	for _, key := range storage.SecretConfigKeys() {
		raw, ok := next[key]
		if !ok {
			continue
		}
		sealed, failed, err := storage.SealSecrets(key, raw)
		if err != nil {
			return errs.InvalidParam.WithMessage("配置格式不合法，无法保存")
		}
		// 加密失败时字段已被置空（绝不落明文），照常保存等于把凭据清空 ——
		// 必须中止，并点明是主密钥/运行环境的问题，而不是让运营去猜。
		if len(failed) > 0 {
			return errs.Internal.WithMessage(
				"凭据加密失败，本次配置未保存（请检查 APP_ENCRYPTION_KEY 与服务运行环境）")
		}
		// 逐字节比较而不是无条件复制：封印对已是密文的输入是幂等的，
		// 但 map 键序不保证，重排过的同值也会被判定为「变了」。只在真需要时才复制整表。
		if string(sealed) != string(raw) {
			if !copied {
				stored = make(map[string]json.RawMessage, len(next))
				for k, v := range next {
					stored[k] = v
				}
				copied = true
			}
			stored[key] = sealed
		}
	}

	// ④ 落库：单事务 upsert。任一键失败整次回滚 —— 配置是「一整套口径」，
	// 半份落库（比如币种写进去了、汇率没写）比整次失败难查得多。
	now := time.Now()
	err := gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for k, v := range stored {
			row := model.SystemConfig{Key: k, Value: model.JSON(v), UpdatedAt: now}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	changed := make([]string, 0, len(next))
	for k := range next {
		changed = append(changed, k)
	}
	sort.Strings(changed)

	// ⑤ 热更新：SetMany 在同一把写锁内写完整包，随后一次性通知订阅者。
	// 逐个 Set 会留下「币种已改成 USD、汇率还是人民币」的真实窗口，此刻任何一次
	// 冻结都会把错误口径写进快照，事后无法复现也无法追溯。
	conf.System().SetMany(next)
	// 请求体门带 3 秒缓存（热路径不解析配置）：保存后立刻失效，
	// 免得「刚把上限调大、下一个请求仍被旧门拦」被当成配置没生效。
	uploadcfg.ResetCapCache()

	// ⑥ 多副本同步 + 定价缓存失效（pub/sub 实时；30s 轮询兜底）
	configsync.NotifySystem(changed)
	billDiff := billingSwitchDiff(next, prev)
	if billDiff != nil || anyRepriceKey(changed) {
		pricing.InvalidatePricingCache()
	}

	// ⑦ 审计：一条汇总（含币种切换前后值）+ 每个受影响的域各一条子动作
	detail := map[string]any{"keys": changed}
	for k, v := range billDiff {
		detail[k] = v
	}
	audit.RecordOrWarn(gdb, adminID, audit.ActionConfigUpdate, "system_config", "", detail)
	for _, k := range changed {
		if act, ok := auditByKey[k]; ok {
			audit.RecordOrWarn(gdb, adminID, act, "system_config", k, map[string]any{"key": k})
		}
	}
	return nil
}

// validateAuth 校验认证策略键（auth_captcha / auth_login / auth_session /
// auth_register / auth_mfa / auth_event_retention_days）。
//
// 为什么单独一个函数而不是平铺在 validate 里：这些约束彼此相关
// （退避阶梯与阈值、渠道就绪与 contact_verify、绝对有效期与空闲超时），
// 放在一起才能一眼看出「哪些组合是不允许的」。
func validateAuth(ctx context.Context, gdb *gorm.DB, adminID uint64, next map[string]json.RawMessage) error {
	bad := func(msg string) error { return errs.InvalidParam.WithMessage(msg) }

	if raw, ok := next["auth_session"]; ok {
		var v struct {
			AbsoluteTTLSec   int `json:"absolute_ttl_sec"`
			IdleTTLSec       int `json:"idle_ttl_sec"`
			MaxActive        int `json:"max_active"`
			TouchIntervalSec int `json:"touch_interval_sec"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return bad("会话策略格式不合法")
		}
		if v.IdleTTLSec < 300 {
			return bad("会话空闲超时不得低于 300 秒")
		}
		// 绝对有效期 <= 空闲超时等于「空闲超时永不生效」：会话到期前一直是活跃的，
		// 于是「闲置 12 小时自动退出」这条策略被静默取消。
		if v.AbsoluteTTLSec <= v.IdleTTLSec {
			return bad("会话绝对有效期必须大于空闲超时")
		}
		if v.MaxActive < 1 || v.MaxActive > 50 {
			return bad("单用户活跃会话上限必须在 1–50 之间")
		}
		if v.TouchIntervalSec < 0 || v.TouchIntervalSec > 3600 {
			return bad("活跃时间写入间隔必须在 0–3600 秒之间")
		}
	}

	if raw, ok := next["auth_login"]; ok {
		var v struct {
			FailThreshold   int   `json:"fail_threshold"`
			BackoffSeconds  []int `json:"backoff_seconds"`
			IPFailLimit     int   `json:"ip_fail_limit"`
			IPFailWindowSec int   `json:"ip_fail_window_sec"`
			DelayMaxMS      int   `json:"delay_max_ms"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return bad("登录失败策略格式不合法")
		}
		if v.FailThreshold < 3 || v.FailThreshold > 20 {
			return bad("转验证码的失败次数阈值必须在 3–20 之间")
		}
		if v.IPFailLimit < 5 || v.IPFailLimit > 200 {
			return bad("单 IP 失败次数上限必须在 5–200 之间")
		}
		if v.IPFailWindowSec < 60 || v.IPFailWindowSec > 86400 {
			return bad("单 IP 失败统计窗口必须在 60–86400 秒之间")
		}
		if v.DelayMaxMS < 0 || v.DelayMaxMS > 3000 {
			return bad("退避延迟上限必须在 0–3000 毫秒之间")
		}
		if len(v.BackoffSeconds) == 0 {
			return bad("退避阶梯不能为空")
		}
		prev := 0
		for _, s := range v.BackoffSeconds {
			if s < 1 {
				return bad("退避阶梯的每一档都必须大于 0 秒")
			}
			if s <= prev {
				return bad("退避阶梯必须严格递增（例如 60, 300, 900）")
			}
			prev = s
		}
		if v.BackoffSeconds[len(v.BackoffSeconds)-1] > 3600 {
			return bad("退避阶梯的最大档不得超过 3600 秒")
		}
	}

	if raw, ok := next["auth_captcha"]; ok {
		var v struct {
			Scenes map[string]string `json:"scenes"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return bad("人机验证策略格式不合法")
		}
		// 只有登录可配。注册与找回恒为 always（代码常量），配置里出现它们
		// 也不读——写进来只会让人以为"配了这个键就生效"。
		switch v.Scenes["login"] {
		case "always", "after_fails", "off":
		case "":
			return bad("人机验证的登录策略不能为空")
		default:
			return bad("人机验证的登录策略只能是 always / after_fails / off")
		}
	}

	if raw, ok := next["auth_register"]; ok {
		var v struct {
			Mode            string   `json:"mode"`
			ContactVerify   string   `json:"contact_verify"`
			ContactChannels []string `json:"contact_channels"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return bad("注册策略格式不合法")
		}
		switch v.Mode {
		case "open", "invite_only", "closed":
		default:
			return bad("注册模式只能是 open / invite_only / closed")
		}
		switch v.ContactVerify {
		case "required", "off":
		default:
			return bad("联系方式验证只能是 required 或 off")
		}
		seen := make(map[string]bool, len(v.ContactChannels))
		channels := make([]string, 0, len(v.ContactChannels))
		for _, ch := range v.ContactChannels {
			switch ch {
			case "email", "sms":
			default:
				return bad("联系方式渠道只能是 email 或 sms")
			}
			if seen[ch] {
				continue
			}
			seen[ch] = true
			channels = append(channels, ch)
		}
		if v.ContactVerify == "required" && len(channels) == 0 {
			return bad("开启联系方式验证时必须至少选择一个渠道")
		}
		// 「要求验证」但渠道没就绪 = 注册入口必然 500/发不出码。
		// 这里必须拒绝保存，不能等到用户点注册才发现。
		if v.ContactVerify == "required" {
			ready := false
			for _, ch := range channels {
				if channelReady(next, ch) {
					ready = true
					break
				}
			}
			if !ready {
				return bad("所选联系方式渠道尚未启用：请先在「通知」里配置并启用，再开启联系方式验证")
			}
		}
	}

	if raw, ok := next["auth_mfa"]; ok {
		var v struct {
			Policy string `json:"policy"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return bad("二次验证策略格式不合法")
		}
		switch v.Policy {
		case "off", "optional":
		case "required_for_admin":
			// 开启前必须确认「操作者本人已绑定」：策略一保存就立刻对全部管理员生效，
			// 若他自己还没绑，点下「保存」的瞬间就把整个管理台关在门外
			// （唯一后门是服务器上的 cmd/implant，多数部署没有交互式 shell）。
			var cnt int64
			if err := gdb.WithContext(ctx).Model(&model.UserMFA{}).
				Where("uid = ? AND enabled = ?", adminID, true).Count(&cnt).Error; err != nil {
				return err
			}
			if cnt == 0 {
				return bad("请先为当前管理员开启二次验证，再启用「管理员强制二次验证」")
			}
		default:
			return bad("二次验证策略只能是 off / optional / required_for_admin")
		}
	}

	if raw, ok := next["auth_event_retention_days"]; ok {
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			return bad("认证日志保留天数必须是整数")
		}
		if v < 7 || v > 730 {
			return bad("认证日志保留天数必须在 7–730 之间")
		}
	}
	return nil
}

// channelReady 判断某个通知渠道是否已启用。
//
// 取值优先用本次提交的 next（运营可能同一次保存里既打开 email 又开验证），
// 否则回落到内存配置（上一次保存后的生效值）。
func channelReady(next map[string]json.RawMessage, channel string) bool {
	raw, ok := next[channel]
	if !ok {
		raw, ok = conf.System().Get(channel)
		if !ok {
			return false
		}
	}
	var v struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return v.Enabled
}

// checkNum 校验数值键的下限（存在才校验）。
func checkNum(next map[string]json.RawMessage, key string, min float64) error {
	raw, ok := next[key]
	if !ok {
		return nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return errs.InvalidParam.WithMessage(key + " 格式不合法")
	}
	if v < min {
		return errs.InvalidParam.WithMessage(key + " 取值不合法")
	}
	return nil
}

// microLabel 是金额类配置键的中文名（错误文案用）。
var microLabel = map[string]string{
	"balance_limit": "余额上限",
	"min_topup":     "最低充值金额",
}

// checkMicro 校验金额类配置键：必须是**整数微**，非负且不越界。
//
// 为什么必须是整数而不是小数：读取端 conf.System().Int() 对小数会静默回落默认值，
// 于是「后台显示 0.5 元、实际按默认值生效」且全程没有任何报错。
// 前端输入的是元，提交前必须换算成微（scale 由定价快照下发）。
func checkMicro(next map[string]json.RawMessage, key string) error {
	raw, ok := next[key]
	if !ok {
		return nil
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return errs.InvalidParam.WithMessage(microLabel[key] + "必须是整数（单位：微，1 元 = 1000000 微）")
	}
	if v < 0 {
		return errs.InvalidParam.WithMessage(microLabel[key] + "不能为负数")
	}
	if err := money.Check(v); err != nil {
		return errs.InvalidParam.WithMessage(microLabel[key] + "超出可表示范围")
	}
	return nil
}

// confIntOf 取本次提交值，缺省回落当前生效值。
func confIntOf(next, prev map[string]json.RawMessage, key string) int64 {
	if raw, ok := next[key]; ok {
		var v int64
		if json.Unmarshal(raw, &v) == nil {
			return v
		}
	}
	if raw, ok := prev[key]; ok {
		var v int64
		if json.Unmarshal(raw, &v) == nil {
			return v
		}
	}
	return 0
}

// channelField 是渠道专属参数 schema 的一项：[键, 标签, 是否必填]。
type channelField = [3]string

// storageSchemes 是云存储的字段 schema（运行期真正读哪些字段，这里就必须列全）。
//
// s3/minio 的 region 与 path_style 以前只在 s3.go 里被读、schema 里没有 ⇒
// 运营改不了、校验看不见，只能手工改库。cos 的 endpoint 同理
// （私有化 / 加速域名的逃生口）。
var storageSchemes = map[string][]channelField{
	"s3": {
		{"endpoint", "Endpoint", "1"}, {"bucket", "Bucket", "1"},
		{"access_key", "Access Key", "1"}, {"secret_key", "Secret Key", "1"},
		{"region", "地域（默认 us-east-1）", "0"}, {"path_style", "Path Style（true 走路径寻址）", "0"},
	},
	"oss": {
		{"endpoint", "Endpoint（如 oss-cn-hangzhou.aliyuncs.com）", "1"}, {"bucket", "Bucket", "1"},
		{"access_key_id", "AccessKey ID", "1"}, {"access_key_secret", "AccessKey Secret", "1"},
	},
	"cos": {
		{"region", "地域（如 ap-guangzhou）", "1"}, {"bucket", "Bucket（name-appid）", "1"},
		{"secret_id", "SecretId", "1"}, {"secret_key", "SecretKey", "1"},
		{"endpoint", "访问域名（可选，私有化/加速域名）", "0"},
	},
	"minio": {
		{"endpoint", "Endpoint", "1"}, {"bucket", "Bucket", "1"},
		{"access_key", "Access Key", "1"}, {"secret_key", "Secret Key", "1"},
		{"region", "地域（默认 us-east-1）", "0"}, {"path_style", "Path Style（true 走路径寻址）", "0"},
	},
	"none": {},
}

// OrphanGroups 返回「有账号挂在上面、但分组折扣表里没有」的分组名（升序）。
//
// 这类分组会静默按 1.0（不打折）计费：计价不报错、账单也不会少，
// 只是运营以为「我给 vip 配了九折」，而那批账号从来没吃上。
// 把它顶在分组折扣表单上方，比事后对账便宜得多。
//
// 判据只看 users.group：分组是**账号**属性，密钥上不接受该字段 ——
// 能自填分组等于能给自己打折、能进专属渠道池。
func OrphanGroups(ctx context.Context, gdb *gorm.DB) []string {
	if gdb == nil {
		return nil
	}
	known := make(map[string]bool)
	if raw, ok := conf.System().Get("group_discount"); ok {
		var m map[string]int64
		if json.Unmarshal(raw, &m) == nil {
			for k := range m {
				known[k] = true
			}
		}
	}
	var groups []string
	// group 是 PostgreSQL 保留字：不加引号的 `group <> ''` 会直接 42601，
	// 而这里又吞掉了 err（返回 nil），表现为「孤儿分组提示永远不出现」。
	if err := gdb.WithContext(ctx).Model(&model.User{}).
		Distinct().Where("\"group\" <> ''").Pluck("group", &groups).Error; err != nil {
		return nil
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if !known[g] {
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

// StorageScheme 是配置页渲染云存储渠道表单所需的 schema。//
// 字段第三项是「是否必填」的 "1"/"0"，与 storageSchemes 的原样形态一致 ——
// 转成 bool 只会多一层可能出错的转换。
type StorageScheme struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Fields [][3]string `json:"fields"`
}

// storageSchemeOrder 是配置页的展示顺序（本地磁盘在最前：它是默认值）。
var storageSchemeOrder = []string{"none", "s3", "minio", "oss", "cos"}

// StorageSchemes 返回云存储渠道 schema（唯一真相源 = storageSchemes）。
//
// 供管理台配置页渲染表单。前端自带一份渠道字段列表的后果是必然漂移，
// 而漂移的表现是「后端支持的字段在前端填不了」（例如 COS 的私有化域名）。
func StorageSchemes() []StorageScheme {
	out := make([]StorageScheme, 0, len(storageSchemeOrder))
	for _, id := range storageSchemeOrder {
		fields, ok := storageSchemes[id]
		if !ok {
			continue
		}
		f := make([][3]string, len(fields))
		copy(f, fields)
		out = append(out, StorageScheme{ID: id, Name: SchemeLabel(id), Fields: f})
	}
	return out
}

// SchemeLabel 返回渠道的中文名（错误文案用；未知值回落原值）。
func SchemeLabel(scheme string) string {
	switch scheme {
	case "none", "":
		return "本地磁盘"
	case "s3":
		return "S3 兼容"
	case "minio":
		return "MinIO"
	case "oss":
		return "阿里云 OSS"
	case "cos":
		return "腾讯云 COS"
	}
	return scheme
}

var smsSchemes = map[string][]channelField{
	"aliyun":  {{"access_key_id", "AccessKey ID", "1"}, {"access_key_secret", "AccessKey Secret", "1"}, {"sign_name", "短信签名", "1"}, {"template_code", "模板 CODE", "1"}},
	"tencent": {{"secret_id", "SecretId", "1"}, {"secret_key", "SecretKey", "1"}, {"sdk_app_id", "SDK App ID", "1"}, {"sign_name", "短信签名", "1"}, {"template_id", "模板 ID", "1"}},
}

// checkChannelRequired 仅校验当前选中渠道的必填参数（级联保存语义）。
func checkChannelRequired(ch map[string]any, scheme []channelField, label string) error {
	for _, f := range scheme {
		if f[2] != "1" {
			continue
		}
		if v, ok := ch[f[0]].(string); !ok || strings.TrimSpace(v) == "" {
			return errs.InvalidParam.WithMessage(label + "「" + f[1] + "」未填写")
		}
	}
	return nil
}

// repriceKeys 是「改了它们必须重建定价快照」的配置键。
//
// 少一个键的表现是「保存成功但价格还是旧的」，一直持续到缓存自然过期；
// 多一个键只是白重建一次快照。所以宁可列全，但不要把什么都塞进来：
// site 只有**结算币种**变化才影响价签，改站点名/Logo 不该触发整份重建，
// 那种情况由 billingSwitchDiff 单独判定（见 Update ⑥）。
var repriceKeys = map[string]bool{
	"group_discount":    true,
	"cny_per_usd":       true,
	"modality_defaults": true,
}

// anyRepriceKey 报告本次变更键里是否含定价相关键。
func anyRepriceKey(keys []string) bool {
	for _, k := range keys {
		if repriceKeys[k] {
			return true
		}
	}
	return false
}

// modelRow 是 checkDefaultModel 的列投影（只取校验要用的三列）。
type modelRow struct {
	ID       string `gorm:"column:id"`
	Modality string `gorm:"column:modality"`
	Enabled  bool   `gorm:"column:enabled"`
}

// checkDefaultModel 校验默认模型存在、已启用且模态匹配（B7 兜底来源）。
func checkDefaultModel(ctx context.Context, gdb *gorm.DB, modality, modelID string) error {
	var m modelRow
	err := gdb.WithContext(ctx).Model(&model.Model{}).
		Select("id, modality, enabled").Where("id = ?", modelID).Take(&m).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return errs.InvalidParam.WithMessage("默认模型「" + modality + "」在模型目录中不存在")
		}
		return err
	}
	if !m.Enabled {
		return errs.InvalidParam.WithMessage("默认模型「" + modality + "」未启用")
	}
	if m.Modality != modality {
		return errs.InvalidParam.WithMessage("默认模型「" + modality + "」与模型模态不符")
	}
	return nil
}

// currencyChanged 判断本次提交是否**真的**改变了结算币种。
//
// 只认「next 里有 site 且归一化后与现值不同」：运营改站点名/Logo 也会带 site，
// 那种提交不能被当成切换币种来处理（否则每次保存都要重填充值比例）。
func currencyChanged(next, prev map[string]json.RawMessage) bool {
	if _, ok := next["site"]; !ok {
		return false
	}
	return siteCurrencyOf(next) != siteCurrencyOf(prev)
}

// billingSwitchDiff 记录「结算口径切换」的前后值，供审计留痕。
//
// 为什么必须记录：审计只写 {keys:["site"]} 的话，事后只能看到「改过」，
// 无法回答「什么时候从人民币切到美元」。而这个事实决定了全部历史金额该按
// 哪种口径解释 —— 对账、客诉、退款都依赖它。
//
// 只在币种真的变了时返回；返回 nil 表示没有切换。
func billingSwitchDiff(next, prev map[string]json.RawMessage) map[string]any {
	fromCur, toCur := siteCurrencyOf(prev), siteCurrencyOf(next)
	if fromCur == toCur {
		return nil
	}
	return map[string]any{"currency": map[string]any{"from": fromCur, "to": toCur}}
}

// ledgerRecords 报告账务相关表里最早有记录的一张（表名, 条数）。
//
// 用途只有一个：判定「结算币种是否还能改」。三张表都要查，缺一不可 ——
// 只查 orders 会漏掉「兑换码入账但没有订单」的站点，
// 只查 usages 会漏掉「充过值但没调用过 API」的站点。
func ledgerRecords(ctx context.Context, gdb *gorm.DB) (table string, n int64, err error) {
	for _, t := range []struct {
		label string
		m     any
	}{
		{"订单", &model.Order{}},
		{"资金流水", &model.Transaction{}},
		{"调用账单", &model.Usage{}},
	} {
		var c int64
		if err := gdb.WithContext(ctx).Model(t.m).Limit(1).Count(&c).Error; err != nil {
			return "", 0, err
		}
		if c > 0 {
			return t.label, c, nil
		}
	}
	return "", 0, nil
}

// siteCurrencyOf 取 site 子树归一化后的结算币种（缺键/空串/非法 → CNY）。
// 与 conf.SiteCurrency 的回落口径保持一致：空串代表「未设置」，不是错误。
func siteCurrencyOf(m map[string]json.RawMessage) string {
	raw, ok := m["site"]
	if !ok {
		return conf.CurrencyCNY
	}
	var s sitecfg.Site
	if json.Unmarshal(raw, &s) != nil {
		return conf.CurrencyCNY
	}
	if c, ok := conf.NormalizeCurrency(s.Currency); ok {
		return c
	}
	return conf.CurrencyCNY
}

func badConfig(msg string) error { return errs.InvalidParam.WithMessage(msg) }

// payChannelIDPattern 是支付渠道稳定 ID 的格式：与订单 pay_channel_id 列宽（32）一致，
// 且必须是 URL 安全字符 —— 它会出现在回调路径 /api/pay/{id}/notify 里。
var payChannelIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// validatePayChannels 校验支付渠道配置。
//
// 三条原则：
//  1. **必填只校验「本次改动过」的启用渠道**。种子里 epay 的 key 就是空串且 enabled=true，
//     若对所有启用渠道强制必填，管理员点任何一次保存都会 400 —— 而那个空值正是种子自己写的。
//  2. 网关地址必须 https。http 会被上游 301 到 https，而支付出站客户端禁止重定向，
//     表现为「地址不被允许」，完全看不出真实原因。
//  3. 删除渠道前检查在途订单。删掉后 FindChannel 返回 false，在途订单的回调永远无法验签，
//     钱收了额度不到账，且网关重试 6 次后放弃，连补单线索都没有。
func validatePayChannels(ctx context.Context, gdb *gorm.DB, next, prev map[string]json.RawMessage) error {
	var list []payment.Channel
	if err := json.Unmarshal(next["pay_channels"], &list); err != nil {
		return badConfig("支付渠道格式不合法")
	}
	// JSON 的 null 解出来是 nil（不是错误）。不归一化就会把 null 原样落库，
	// 之后 payment.Channels() 与前端 .map 都会拿到 nil，且前端会以为「配置丢了」。
	if list == nil {
		list = []payment.Channel{}
	}

	prevByID := map[string]payment.Channel{}
	if raw, ok := prev["pay_channels"]; ok {
		var old []payment.Channel
		if json.Unmarshal(raw, &old) == nil {
			for _, ch := range old {
				prevByID[ch.ID] = ch
			}
		}
	}

	seen := map[string]bool{}
	seenName := map[string]bool{}
	nextIDs := map[string]bool{}
	// 同协议内商户标识必须唯一：两条渠道用同一套 app_id/pid 时，回调的商户号
	// 一致性校验（merchantOf）会把正常订单判成「不一致」转人工；
	// 更糟的是运营分不清钱到底进了哪个通道，所以要在保存期就拦住。
	seenMerchant := map[string]string{}
	for i := range list {
		ch := &list[i]
		ch.ID = strings.TrimSpace(ch.ID)
		ch.Name = strings.TrimSpace(ch.Name)
		if ch.ID == "" || ch.Name == "" {
			return badConfig("支付渠道 id/名称不能为空")
		}
		if !payChannelIDPattern.MatchString(ch.ID) {
			return badConfig(fmt.Sprintf("支付渠道 id「%s」不合法：只能用小写字母/数字/下划线/中划线，长度 1–32", ch.ID))
		}
		if strings.Contains(ch.Name, "-") {
			// 订单的 pay_channel 快照是「渠道名-支付方式」，历史回填按第一个 '-' 拆；
			// 名字里再带 '-' 会让拆分错位（看不出错，但迁移会映射到错误的渠道）。
			return badConfig(fmt.Sprintf("支付渠道名称「%s」不能包含中划线（-）", ch.Name))
		}
		if seen[ch.ID] {
			return badConfig("支付渠道 id 重复")
		}
		seen[ch.ID] = true
		nextIDs[ch.ID] = true
		// 名称不强制唯一（现网历史配置里可能已有同名，硬校验会让存量库连改名都保存不了），
		// 但订单的 pay_channel 快照是「渠道名-支付方式」，重名会让对账分不清是哪条渠道。
		if seenName[ch.Name] {
			logger.L().Warn("支付渠道名称重复：订单快照按名称展示，重名会让对账难以区分",
				zap.String("channel", ch.ID), zap.String("name", ch.Name))
		}
		seenName[ch.Name] = true

		proto := payment.ParseProtocol(ch.Protocol)
		oldCh, existed := prevByID[ch.ID]
		if strings.TrimSpace(ch.Protocol) != "" && !proto.Valid() {
			// **回滚保护**：这条检查对所有渠道生效且在最前面，若一律 400，
			// 那么新协议上线后一旦回滚（新版本里 Valid() 不含它），库里存在该协议的
			// 渠道就会让**整页配置无法保存**（连改站点名都 400）。
			// 规则与 aggregate 一致：只拒「本次新引入」的未知协议，存量原样放行。
			if existed && oldCh.Protocol == ch.Protocol {
				logger.L().Warn("支付渠道的协议在当前版本未实现，保留原样（禁止新引入）",
					zap.String("channel", ch.ID), zap.String("protocol", ch.Protocol))
			} else {
				return badConfig(fmt.Sprintf("支付渠道「%s」的协议不支持：%s（只支持 %s）",
					ch.Name, ch.Protocol, strings.Join(payment.ProtocolNames(), " / ")))
			}
		}
		// 协议不可变：回调验签、查单、退款、订单快照全部按 ch.Protocol 分发，
		// 已支付订单的原路退款也走它 —— 改协议会让这些订单退化成「只能回退余额」。
		// 唯一放行的是 "" → 有效值（历史渠道 / 占位补全），那不是「换协议」。
		if oldCh, existed := prevByID[ch.ID]; existed {
			if oldProto := payment.ParseProtocol(oldCh.Protocol); oldProto.Valid() && proto != oldProto {
				return badConfig(fmt.Sprintf("支付渠道「%s」的协议创建后不可修改（%s → %s）：如需更换请新增一条并停用旧的",
					ch.Name, oldCh.Protocol, ch.Protocol))
			}
		}

		// 支付方式归一化：接受中文别名（老前端/旧配置），但**只允许枚举落库**。
		ms, dropped := payment.NormalizeMethods(ch.Methods)
		if len(dropped) > 0 {
			return badConfig(fmt.Sprintf("支付渠道「%s」的支付方式「%s」无法识别（只支持微信支付 / 支付宝）",
				ch.Name, strings.Join(dropped, "、")))
		}
		ch.Methods = payment.MethodStrings(ms)
		// 聚合支付只作兜底产物（Options 在拿不到任何具体方式时才生成），不允许运营新选：
		// 选了它收银台就会出现「微信支付」和「聚合支付」两个按钮，用户点聚合后还能改选。
		// 但存量渠道（M7 之前的单 APPID 虎皮椒）库里就是 aggregate —— 若一律拒绝，
		// 运营改一个无关参数就会锁死整个配置页保存，所以只拒「本次新引入」。
		if containsMethod(ms, payment.MethodAggregate) {
			hadAggregate := false
			if oldCh, existed := prevByID[ch.ID]; existed {
				oldMS, _ := payment.NormalizeMethods(oldCh.Methods)
				hadAggregate = containsMethod(oldMS, payment.MethodAggregate)
			}
			if !hadAggregate {
				return badConfig(fmt.Sprintf("支付渠道「%s」不能选择「聚合支付」：请按微信 / 支付宝分别新增渠道（聚合仅用于兼容历史配置）", ch.Name))
			}
		}
		if proto == payment.ProtocolXunhuPay && len(ms) > 1 {
			// 虎皮椒的微信/支付宝是两套独立 APPID，一条渠道只有一个；
			// 勾两个必然出现「选支付宝却进微信通道」。
			return badConfig(fmt.Sprintf("虎皮椒渠道「%s」只能选择一种支付方式（微信或支付宝）：请分别为两者各建一条渠道", ch.Name))
		}
		// 方式白名单：method_mode 只表达「选几个」，表达不了「能选哪些」——
		// 微信 Native 只能收微信、支付宝当面付只能收支付宝，配错必然下单失败。
		// 只校验「具体方式」：aggregate 由上面的存量兼容分支处理，不在白名单里。
		if sch, ok := payment.SchemaOf(proto); ok {
			if sch.MethodMode == payment.MethodModeSingle && len(ms) > 1 && proto != payment.ProtocolXunhuPay {
				return badConfig(fmt.Sprintf("支付渠道「%s」（%s）只能选择一种支付方式", ch.Name, sch.Label))
			}
			for _, m := range ms {
				if !m.Specific() {
					continue
				}
				if !schemaAllowsMethod(sch, m) {
					return badConfig(fmt.Sprintf("支付渠道「%s」（%s）不支持「%s」（只支持 %s）",
						ch.Name, sch.Label, m.Label(), schemaMethodLabels(sch)))
				}
			}
		}
		if ch.Enabled && len(ms) == 0 {
			// 启用却不选方式，收银台会出现一张只有渠道名的卡，用户不知道点进去是什么。
			return badConfig(fmt.Sprintf("启用中的渠道「%s」必须至少选择一种支付方式", ch.Name))
		}

		if merchant := paymentMerchantID(*ch); merchant != "" {
			key := ch.Protocol + "\x00" + merchant
			if prevID, ok := seenMerchant[key]; ok {
				return badConfig(fmt.Sprintf("渠道「%s」与「%s」使用了相同的商户标识（%s），请确认是否重复配置",
					ch.Name, prevID, merchant))
			}
			seenMerchant[key] = ch.Name
		}

		// 只校验**仍在管理台暴露**的端点。
		//
		// query_address / refund_address 已从 schema 移除（改由 pay_address 推导），
		// 运营在界面上改不了它们。若继续对它们强制 https：存量里一个 http 值就会让
		// 「连改站点名都 400」，而用户没有任何自救手段 —— 与 M7 的 aggregate 事故同型。
		// 存量非 https 值改为 Warn：不阻断保存，但日志可追溯（运维文档说明补救办法）。
		{
			addr := strings.TrimSpace(ch.Params.PayAddress)
			if addr != "" {
				if !strings.HasPrefix(strings.ToLower(addr), "https://") {
					return badConfig(fmt.Sprintf("支付渠道「%s」的网关地址必须使用 https", ch.Name))
				}
				if err := netguard.ValidateURL(addr, netguard.Policy{}); err != nil {
					return badConfig(fmt.Sprintf("支付渠道「%s」的网关地址不合法", ch.Name))
				}
			}
		}
		for _, legacy := range []struct{ field, val string }{
			{"query_address", ch.Params.QueryAddress},
			{"refund_address", ch.Params.RefundAddress},
		} {
			v := strings.TrimSpace(legacy.val)
			if v == "" || strings.HasPrefix(strings.ToLower(v), "https://") {
				continue
			}
			logger.L().Warn("渠道的入站地址仍为非 https（已从管理台移除，不再阻断保存）",
				zap.String("channel", ch.Name), zap.String("field", legacy.field), zap.String("value", v))
		}

		// 格式自洽性（PEM 可解析 / APIv3 密钥长度 / 序列号格式 / 公钥 ID 前缀）：
		// **非空即校验，与 enabled 无关**。运营把掩码改坏、把私钥粘到公钥框，
		// 保存时立刻报错，而不是等到「启用后第一笔支付失败」才发现。
		//
		// 未改动且停用的渠道放行：否则一条历史脏数据会让整页配置无法保存
		// （与 sameChannel 豁免同一套思路：新增校验只对本次变更生效）。
		unchanged := existed && sameChannel(oldCh, *ch)
		if ch.Enabled || !unchanged {
			if err := payment.ValidateChannelShape(*ch); err != nil {
				return badConfig(err.Error())
			}
		}

		if !ch.Enabled {
			continue
		}
		if unchanged {
			// 未改动的启用渠道放行：否则一条历史脏数据（种子里就有空值）会让整页配置
			// 无法保存，运营连修的机会都没有。
			//
			// 但「启用 + 必填不全」是收款链路的事故：这类渠道已被 ChannelUsable 挡在
			// 收银台之外，回调也会因为无法验签被 fail-closed 拒绝。所以保存时必须留下
			// ERROR 痕迹，让运维在日志/对账里能立刻发现，而不是以为「页面能保存 = 配置没问题」。
			if err := payment.ValidateChannelRequired(*ch); err != nil {
				logger.L().Error("已启用渠道的必填项不全：已从收银台隐藏，回调将被拒绝",
					zap.String("channel", ch.Name), zap.String("id", ch.ID), zap.Error(err))
			}
			continue
		}
		// 协议为空 = 未实现的占位（历史 wxpay / alipay）。允许保留但**不允许启用**：
		// Options() 对空协议直接 continue，启用只会让运营以为「已经能收款了」。
		// 放在 sameChannel 豁免之后：历史上本就「启用 + 空协议」的渠道改别的字段
		// 仍可保存（否则整个配置页会被一条脏数据锁死），但新开启/新启用会被拦。
		if !proto.Valid() {
			return badConfig(fmt.Sprintf("支付渠道「%s」未选择协议，不能启用（请先选择 %s）",
				ch.Name, strings.Join(payment.ProtocolNames(), " / ")))
		}
		// 必填项由 schema 的 Required 声明驱动（不再按协议写 switch）：
		// 新增协议时只改 schema，漏写必填不会被静默放过。
		if err := payment.ValidateChannelRequired(*ch); err != nil {
			return badConfig(err.Error())
		}
	}

	// reserved 键（query_address / refund_address / return_url / callback_url）
	// 已从管理台移除，运营看不到也改不了。若本次提交把它们变成空串，只可能是
	// 客户端裁剪了未显示的字段 —— 用旧值补回。ChannelParams 无 omitempty，
	// 不补就会被全量 marshal 写回成空串，且没有任何界面能让它回来。
	for i := range list {
		if oldCh, ok := prevByID[list[i].ID]; ok {
			payment.RestoreReservedParams(&list[i], oldCh)
		}
	}

	// 归一化结果必须写回 next：③ 落库与 ④ 热更新都读 next，
	// 不回写就会出现「校验用 code、库里还是中文」的错位。
	if normalized, err := json.Marshal(list); err == nil {
		next["pay_channels"] = normalized
	}

	if gdb == nil {
		return nil
	}
	for id, oldCh := range prevByID {
		if nextIDs[id] {
			continue
		}
		var n int64
		// 四类都要拦，理由都是「账还没闭上」：
		//   refunding —— 退款巡检靠 FindChannel(pay_channel_id) 查上游退款状态，
		//                渠道删了这笔退款永远查不到终态：钱已退、额度已扣，账再也平不了。
		//   expired   —— 用户可能在关单前就付了钱，回调迟到；补认领要按渠道回查上游，
		//                渠道没了就永远认不了这笔钱。
		//   pending / need_manual —— 在途原单，删除后无法查单也无法原路退款。
		if err := gdb.WithContext(ctx).Model(&model.Order{}).
			Where("pay_channel_id = ? AND status IN ?", id,
				[]string{model.OrderPending, model.OrderNeedManual, model.OrderRefunding, model.OrderExpired}).
			Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return badConfig(fmt.Sprintf("渠道「%s」仍有 %d 笔在途/退款中/已过期订单，不能删除（可改为停用）", oldCh.Name, n))
		}
		// 已支付订单允许删除（历史快照仍在订单上），但要让运营知道后果：
		// 删掉渠道后这些订单无法再走原路退款，只能「仅回退平台余额」。
		var paid int64
		if err := gdb.WithContext(ctx).Model(&model.Order{}).
			Where("pay_channel_id = ? AND status = ?", id, "paid").Count(&paid).Error; err == nil && paid > 0 {
			logger.L().Warn("删除支付渠道：该渠道仍有已支付订单，删除后只能手工退款",
				zap.String("channel", id), zap.Int64("paid_orders", paid))
		}
	}
	return nil
}

// containsMethod 判断归一化后的方式列表是否含目标项。
func containsMethod(ms []payment.Method, target payment.Method) bool {
	for _, m := range ms {
		if m == target {
			return true
		}
	}
	return false
}

// paymentMerchantID 返回渠道的商户标识（用于保存期查重）。
//
// 四套协议各自对应一个「上游认的身份」：易支付 pid / 虎皮椒 app_id /
// 微信商户号 / 支付宝应用 app_id。同一协议内两条渠道用同一个身份时，
// 回调的商户号一致性校验会把正常订单判成「不一致」转人工。
func paymentMerchantID(ch payment.Channel) string {
	switch payment.ParseProtocol(ch.Protocol) {
	case payment.ProtocolEpay:
		return strings.TrimSpace(ch.Params.PID)
	case payment.ProtocolXunhuPay:
		return strings.TrimSpace(ch.Params.AppID)
	case payment.ProtocolWxpay:
		return strings.TrimSpace(ch.Params.MchID)
	case payment.ProtocolAlipay:
		return strings.TrimSpace(ch.Params.AppID)
	}
	return ""
}

// schemaAllowsMethod 判断协议 schema 是否允许该支付方式。
func schemaAllowsMethod(sch payment.ChannelSchema, m payment.Method) bool {
	for _, allowed := range sch.Methods {
		if allowed == string(m) {
			return true
		}
	}
	return false
}

// schemaMethodLabels 把 schema 允许的方式翻成中文，用于错误文案。
func schemaMethodLabels(sch payment.ChannelSchema) string {
	labels := make([]string, 0, len(sch.Methods))
	for _, allowed := range sch.Methods {
		labels = append(labels, payment.MethodLabel(allowed))
	}
	if len(labels) == 0 {
		return "微信支付 / 支付宝"
	}
	return strings.Join(labels, " / ")
}

// sameChannel 判断两次提交的渠道是否等价（决定「只校验改动过的渠道」是否放行）。
//
// 必须包含 Enabled 与 Methods：只改支付方式（或把方式清空）也要触发必填校验，
// 否则「启用渠道一个方式都不选」这条规则可以从后门绕过去。
//
// Methods 比较的是**归一化后**的序列：库里的 wechat 与前端提交的「微信支付」
// 是同一个东西，不归一化会把「什么都没改」误判成改动，导致管理员每次保存
// 都被要求补齐种子里的空参数。
func sameChannel(a, b payment.Channel) bool {
	if a.Enabled != b.Enabled {
		return false
	}
	am, _ := payment.NormalizeMethods(a.Methods)
	bm, _ := payment.NormalizeMethods(b.Methods)
	if len(am) != len(bm) {
		return false
	}
	for i := range am {
		if am[i] != bm[i] {
			return false
		}
	}
	ja, errA := json.Marshal(a.Params)
	jb, errB := json.Marshal(b.Params)
	if errA != nil || errB != nil {
		return false
	}
	return string(ja) == string(jb)
}

package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/affcfg"
	"lingdong-api/internal/pkg/crypto"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/uploadcfg"
)

// 初始超管口令**不再写死**。
//
// 历史上这里是 "123456" 的 bcrypt 摘要，且 seedAdmin 不受 LD_SEED_ENABLED 约束 ——
// 每一个全新库（含生产）开箱即带一个口令公开的超管。现在改为：
//   - 配了 LD_BOOTSTRAP_ADMIN_PASSWORD 就用它（运维显式选择，不强制改密，避免打断脚本）；
//   - 没配就用内置出厂值 admin123，并置 must_change_password（首登强制改密）。
//
// 无论哪种来源，口令都不落仓库、不进镜像。
const bootstrapAdminEnv = "LD_BOOTSTRAP_ADMIN_PASSWORD"

// defaultBootstrapPassword 是出厂内置的管理员口令。
//
// 为什么要内置而不是随机生成：随机口令必须"被人拿到"，于是要么翻首启日志（一行，
// 极易被构建输出刷走），要么再落一个交接文件、再写一个抓取脚本、再在文档里教两遍
// 怎么找 —— 一条部署链上多出四个活动件，只为了传一个字符串。克隆即部署的代价
// 不该花在这里。
//
// 代价是"出厂口令是公开的"，用 must_change_password=true 兜住：这个口令只够进门，
// 进门第一件事就是改密（见 seedAdmin 里 must_change_password 的来源）。想跳过强制
// 改密（例如给 CI 建静态环境）就显式设 LD_BOOTSTRAP_ADMIN_PASSWORD。
const defaultBootstrapPassword = "admin123"

// Seed 写入幂等种子数据（ON CONFLICT DO NOTHING）。
//
// 分两层执行（LD_SEED_ENABLED）：
//   - **基础层**（无条件）：管理员账号 + system_config 默认值 + 默认接入应用 + modality_defaults 回填。
//   - **演示/开通层**（LD_SEED_ENABLED=1 才跑，默认开）：供应商 / 模型 / 渠道 / 绑定 / 套餐。
//
// 种子价目**直接写金额（微）**，不再写倍率：模型价格是"每百万 token 多少钱"这一
// 唯一事实，任何"倍率 × 刻度"的中间量都会让管理台回显与实际扣费分叉。
func Seed(gdb *gorm.DB) error {
	if err := seedAdmin(gdb); err != nil {
		return err
	}
	if err := seedSystemConfig(gdb); err != nil {
		return err
	}
	// 默认接入应用必须在**基础层**：生产编排默认 LD_SEED_ENABLED=0，
	// 放进演示层等于"部署完没有任何应用"，托管密钥链路在管理员手工建应用之前不可达。
	if err := seedDefaultApp(gdb); err != nil {
		return err
	}
	if conf.EnvConfig().SeedEnabled {
		if err := seedVendors(gdb); err != nil {
			return err
		}
		if err := seedModels(gdb); err != nil {
			return err
		}
		if err := seedChannels(gdb); err != nil {
			return err
		}
		if err := seedBindings(gdb); err != nil {
			return err
		}
		if err := seedPackages(gdb); err != nil {
			return err
		}
	}
	return SeedModalityDefaults(gdb)
}

// seedPackages 只在**空表**时写入默认套餐（4 档，含 10%–20% 赠送）。
//
// 为什么用 Unscoped().Count() 而不是 OnConflict{DoNothing}：
// 套餐是自增主键、没有可冲突的自然键，OnConflict 什么都拦不住，重复启动会插入重复档位；
// 而 Unscoped 计数保证「运营把套餐全删光后重启不会复活」。
//
// 到账口径（唯一）：到账 = 实付（分→微）+ 赠送（微）。没有倍率、没有刻度。
func seedPackages(gdb *gorm.DB) error {
	var n int64
	if err := gdb.Unscoped().Model(&model.Package{}).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	tiers := []struct {
		name      string
		priceCent int64
		bonusPct  int64 // 百分比
		badge     string
	}{
		{"体验包", 7200, 10, ""},
		{"标准包", 21600, 10, "recommend"},
		{"专业包", 43200, 15, ""},
		{"旗舰包", 216000, 20, ""},
	}
	rows := make([]model.Package, 0, len(tiers))
	for i, t := range tiers {
		base, err := money.FromCents(t.priceCent)
		if err != nil {
			return fmt.Errorf("db: 种子套餐金额非法: %w", err)
		}
		// 赠送额取整到 1 个货币单位：门户只展示两位小数，取整后不会出现 ¥19.9999。
		bonus, err := money.MulDiv(base, t.bonusPct, 100)
		if err != nil {
			return fmt.Errorf("db: 种子套餐赠送额计算失败: %w", err)
		}
		bonus = (bonus + money.Scale - 1) / money.Scale * money.Scale
		rows = append(rows, model.Package{
			Name: t.name, PriceCent: t.priceCent, Bonus: bonus,
			Enabled: true, SortOrder: (i + 1) * 10, Badge: t.badge,
		})
	}
	return gdb.Create(&rows).Error
}

// SeedModalityDefaults 把 models.is_default 搬迁成 system_config.modality_defaults。
//
// 为什么需要：models.is_default 是历史遗留的第二真相源，**没有写入口**，
// 而运行时判定又曾经优先读它。现在运行时只认配置；本函数只在配置缺失或全空时搬一次。
func SeedModalityDefaults(gdb *gorm.DB) error {
	var row model.SystemConfig
	err := gdb.Where("key = ?", "modality_defaults").First(&row).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("db: 读取 modality_defaults 失败: %w", err)
	}
	if err == nil {
		var configured map[string]string
		if json.Unmarshal(row.Value, &configured) == nil && len(configured) > 0 {
			return nil // 已有配置：不动
		}
	}

	// 取每个模态里 is_default=true 的模型（同模态多条时取 id ASC 首个，保证幂等）。
	var models []model.Model
	if err := gdb.Where("is_default = ? AND enabled = ?", true, true).Order("id ASC").Find(&models).Error; err != nil {
		return fmt.Errorf("db: 读取默认模型失败: %w", err)
	}
	if len(models) == 0 {
		return nil
	}
	out := make(map[string]string, len(models))
	for _, m := range models {
		if m.Modality == "" {
			continue
		}
		if _, ok := out[m.Modality]; !ok {
			out[m.Modality] = m.ID
		}
	}
	if len(out) == 0 {
		return nil
	}
	if err := gdb.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&model.SystemConfig{Key: "modality_defaults", Value: model.MustJSON(out)}).Error; err != nil {
		return fmt.Errorf("db: 回填 modality_defaults 失败: %w", err)
	}
	return nil
}

func seedAdmin(gdb *gorm.DB) error {
	now := time.Now()
	password, isFactoryDefault := bootstrapAdminPassword()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("db: 生成管理员口令摘要失败: %w", err)
	}
	// role=0 是 int16 零值，而 role 列带 default:1 —— GORM 会省略零值字段并落到 DB 默认值
	// （结果默认管理员变成普通用户），故此处用 map 显式写入。
	// aff_code 带唯一索引：必须保持 NULL，否则空串会与后续注册的用户互相冲突。
	row := map[string]any{
		"username":             "admin",
		"password_hash":        string(hash),
		"role":                 int16(0),
		"status":               int16(1),
		"group":                "default",
		"must_change_password": isFactoryDefault,
		"created_at":           now,
		"updated_at":           now,
	}
	res := gdb.Table("users").Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	if res.Error != nil {
		return fmt.Errorf("db: 种子默认管理员失败: %w", res.Error)
	}
	// 只在"真的新建了这一行"时打印。已存在时 OnConflict 不动行 —— 每次启动都报一次
	// "账号 admin 已就绪"属于噪音。出厂口令本身不进日志：它是文档里的公开值，
	// 写进日志只会在别人的 log 里多留一处"看起来像凭据"的东西。
	if res.RowsAffected > 0 {
		msg := "已创建管理员账号 admin"
		if isFactoryDefault {
			msg += "，出厂口令见 README「初始口令」，首个登录会被强制改密"
		}
		logger.L().Info(msg, zap.String("username", "admin"))
	}
	return nil
}

// defaultAppSlug 是出厂默认接入应用的标识。
//
// 必须满足 controller.appSlugPattern（小写字母/数字/连字符），否则签发端点会
// 直接 404 而管理员却能在列表里看到它 —— 那种"看得见用不了"最难排查。
const defaultAppSlug = "default"

// seedDefaultApp 写入出厂默认接入应用，保证「部署完就能接入」。
//
// 幂等判据 = slug 是否存在，**存在即跳过、一个字段都不更新**：
// `apps` 没有删除端点（下线用 status=2），所以不存在"删了又复活"；
// 剩下的风险只有"运营改过名/关了自助签发，重启被覆盖"，用"存在即 return"
// 从结构上杜绝。因此也不写 data_migrations 标记 —— 判据本身幂等。
//
// allow_self_issue 必须显式为 true：不开的话新用户得先找管理员发凭证，
// 「开箱即用」就不成立。敏感部署可在管理台关掉（只拦新签发，存量不受影响）。
//
// 用 map 插入而不是 struct：struct 的零值 false 会被 `default:true` 标签替换
// （与 apps.allow_self_issue 的零值陷阱同源）。
func seedDefaultApp(gdb *gorm.DB) error {
	var row model.App
	// 铁律：取行用 Limit(1).Find。First 的 ErrRecordNotFound 会被当成启动失败。
	if err := gdb.Where("slug = ?", defaultAppSlug).Limit(1).Find(&row).Error; err != nil {
		return fmt.Errorf("db: 查询默认接入应用失败: %w", err)
	}
	if row.ID != 0 {
		return nil
	}
	now := time.Now()
	payload := map[string]any{
		"slug":             defaultAppSlug,
		"name":             "默认应用",
		"description":      "平台预置的接入应用（标识不可改；可改名、关闭自助签发或停用）",
		"status":           model.AppStatusEnabled,
		"allow_self_issue": true,
		"created_at":       now,
		"updated_at":       now,
	}
	res := gdb.Table("apps").Clauses(clause.OnConflict{DoNothing: true}).Create(payload)
	if res.Error != nil {
		return fmt.Errorf("db: 种子默认接入应用失败: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		logger.L().Info("已创建默认接入应用", zap.String("slug", defaultAppSlug))
	}
	return nil
}

// bootstrapAdminPassword 返回初始超管口令，以及它是否取的是出厂默认值。
//
// 第二个返回值直接决定 must_change_password：出厂口令写在本仓库文档里、人人可见，
// 只够进门，进门必须先改密；运维显式指定的口令视为有意选择（CI、自动化建环境），不拦。
func bootstrapAdminPassword() (string, bool) {
	if v := strings.TrimSpace(os.Getenv(bootstrapAdminEnv)); v != "" {
		return v, false
	}
	return defaultBootstrapPassword, true
}

func seedSystemConfig(gdb *gorm.DB) error {
	rows := []model.SystemConfig{
		// 运营模式：默认开启门户（= 改造前的行为）。关闭后门户站点不可用，
		// 仅管理员可登录管理台；API 面（/api/user/*、/v1/*）不受影响。
		{Key: "portal_enabled", Value: model.MustJSON(true)},
		{Key: "site", Value: model.MustJSON(map[string]any{
			"site_name": "灵动 API", "logo_image": "", "favicon_image": "",
			// 站点地址与 API 调用地址**不预置占位域名**：未配置时门户按当前访问域名
			// 回落（见 pkg/siteurl.DisplayBases），自建部署不会把别人的域名显示给用户。
			// 填了占位域名反而更糟——它看起来像"已配置"，实际指向一个不属于自己的站。
			"site_url": "", "api_base_urls": []string{},
			"contact_email": "support@example.com",
			"footer_text":   "", "icp_beian": "",
			// 结算币种挂在 site 子树下。**有账后只读**（由 systemconfig.Update 校验）。
			// 不再有 currency_symbol：符号由币种派生，可自由编辑的符号只会与币种不一致。
			"currency": "CNY",
		})},
		// 分组折扣：微比例（1e6 = 不打折）。取代原 group_ratios（倍率）。
		{Key: "group_discount", Value: model.MustJSON(map[string]any{"default": 1000000, "vip": 900000})},
		{Key: "cny_per_usd", Value: model.MustJSON(7.2)},
		// 单用户「余额 + 到账」硬闸（微）：默认 7,200 元。
		// 它同时决定「自定义充值单笔上限」—— 到账 = 实付 + 赠送，所以单笔上限就是它本身。
		{Key: "balance_limit", Value: model.MustJSON(int64(7200) * money.Scale)},
		// 最低充值额（微）：10 元。
		//
		// 不再用 1 元：返利赠金是「5 元/笔且不超过本单实付」，1 元门槛会让赠金
		// 逐单被压到 1 元（等于「赠金 5 元」永远发不满，运营会当成 bug 报），
		// 也与 10 元的转出门槛不成比例。
		{Key: "min_topup", Value: model.MustJSON(int64(10) * money.Scale)},
		{Key: "custom_topup_enabled", Value: model.MustJSON(true)},
		// 支付渠道：默认**不预置任何渠道**。未填密钥的渠道只会给用户一个必然失败的选项，
		// 初始化路径改为「管理台 → 计费与支付 → 添加支付渠道」。
		{Key: "pay_channels", Value: model.MustJSON([]any{})},
		{Key: "retry_times", Value: model.MustJSON(3)},
		{Key: "file_retention_days", Value: model.MustJSON(7)},
		{Key: "poll_deadline_min", Value: model.MustJSON(30)},
		{Key: "logs_retention_days", Value: model.MustJSON(180)},
		{Key: "archive_retention_days", Value: model.MustJSON(365)},
		{Key: "audit_retention_days", Value: model.MustJSON(180)},
		{Key: "rate_limit_defaults", Value: model.MustJSON(map[string]any{"rpm": 60, "concurrency": 5})},
		// 单用户可保留的用户自建 KEY 上限。
		//
		// 存在的理由不是"省钱"，而是 `GET /api/user/keys` 一次返回全量：没有上限时
		// 一个脚本可以先刷几千把再拉列表，单响应几十 MB（每把 KEY 带 4 个 JSONB 数组）。
		// 缺失时按 20 回落（见 authcfg.KeyLimitPerUser），不进 requiredKeys ——
		// 老库没有这个键不该起不来。
		{Key: "key_limit_per_user", Value: model.MustJSON(20)},
		// 用量查询的默认时间窗（天）。
		//
		// `/api/user/usage` 每次请求要跑一次 `GROUP BY modality` 的全历史聚合。
		// 不传时间参数时按这个窗口收敛，否则老账号的一次首屏请求就会扫全部历史。
		// 0 表示不设默认窗（仅调试用）。
		{Key: "usage_default_window_days", Value: model.MustJSON(30)},
		// 认证策略（6 键）。默认值只有这一处来源：M33 只删旧键，不造默认值。
		//
		// 人机验证：图形验证码（5 位数字）。
		// 只有登录的策略可配；注册与找回恒为「每次都要」（代码常量，见 authcfg.Captcha）。
		// 失败阈值不在这里——唯一来源是 auth_login.fail_threshold。
		{Key: "auth_captcha", Value: model.MustJSON(map[string]any{
			"scenes": map[string]any{"login": "after_fails"},
		})},
		// 登录失败处置：达阈值后**不返回 429**，改为要求验证码 + 服务端退避延迟。
		// 429 硬锁等于给攻击者一个「锁死任意已知账号」的开关。
		{Key: "auth_login", Value: model.MustJSON(map[string]any{
			"fail_threshold": 5, "backoff_seconds": []int{60, 300, 900},
			"ip_fail_limit": 20, "ip_fail_window_sec": 600, "delay_max_ms": 800,
		})},
		{Key: "auth_session", Value: model.MustJSON(map[string]any{
			"absolute_ttl_sec": 604800, "idle_ttl_sec": 43200,
			"max_active": 10, "touch_interval_sec": 300,
		})},
		// contact_verify 默认 off：email.enabled 出厂为 false，若默认 required
		// 则「校验通过才允许启动」会让全新部署直接起不来。要邮箱验证时
		// 先配好 SMTP、打开 email.enabled，再把它改成 required（保存期会校验渠道就绪）。
		{Key: "auth_register", Value: model.MustJSON(map[string]any{
			"mode": "open", "contact_verify": "off", "contact_channels": []string{"email"},
		})},
		{Key: "auth_mfa", Value: model.MustJSON(map[string]any{"policy": "optional"})},
		{Key: "auth_event_retention_days", Value: model.MustJSON(90)},
		// active=none 出厂值 = 写本地磁盘（显式选择，不是缺省回落）。
		// 缺这个键时写路径会直接 503，绝不静默落本地 —— 见 storage.ErrNotConfigured。
		{Key: "storage", Value: model.MustJSON(map[string]any{
			"active": "none",
			"channels": map[string]any{
				"s3": map[string]any{}, "oss": map[string]any{}, "cos": map[string]any{},
				"minio": map[string]any{}, "none": map[string]any{},
			},
		})},
		// 出厂值只在 uploadcfg.Defaults() 一处维护：这里再抄一遍字段清单，
		// 加字段时必然漏掉一处（症状是「保存成功但重启后回退」）。
		{Key: "uploads", Value: model.MustJSON(uploadcfg.Defaults())},
		{Key: "email", Value: model.MustJSON(map[string]any{
			"enabled": false, "smtp_host": "", "smtp_port": 465, "encryption": "ssl",
			"username": "", "password": "", "from_addr": "", "sender_name": "灵动 API",
			"scenes":    map[string]any{"task_done": true, "low_balance": true, "price_change": false},
			"templates": map[string]any{"task_done": map[string]any{}, "low_balance": map[string]any{}, "price_change": map[string]any{}, "reset_password": map[string]any{}},
		})},
		{Key: "sms", Value: model.MustJSON(map[string]any{
			"enabled": false, "active": "aliyun", "daily_limit": 0,
			"channels": map[string]any{"aliyun": map[string]any{}, "tencent": map[string]any{}},
		})},
		// 邀请返利：出厂值只有一处定义（pkg/affcfg.Defaults），这里不再手写一份。
		//
		// 曾经 seed 与迁移各写一份 map，键名靠人眼对齐；只要有一处写歪，
		// 表现就是「保存能过、读取恒为 0」。默认值刻意保守（10% × 前 3 笔 +
		// 每笔最多 5 元）：返利配错是持续性负毛利，要抬高必须来管理台改。
		{Key: "aff", Value: model.MustJSON(affcfg.Defaults())},
		{Key: "monitor", Value: model.MustJSON(map[string]any{
			"auto_disable": true, "fail_threshold": 0.5, "fail_window": 10, "auto_enable": true, "probe_interval_min": 5,
		})},
		{Key: "announcements", Value: model.MustJSON(map[string]any{
			"enabled": true,
			"items": []any{
				map[string]any{"id": 3, "type": "success", "content": "全新计费体系已全量生效：价格即金额，所见即所得。", "publishDate": "2026-10-10T09:00:00+08:00", "extra": ""},
				map[string]any{"id": 2, "type": "ongoing", "content": "可灵 2.0 master 已接入并开放 `1080p` 规格。", "publishDate": "2026-10-06T14:30:00+08:00", "extra": ""},
				map[string]any{"id": 1, "type": "default", "content": "价目数据源接入完成，模型价格已同步最新版本。", "publishDate": "2026-10-01T08:00:00+08:00", "extra": ""},
			},
		})},
		{Key: "about_content", Value: model.MustJSON("灵动 API 是面向开发者的多模态模型接入网关，提供对话、图像、音频、视频等全模态模型的统一 API。\n\n我们通过统一的 OpenAI 兼容接口接入多家模型供应商，按实际用量计费，价格透明、账单可查，支持余额充值与兑换码。\n\n如需商务合作或大额采购，请联系 support@example.com。")},
		{Key: "agreement_content", Value: model.MustJSON("一、服务说明：灵动 API（以下简称「本服务」）为开发者提供模型调用网关服务。注册即视为已阅读并同意本协议。\n\n二、账号与安全：用户应妥善保管账户与 API KEY，因保管不善造成的损失由用户承担。禁止利用本服务从事违法违规用途。\n\n三、计费与退款：按实际调用量计费，充值余额不支持提现；因系统原因导致的错误计费将予以退回。\n\n四、免责条款：模型输出内容不构成平台立场，平台对生成内容不承担审核义务。本协议最终解释权归平台所有。\n\n生效日期：2026-10-01")},
		// 发布清单地址（一键更新的"有没有新版本"来源）。默认空 = 不检查，
		// 由管理员在「系统配置 → 通用」里填；地址变了缓存自然失效（缓存键就是它）。
		{Key: "release_manifest_url", Value: model.MustJSON("")},
		{Key: "modality_defaults", Value: model.MustJSON(map[string]any{
			"chat": "gpt-4o", "image": "gpt-image-1", "audio": "tts-1-hd", "video": "kling-v2-master",
			"embedding": "text-embedding-3-large", "rerank": "qwen3-reranker", "moderation": "omni-moderation",
		})},
	}
	if err := gdb.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
		return fmt.Errorf("db: 种子 system_config 失败: %w", err)
	}
	return nil
}

// seedVendors 写入出厂供应商分组。
//
// **注意 OnConflict{DoNothing}**：已存在的行不会更新。所以：
//   - 首次建库 → 出厂图标随 seed 落库；
//   - 已跑过的库 → 想拿到新的出厂图标只能重置开发库（本项目无存量用户，
//     刻意不写回填迁移；写回填就得决定"覆盖不覆盖运营改过的值"，那个判断
//     本身就是新的坑）。
//
// 图标 key 与 internal/assets/brandicon/catalog.go 一一对应（embed_test 守）；
// 「其他」不给图标，让它显示首字 —— 它本来就不是一个品牌。
func seedVendors(gdb *gorm.DB) error {
	vendors := []model.Vendor{
		{ID: "openai", Name: "OpenAI", Color: "#10A37F", Icon: "builtin:openai-gpt", Builtin: true, Sort: 1},
		{ID: "anthropic", Name: "Anthropic", Color: "#D97757", Icon: "builtin:anthropic-claude", Builtin: true, Sort: 2},
		{ID: "google", Name: "Google", Color: "#4285F4", Icon: "builtin:google-gemini", Builtin: true, Sort: 3},
		{ID: "deepseek", Name: "DeepSeek", Color: "#4D6BFE", Icon: "builtin:deepseek", Builtin: true, Sort: 4},
		{ID: "alibaba", Name: "阿里通义", Color: "#FF7A45", Icon: "builtin:alibaba-qwen", Builtin: true, Sort: 5},
		{ID: "bytedance", Name: "字节豆包", Color: "#325AB4", Icon: "builtin:bytedance-doubao", Builtin: true, Sort: 6},
		{ID: "zhipu", Name: "智谱", Color: "#3859FF", Icon: "builtin:zhipu-glm", Builtin: true, Sort: 7},
		{ID: "moonshot", Name: "月之暗面", Color: "#7C7C7C", Icon: "builtin:moonshot-kimi", Builtin: true, Sort: 8},
		{ID: "minimax", Name: "MiniMax", Color: "#F23F5D", Icon: "builtin:minimax", Builtin: true, Sort: 9},
		{ID: "xai", Name: "xAI", Color: "#8E8E93", Icon: "builtin:xai-grok", Builtin: true, Sort: 10},
		{ID: "kling", Name: "可灵 AI", Color: "#F59E0B", Icon: "builtin:kuaishou-kling", Builtin: true, Sort: 11},
		{ID: "vidu", Name: "Vidu", Color: "#8B5CF6", Icon: "builtin:shengshu-vidu", Builtin: true, Sort: 12},
		{ID: "runway", Name: "Runway", Color: "#00E599", Icon: "builtin:runway", Builtin: true, Sort: 13},
		{ID: "suno", Name: "Suno", Color: "#E11D48", Icon: "builtin:suno", Builtin: true, Sort: 14},
		{ID: "other", Name: "其他", Color: "#64748B", Builtin: false, Sort: 99},
	}
	if err := gdb.Clauses(clause.OnConflict{DoNothing: true}).Create(&vendors).Error; err != nil {
		return fmt.Errorf("db: 种子 vendors 失败: %w", err)
	}
	return nil
}

// p 把「元」写成微，仅供种子数据使用（价格写错会在启动时 panic，而不是上线后算错钱）。
func p(yuan string) *int64 {
	v := money.MustParse(yuan)
	return &v
}

// specPrices 把结构化价目写成 JSONB 值（走 model.SpecPrices 的唯一键集）。
func specPrices(v model.SpecPrices) model.JSON { return model.MustJSON(v) }

func seedModels(gdb *gorm.DB) error {
	rows := []model.Model{
		{ID: "gpt-4o", DisplayName: "GPT-4o", Vendor: "openai", Modality: "chat",
			BillingMode: model.BillingModeTokens, Enabled: true, IsDefault: true,
			Prices:    model.Prices{Input: p("18.00"), Output: p("72.00"), Cache: p("9.00")},
			Endpoints: model.MustJSON([]string{"chat/completions", "responses"})},
		{ID: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5", Vendor: "anthropic", Modality: "chat",
			BillingMode: model.BillingModeTokens, Enabled: true,
			Prices: model.Prices{Input: p("21.60"), Output: p("108.00"), Cache: p("2.16"), CacheCreate: p("27.00")},
			// /v1/messages 是**协议变体**（EndpointKey 为空，见 apidoc/kind.go），
			// 不是模型能声明的能力键 —— 声明它会被判定为未知端点。
			Endpoints: model.MustJSON([]string{"chat/completions"})},
		{ID: "deepseek-v3.2", DisplayName: "DeepSeek V3.2", Vendor: "deepseek", Modality: "chat",
			BillingMode: model.BillingModeTokens, Enabled: true,
			Prices:    model.Prices{Input: p("2.00"), Output: p("8.00"), Cache: p("0.50")},
			Endpoints: model.MustJSON([]string{"chat/completions"})},
		{ID: "text-embedding-3-large", DisplayName: "Embedding 3L", Vendor: "openai", Modality: "embedding",
			BillingMode: model.BillingModeTokens, Enabled: true,
			Prices:    model.Prices{Input: p("0.94")},
			Endpoints: model.MustJSON([]string{"embeddings"})},
		{ID: "kling-v2-master", DisplayName: "可灵 2.0 master", Vendor: "kling", Modality: "video",
			BillingMode: model.BillingModeSpec, Enabled: true, IsDefault: true,
			SpecPrices: model.SpecPrices{
				Kind:          "per_video",
				SecondPrice:   map[string]int64{"480p": money.MustParse("0.96"), "720p": money.MustParse("1.20"), "1080p": money.MustParse("1.80")},
				RefImagePrice: p("1.50"), RefVideoPrice: p("4.00"), RefAudioPrice: p("2.50"),
			},
			Endpoints: model.MustJSON([]string{"video/generations"})},
		{ID: "tts-1-hd", DisplayName: "TTS HD", Vendor: "openai", Modality: "audio",
			BillingMode: model.BillingModeCall, Enabled: true, IsDefault: true,
			Prices:    model.Prices{Call: p("0.32")},
			Endpoints: model.MustJSON([]string{"audio/speech"})},
		{ID: "gpt-image-1", DisplayName: "GPT Image 1", Vendor: "openai", Modality: "image",
			BillingMode: model.BillingModeSpec, Enabled: true,
			// 只配 1K 一档：OpenAI 图片接口只有 1024x1024 / 1536x1024 / 1024x1536 三个枚举，
			// 根本出不了 2K / 4K。之前配了 2K/4K 价，等于卖一个上游交付不了的档位 ——
			// 用户付 2K 的钱，要么被上游 400，要么拿到 1024 的图。
			SpecPrices: model.SpecPrices{
				Kind:            "per_image",
				TierPrice:       map[string]int64{"1K": money.MustParse("0.60")},
				ResolutionFixed: "1K", MaxN: iptr(10),
			},
			Endpoints: model.MustJSON([]string{"images/generations"})},
		{ID: "whisper-1", DisplayName: "Whisper 语音转写", Vendor: "openai", Modality: "audio",
			BillingMode: model.BillingModeCall, Enabled: true,
			Prices:    model.Prices{Call: p("0.09")},
			Endpoints: model.MustJSON([]string{"audio/transcriptions"})},
		{ID: "qwen3-reranker", DisplayName: "Qwen3 Reranker", Vendor: "alibaba", Modality: "rerank",
			BillingMode: model.BillingModeTokens, Enabled: true, IsDefault: true,
			Prices:    model.Prices{Input: p("0.72")},
			Endpoints: model.MustJSON([]string{"rerank"})},
		{ID: "omni-moderation", DisplayName: "Omni Moderation", Vendor: "openai", Modality: "moderation",
			BillingMode: model.BillingModeCall, Enabled: true, IsDefault: true,
			Prices:    model.Prices{Call: p("0.02")},
			Endpoints: model.MustJSON([]string{"moderations"})},
	}
	for i := range rows {
		rows[i].IsFree = rows[i].PricingIsFree()
	}
	if err := gdb.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
		return fmt.Errorf("db: 种子 models 失败: %w", err)
	}
	return nil
}

// seedChannels 只在**空表**时写入演示渠道。
//
// 为什么必须用空表判断而不是 OnConflict{DoNothing}：
// channels 是自增主键、除主键外**没有任何唯一索引**，OnConflict 什么都拦不住 ——
// 重复启动会把渠道再插一遍，选路按 weight 随机派发，限流/熔断/健康度全部失真。
func seedChannels(gdb *gorm.DB) error {
	var n int64
	if err := gdb.Unscoped().Model(&model.Channel{}).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	channels := []model.Channel{
		{Name: "OpenAI 官方", AdapterID: "openai", BaseURL: "https://api.openai.com",
			Icon: "builtin:openai-gpt",
			Keys: fernetKeys("sk-demo-openai-key"), MultiKeyMode: "polling",
			Groups: model.MustJSON([]string{"default", "vip"}), Weight: 10, Priority: 1,
			MaxConcurrency: 50, RPMLimit: 500, Status: 1, AutoBan: true},
		// 火山方舟（Ark）**不在这里**：它的数据面在 `/api/v3`，而 openai 适配器打的是
		// `/v1` 前缀，配一条 base 到主域的 demo 渠道只会得到 100% 的 404。
		// 它由内置预设层写入（`seed_builtin_ark.go`，停用 + 空密钥，等运营填 KEY）。
	}
	return gdb.Create(&channels).Error
}

// fernetKeys 把演示密钥加密为 Fernet token 数组（与 channels.keys 存储体例一致）。
func fernetKeys(plain ...string) model.JSON {
	master := crypto.FernetDerive(conf.EnvConfig().AppEncryptionKey)
	tokens := make([]string, 0, len(plain))
	for _, k := range plain {
		if t, err := crypto.FernetEncrypt(master, []byte(k)); err == nil {
			tokens = append(tokens, t)
		}
	}
	return model.MustJSON(tokens)
}

// seedBindingSpec 描述一条演示绑定：模型 → 哪个渠道（按 adapter_id + base_url 定位）。
//
// 为什么不直接写 channel_id：自增主键不可预测。硬编码 1/2 时，
// 任何一次「删过渠道再重建」都会让绑定指向不存在或不相干的渠道，
// 而表象是「模型广场有模型、调用 404 无可用渠道」。
type seedBindingSpec struct {
	modelID   string
	adapterID string
	baseURL   string
	upstream  string
	weight    int
	priority  int
}

func seedBindings(gdb *gorm.DB) error {
	specs := []seedBindingSpec{
		{modelID: "gpt-4o", adapterID: "openai", baseURL: "https://api.openai.com", weight: 10, priority: 1},
		{modelID: "text-embedding-3-large", adapterID: "openai", baseURL: "https://api.openai.com", weight: 5, priority: 1},
		{modelID: "tts-1-hd", adapterID: "openai", baseURL: "https://api.openai.com", weight: 5, priority: 1},
		// 异步视频：当前仅 openai 适配器实现 SubmitTask/PollTask，故视频模型绑定到 OpenAI 兼容渠道。
		{modelID: "kling-v2-master", adapterID: "openai", baseURL: "https://api.openai.com",
			upstream: "kling-v2-master", weight: 5, priority: 1},
		{modelID: "gpt-image-1", adapterID: "openai", baseURL: "https://api.openai.com", weight: 5, priority: 1},
		{modelID: "whisper-1", adapterID: "openai", baseURL: "https://api.openai.com", weight: 5, priority: 1},
		{modelID: "qwen3-reranker", adapterID: "openai", baseURL: "https://api.openai.com",
			upstream: "qwen3-reranker", weight: 5, priority: 1},
		{modelID: "omni-moderation", adapterID: "openai", baseURL: "https://api.openai.com", weight: 5, priority: 1},
	}

	// 渠道与模型都可能已被运营删除（演示数据本来就该能删干净），
	// 因此逐条「能解析才写」：解析不到就跳过，绝不留孤儿绑定。
	channels := make([]model.Channel, 0, 4)
	if err := gdb.Select("id", "adapter_id", "base_url").Find(&channels).Error; err != nil {
		return err
	}
	channelID := func(adapterID, baseURL string) uint64 {
		for i := range channels {
			if channels[i].AdapterID == adapterID && strings.TrimRight(channels[i].BaseURL, "/") == strings.TrimRight(baseURL, "/") {
				return channels[i].ID
			}
		}
		return 0
	}

	rows := make([]model.ModelBinding, 0, len(specs))
	for _, s := range specs {
		cid := channelID(s.adapterID, s.baseURL)
		if cid == 0 {
			continue
		}
		var n int64
		if err := gdb.Model(&model.Model{}).Where("id = ?", s.modelID).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		rows = append(rows, model.ModelBinding{
			ModelID: s.modelID, ChannelID: cid, UpstreamModel: s.upstream,
			Weight: s.weight, Priority: s.priority, Enabled: true,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	if err := gdb.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
		return fmt.Errorf("db: 种子 model_bindings 失败: %w", err)
	}
	return nil
}

func iptr(v int64) *int64 { return &v }

package model

import "time"

// App 对应 apps 表：一个「接入应用」。
//
// 语义边界（改之前先读）：应用不是租户。
//   - 模型、渠道、供应商、价目、套餐 **全平台一套**，应用之间共享；
//   - 按应用隔离的是：API 密钥（api_keys.app_id）、订单（orders.app_id）、
//     以及由密钥派生的调用记录 / 任务 / 文件（归因链 usages.key_id -> api_keys.app_id）；
//   - 用户池 **全局共享**：同一个用户可以同时在多个应用下持有托管密钥，
//     所以他不会有"某个应用下的账号"这种身份，只有"某个应用下的密钥"。
//
// 刻意**没有 DeletedAt**：应用是长期实体，删除会留下孤儿引用
// （api_keys.app_id、orders.app_id 指向已消失的行，join 拿不到名字，
// 而账单页必须显示"这笔充值属于哪个应用"）。
// 下线手段是 Status=2（停用）：停用后新的托管密钥签发被拒，
// 已有密钥仍可用 —— 要彻底断掉需要管理员逐个禁用密钥，这是刻意的两步操作：
// 停应用影响的是"新接入"，停密钥影响的是"正在跑的流量"，不该用一次点击同时完成。
type App struct {
	ID uint64 `gorm:"primaryKey"`
	// Slug 是对外的稳定标识（接入方按它签发密钥：POST /api/user/apps/{slug}/hosted-key）。
	// 一旦对外发布就不再改：改名等于全部接入方失效。唯一性由
	// db/indexes.go 的 uq_apps_slug 保证（不用 gorm uniqueIndex tag，见项目铁律）。
	Slug string `gorm:"size:32;not null"`
	Name string `gorm:"size:64;not null"`
	// Description 只用于后台列表的辨识，不对接入方展示。
	Description string `gorm:"size:255;not null;default:''"`
	// Status：1 启用 / 2 停用。带 default 标签，插入零值会被替换成 1 ——
	// 而 0 不是合法值，创建入口会先校验拒绝，所以不存在"想存 0 却变成 1"的静默错误。
	Status int16 `gorm:"not null;default:1"`
	// AllowSelfIssue 控制「任意登录用户都能为自己的应用签发托管密钥」。
	//
	// 默认 true = 保持既有行为（自助接入），只有敏感应用才由管理员关掉。
	// 关掉后签发端点返回 403 并提示走运营接入码 —— 校验放在 controller 而不是
	// apikey.CreateHosted：那个函数还被轮换路径复用，轮换不该被这个开关拦住
	// （已有密钥的应用必须能换新，否则开关一开就断服）。
	//
	// **注意 GORM 的零值陷阱**：本列带 default:true，Create 传零值 false 会被
	// 替换成列默认值 true。要建一个「关闭自助签发」的应用，必须在插入后用
	// Update 显式置 false（或用 map 插入），否则开关静默失效。
	AllowSelfIssue bool `gorm:"not null;default:true"`
	// ---- 对外品牌（终端应用可见；Name/Description 仍是后台辨识用）----
	//
	// 为什么与 Name 分开：Name 是"给运营看的"，改了不影响接入方；
	// DisplayName 是"给终端用户看的"，改动等于换品牌。合成一个字段后，
	// 运营改个后台辨识名就会把 App 里的品牌名一起改掉。
	//
	// 全部 not null default ''：零值恰好等于默认值，不存在 GORM 零值陷阱。
	DisplayName string `gorm:"size:64;not null;default:''"`
	// Tagline 是一句话标语（终端可能展示在启动页）。
	Tagline string `gorm:"size:120;not null;default:''"`
	// Intro 是较长简介（纯文本，不解析 Markdown）。
	Intro string `gorm:"type:text;not null;default:''"`
	// LogoIcon 存**图标令牌**（upload:<ulid> / builtin:<key>），不存 URL：
	// 换存储后端/换域名时令牌不变，URL 由 iconref 按当前站点派生。
	LogoIcon string `gorm:"size:64;not null;default:''"`
	// ContactEmail 等属于"接入方对终端用户的客服信息"，不是平台账号的联系方式。
	ContactName  string `gorm:"size:64;not null;default:''"`
	ContactEmail string `gorm:"size:128;not null;default:''"`
	SupportURL   string `gorm:"size:512;not null;default:''"`
	PrivacyURL   string `gorm:"size:512;not null;default:''"`
	TermsURL     string `gorm:"size:512;not null;default:''"`
	// ---- 维护模式（与 Status 正交）----
	//
	// Status=2（停用）只拦"新签发"，存量密钥照跑（既有语义，不得改）；
	// MaintenanceMode 只拦"存量流量"（/v1 返回 503），不影响签发。
	// 两者不联动：一次点击不该同时完成"断新接入"与"断在线流量"。
	MaintenanceMode bool `gorm:"not null;default:false"`
	// MaintenanceMessage 是对外提示文案（终端展示），空串时用默认文案。
	// 必须与 MaintenanceMode 同表：它是 TokenAuth 热路径的判定输入，
	// 分两张表意味着热路径多一次查询或两处缓存。
	MaintenanceMessage string    `gorm:"size:200;not null;default:''"`
	CreatedAt          time.Time `gorm:"type:timestamptz"`
	UpdatedAt          time.Time `gorm:"type:timestamptz"`
}

// TableName 固定表名（不依赖 GORM 的复数推断）。
func (App) TableName() string { return "apps" }

// 应用状态常量。
const (
	// AppStatusEnabled 表示应用可用（可签发托管密钥、可被接入方读取）。
	AppStatusEnabled int16 = 1
	// AppStatusDisabled 表示应用已停用（拒绝新签发，已签发的密钥仍可用）。
	AppStatusDisabled int16 = 2
)

package model

import "time"

// 应用插件目录状态。下架（2）只拦"新安装/新写入"，已安装的读与导出照常 ——
// 下架是"不再推荐"，不是"停服"（与 apps.status 的停用语义同源的克制）。
const (
	AppPluginStatusEnabled  int16 = 1
	AppPluginStatusDisabled int16 = 2
)

// 应用插件安装状态。三态必须分开：
//   - 2 停用：可读不可写，用于"先冻结再排查"；
//   - 3 已卸载：读写都拒，但**行保留** —— 否则「重新启用 / 清除数据」的入口会随行一起消失，
//     而数据其实还在库里（卸载刻意不删记录，见 service/appplugins/install.go）。
const (
	AppPluginInstallEnabled  int16 = 1
	AppPluginInstallDisabled int16 = 2
	AppPluginInstallRemoved  int16 = 3
)

// AppPlugin 是**全局插件目录**：描述"这个插件能存什么、能怎么展示"。
//
// 为什么必须单独一张表，而不是只靠安装记录反推：
// 同一份 manifest 会被 N 个应用安装，从 installs 反推会得到 N 份副本，
// 且"有哪些插件可装"取决于"谁装过"——这正是 docs/规格适配器与非标接入.md:104-106
// 记下的错误（拿运行态当清单）。目录只有一份，安装只是引用它。
//
// 列设计的三条边界（与 adapter_specs 同族）：
//  1. **Manifest 是唯一真相**。Key/Name/Version/APIVersion 是从它投影出来的
//     （列表要能排序、唯一索引要可索引），写入只经 service/appplugins 一个入口，
//     投影与原文永远同批写。
//  2. **Key 创建后不可改**：它进 URL（/admin/apps/:id/p/:key）与安装记录，
//     改名等于让全部引用一夜失效（同 Directus 的 collection name immutable）。
//  3. **不执行任何第三方代码**：manifest 里不存在可执行/可外联字段，
//     这是"AI 生成也伪造不了能力"的根（项目在 2026-10-25 下线过执行型插件系统）。
type AppPlugin struct {
	ID uint64 `gorm:"primaryKey"`
	// Key 语法 ^[a-z][a-z0-9-]{1,31}$，唯一性由 db/indexes.go 的 uq_app_plugins_key 保证。
	Key string `gorm:"size:32;not null"`
	// Name / Version / APIVersion 是 Manifest 的投影列。
	//
	// Version 只做展示与 drift 比较，**不比较大小**（同 app_releases.version：
	// "1.10.0 比 1.9.0 新"是语义承诺，本系统不做，也不靠它拦降级）。
	Name       string `gorm:"size:64;not null"`
	Version    string `gorm:"size:32;not null"`
	APIVersion int16  `gorm:"not null;default:1"`
	// Manifest 是插件能力全文（结构见 service/appplugins.Manifest）。
	// 保存前必须过 Decode + Validate；读取时从库里再 Decode 一次（双保险）。
	Manifest JSON `gorm:"type:jsonb"`
	// Status 见 AppPluginStatus* 常量。
	Status int16 `gorm:"not null;default:1"`
	// Source 记来源：import（管理员/AI 导入）。刻意**没有** seed ——
	// 出厂示例只在 GET /app-plugins/sample 里返回，不落库：
	// 落库后会变成"运营删了重启又回来"，而 v7_seed 的规则是"只插不改"。
	Source string `gorm:"size:16;not null;default:'import'"`
	// LockVersion 是导入的乐观锁（不能叫 Version：那个名字已被 manifest 版本占用）。
	// 两个 PUT（AI 导入 vs 运营微调）必须有一个拿到 409，否则最后写入者静默覆盖。
	LockVersion int       `gorm:"not null;default:1"`
	UpdatedBy   uint64    `gorm:"not null;default:0"`
	CreatedAt   time.Time `gorm:"type:timestamptz"`
	UpdatedAt   time.Time `gorm:"type:timestamptz"`
}

func (AppPlugin) TableName() string { return "app_plugins" }

// AppPluginInstall 是「某应用安装了某插件」这一事实，也是该应用下插件配置的唯一来源。
//
// 唯一性由 uq_plugin_installs_app_key 保证：(app_id, plugin_key) 只能一行。
// 不用"查不到行 = 未安装"表达状态：卸载后行要留着，否则清除数据的入口会消失。
type AppPluginInstall struct {
	ID        uint64 `gorm:"primaryKey"`
	AppID     uint64 `gorm:"not null"`
	PluginKey string `gorm:"size:32;not null"`
	// PluginVersion 是安装当时目录的版本快照，**只用于漂移提示**（目录升级后
	// 页面顶部出"插件已升级，当前数据按旧版写入"横幅）。
	// 能力一律读目录表：把它当第二真相源就会出现"install 说 1.0、实际行为是 1.1"。
	PluginVersion string `gorm:"size:32;not null;default:''"`
	// Status 见 AppPluginInstall* 常量。
	Status int16 `gorm:"not null;default:1"`
	// Config 是插件级配置（manifest 的 config[]），必须与 Status 同事务写：
	// 启用时配置不合法 = 拒绝启用，不能出现"启用着但配置缺项"的中间态。
	Config JSON `gorm:"type:jsonb"`
	// PublicEnabled 决定该应用下这个插件的数据能否被**免凭证**读取
	// （GET /api/apps/:slug/p/:plugin/:entity）。
	//
	// 默认 false 且必须在管理台显式打开：免凭证面一旦开出去就没有"谁看过"的记录。
	// 它只是两道闸之一 —— 另一道是 manifest 的 entities[].public 字段白名单，
	// 两者同时成立才真的下发数据；只开这里、manifest 没声明 = 什么都不公开。
	PublicEnabled bool      `gorm:"not null;default:false"`
	LockVersion   int       `gorm:"not null;default:1"`
	CreatedAt     time.Time `gorm:"type:timestamptz"`
	UpdatedAt     time.Time `gorm:"type:timestamptz"`
}

func (AppPluginInstall) TableName() string { return "app_plugin_installs" }

// AppPluginRecord 是插件的一条业务记录。
//
// 为什么所有插件共用一张表：manifest 由导入方决定，每插件建表就是运行期 DDL，
// 会让 db/indexes.go 的静态索引清单与 verifyIndexes 失效（缺索引没人知道）。
// 代价没有跨字段范围索引，用"单实体 >5 万行 / 需 ≥2 字段范围查询就不是插件"挡住。
//
// 两个身份必须分开（这是本表最容易写错的地方）：
//   - RecordKey 恒为服务端生成的 ULID：它进 URL 与审计 target_id，
//     若用业务唯一值当主键，值里出现 `/`、空格、中文就无法进 gin 的 `:rk` 段，
//     而 admin_audit.target_id 只有 64 字符。
//   - UniqueNorm 只在 entity 声明了 unique 字段时写入（TrimSpace + ToLower），
//     承担业务唯一性；不声明时为 NULL，Postgres 默认 NULLS DISTINCT，
//     所以"没有唯一字段的实体"彼此不冲突。
type AppPluginRecord struct {
	ID        uint64 `gorm:"primaryKey"`
	AppID     uint64 `gorm:"not null"`
	PluginKey string `gorm:"size:32;not null"`
	Entity    string `gorm:"size:32;not null"`
	RecordKey string `gorm:"size:32;not null"`
	// UniqueNorm 可空；指针是为了让 NULL 与空串在被比较时不同。
	UniqueNorm *string `gorm:"size:64"`
	// Data 只含 manifest 声明的字段，写入前重新 marshal（"存的字节 = 理解的字节"）。
	// 升级删字段后，存量行里会留下未声明键：读取按新 manifest 投影，不显示不导出，
	// 也不批量清洗（清洗会重写整表且丢掉可回滚性）。
	Data        JSON      `gorm:"type:jsonb"`
	LockVersion int       `gorm:"not null;default:1"`
	CreatedAt   time.Time `gorm:"type:timestamptz"`
	UpdatedAt   time.Time `gorm:"type:timestamptz"`
}

func (AppPluginRecord) TableName() string { return "app_plugin_records" }

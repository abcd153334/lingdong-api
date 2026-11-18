package model

import (
	"time"

	"gorm.io/gorm"
)

// 计费方式（models.billing_mode）。替代历史的 quota_type 0/1/2。
//
// 用字符串而不是整数：整数在日志与 DTO 里全是"2"，事后无法一眼看出是按规格，
// 而每次排查计费都先要确认"这是哪种计费方式"。
const (
	BillingModeTokens = "tokens" // 按 token 用量计价（8 条 lane）
	BillingModeCall   = "call"   // 按次固定价
	BillingModeSpec   = "spec"   // 按规格（图片档位 / 视频秒价 / 任务固定价）
)

// Model 对应 models 表。
type Model struct {
	ID          string `gorm:"column:id;size:64;primaryKey"`
	DisplayName string `gorm:"column:display_name;size:128"`
	Description string `gorm:"column:description;type:text"`
	Vendor      string `gorm:"column:vendor;size:32;index"`
	Modality    string `gorm:"column:modality;size:16"` // chat/image/audio/video/music/embedding/rerank/moderation

	// Icon 是**模型自己设的**图标 token："" | "builtin:<key>" | "upload:<ulid>"。
	//
	// 只存"自己设的"，不存派生结果：为空时展示层回落到所属供应商的图标
	// （派生唯一实现 brandicon.Effective）。若把回落结果写进本列，
	// 换供应商图标时旧模型不会跟着变，且"到底是谁的图标"再也分不清。
	//
	// **必须带 column tag**：modelcfg.Updates 用反射 + gormColumnOf 生成列清单，
	// 没有显式 column 的字段会被静默忽略 —— 表现是"保存成功但图标永远不生效"。
	Icon string `gorm:"column:icon;size:255;not null;default:''"`

	// BillingMode 是计费方式（tokens / call / spec）。唯一的计费分派依据。
	BillingMode string `gorm:"column:billing_mode;size:8;not null;default:tokens"`

	// Prices 是 9 个平铺价格列（微 / 百万 tokens 或 微 / 次）。
	//
	// 匿名内嵌 + 本结构体内每个字段都带显式 column tag：列名唯一来源就是
	// model.Prices 的 gorm tag，写库列清单、归档列清单、改价审计载荷全部由它反射生成。
	// 历史教训：价格字段曾有 6 份副本，任何一处漏改都表现为"保存成功但价格不生效"。
	Prices

	// SpecPrices 是按规格计价的价目（per_image / per_video / per_task）。
	// 结构体即键集：新增价目键必须加到 model.SpecPrices，否则严格解码会直接拒绝。
	SpecPrices SpecPrices `gorm:"column:spec_prices;type:jsonb"`

	Enabled   bool `gorm:"column:enabled;not null;default:true"`
	IsDefault bool `gorm:"column:is_default;not null;default:false"`
	// Capabilities 是模型能力声明（apidoc.Capabilities() 的封闭词表，如 t2i/i2v/ref_audio）。
	//
	// 为什么是模型事实而不是文档文案：文档的「能力预设」、签名分组、渠道能力校验
	// 必须读同一份声明；写进文档层会让「模型支持什么」变成第二份、可被文案改写的真相。
	// NULL = 未声明（文档按模态给最小兜底）；[] = 明确声明无额外能力。
	Capabilities JSON `gorm:"column:capabilities;type:jsonb"`
	// Tags 是营销标签（最多 TagMaxCount 个，数组顺序即展示顺序）。
	//
	// 与 Capabilities 的区别：能力是**事实**（决定路由/文档 TAB），标签是**文案**
	// （只影响展示）。过期项保留在库里、只在读出口过滤 —— 运营要能续期，
	// 而不是每次重新建一遍。详见 model_tag.go 的文件头。
	Tags JSON `gorm:"column:tags;type:jsonb"`
	// IsFree 是「价格全为 0」的派生事实（保存期由 PricingIsFree 唯一派生）。
	// 它只是展示投影（门户徽标 / 管理台回显），运行期计费只看价格数值本身，绝不读它。
	IsFree    bool `gorm:"column:is_free;not null;default:false"`
	Endpoints JSON `gorm:"column:endpoints;type:jsonb"`
	// ContextWindow 是该模型的**真实上下文窗口**（token），0 = 继承 agent_settings.context_window。
	//
	// 为什么放在模型上而不是只放全局：8k 的本地模型与 1M 的旗舰模型共用一套默认值，
	// 前者每一轮都会被装配到 2M 然后被上游 400 打回，后者永远只用得到 1/20 的窗口。
	// 0 是"未声明"，不是"无限"—— 装配时按「模型 > 全局 > 内置默认」取值。
	ContextWindow int `gorm:"column:context_window;not null;default:0"`
	// MaxOutputTokens 是该模型单次输出的上限，0 = 继承 agent_settings.max_output_tokens。
	// 上游通常有更小的硬限制，填大了会被上游拒；填 0 表示跟随全局。
	MaxOutputTokens int `gorm:"column:max_output_tokens;not null;default:0"`
	// Version 是乐观锁版本号（AI 提案 apply 与管理台改价 CAS 用）。
	// 绑定变更也算模型状态变更 → 同样 +1，否则「先改绑定再 apply 改价」会覆盖绑定的新状态。
	Version   int            `gorm:"column:version;not null;default:1"`
	CreatedAt time.Time      `gorm:"column:created_at;type:timestamptz"`
	UpdatedAt time.Time      `gorm:"column:updated_at;type:timestamptz"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index;type:timestamptz"`
}

func (Model) TableName() string { return "models" }

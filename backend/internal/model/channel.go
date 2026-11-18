package model

import (
	"time"

	"gorm.io/gorm"
)

// 渠道状态枚举（G23 冻结，唯一真相源）：1 启用 / 2 手动禁用 / 3 自动禁用。
// 常量保持无类型，便于与 int16 字段直接比较。
const (
	ChannelStatusEnabled  = 1
	ChannelStatusDisabled = 2
	ChannelStatusAutoBan  = 3
)

// Channel 对应 channels 表（v2.0 §4.2.4，字段对齐 mock-data.js L172-L178）。
// status：1 启用 / 2 手动禁用 / 3 自动禁用（G23 枚举冻结）。
// keys 为 Fernet 加密密钥池（与 api_keys 的 AES-GCM 不混用）；
// auto_ban 是渠道级熔断开关，是否生效由 system_config.monitor 全局策略共同决定（§4.6.5）；
// health 是健康度快照（fail_rate / success_count / fail_count / last_test_at / last_error）。
// bound_models 由 model_bindings 反查派生，不落库。
type Channel struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string `gorm:"size:64;not null"`
	// Icon 是渠道图标 token（"" | builtin:<key> | upload:<ulid>）。
	//
	// 渠道是运营内部概念，图标只在管理台展示（门户/文档站不出现渠道），
	// 所以它没有"回落到供应商"的语义 —— 为空就是首字占位。
	Icon         string `gorm:"size:255;not null;default:''"`
	AdapterID    string `gorm:"size:64;not null"`
	BaseURL      string `gorm:"size:255"`
	Keys         JSON   `gorm:"type:jsonb"`
	MultiKeyMode string `gorm:"size:16;not null;default:random"`
	// KeyMeta 是密钥池的**人工属性表**（map[keyID]Meta，见 pkg/channelkeys）。
	//
	// 为什么不做成独立表：密文只有一份（Keys），属性若另起一张表，两表就必然要
	// 同步——「keys 删了、属性还在」这类漂移没有任何机制能发现。同一行同一次
	// UPDATE 落库是唯一能保证"要么一起变、要么都不变"的形态。
	// NULL / {} / 缺项 = 全部默认（启用、无别名）→ 零数据迁移。
	KeyMeta JSON `gorm:"type:jsonb"`
	// SwitchKeyRetry：失败时是否换同渠道的另一把密钥重试。
	//
	// 默认 false = 与改造前**逐字节等价**（一次尝试只用一把 key，失败换渠道）。
	// 只作用于同步转发链路；异步任务提交不能开（会向上游建出两个计费任务）。
	SwitchKeyRetry bool     `gorm:"not null;default:false"`
	Groups         JSON     `gorm:"type:jsonb"`
	Weight         int      `gorm:"not null;default:1"`
	Priority       int      `gorm:"not null;default:1"`
	MaxConcurrency int      `gorm:"not null;default:0"`
	RPMLimit       int      `gorm:"not null;default:0"`
	Status         int16    `gorm:"not null;default:1"`
	AutoBan        bool     `gorm:"not null;default:true"`
	BalanceCNY     *float64 `gorm:"type:numeric(12,2)"`
	Health         JSON     `gorm:"type:jsonb"`
	// AdapterConfig 是渠道级声明式适配配置（auth_header / path_overrides / request_patch / response_path）。
	// 可空、无默认值：NULL 与 {} 都表示"关闭"，行为与改造前完全一致。
	// 由 AutoMigrate 加列（不需要数据迁移）。
	AdapterConfig JSON `gorm:"type:jsonb"`
	// AdapterOptions 是渠道级「适配器私有配置」（形状由适配器自己定义，如 AutoDL 的工作流映射）。
	//
	// 为什么不复用 AdapterConfig：后者的解析器开了 DisallowUnknownFields，
	// 厂商私有键塞进去会让旧二进制整条解析失败（DeclErr → 该渠道所有请求 500）。
	// NULL = 未配置（由适配器决定内置默认）；同样由 AutoMigrate 加列。
	AdapterOptions JSON `gorm:"type:jsonb"`
	// Version 是乐观锁版本号：**所有**写路径都要 version+1，
	// 否则 AI 提案的 CAS（期望版本 == 当前版本）永远成立，会静默覆盖别人的改动。
	Version   int            `gorm:"not null;default:1"`
	CreatedAt time.Time      `gorm:"type:timestamptz"`
	UpdatedAt time.Time      `gorm:"type:timestamptz"`
	DeletedAt gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (Channel) TableName() string { return "channels" }

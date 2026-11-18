package model

import (
	"time"

	"gorm.io/gorm"
)

// 规格状态枚举（与渠道一致的两态语义）：1 启用 / 2 停用。
//
// 没有第三态「草稿」：规格是运营/AI 提案产出的结果，草稿态存在于提案里
// （agent_proposals.status=draft），落库即生效。多一态会让「能不能被选路」
// 出现两个判断口（status 与提案状态），而它们必然不同步。
const (
	SpecStatusEnabled  = 1
	SpecStatusDisabled = 2
)

// AdapterSpec 对应 adapter_specs 表：L2「数据驱动适配器」的唯一真相源。
//
// 为什么要有这张表（而不是把 DSL 塞进 channels.adapter_config）：
// 一份规格描述的是「某家上游的 API 形状」，同形状的渠道可以有几十条。
// 塞进渠道就意味着改一处要改几十条、且无法保证它们描述的是同一个上游；
// 独立成表后，渠道只引用 `spec:<slug>`，改规格一次生效全部。
//
// 列设计的三条边界：
//  1. **Spec 是唯一真相**。Slug / Label / Vendor 三列是从 Spec JSON 里投影出来的
//     （列表展示、唯一索引、按供应商归组需要它们可索引），写入只经 service/spec
//     一个入口，投影与原文永远同批写，因此不存在两份真相。
//     停用/删除规格前请用 service/spec 的引用检查，别绕过去直接改库。
//  2. **不做软删以外的历史**。软删行仍占 (slug) 的部分唯一索引之外的空间，
//     但唯一索引条件带 `deleted_at IS NULL`，所以「删掉再建同名」是允许的。
//  3. **Version 是乐观锁**。AI 提案 apply 时按 version 做 CAS（S6）：
//     提案生成到人工确认之间，运营可能已经手工改过规格，直接覆盖会静默丢改动。
type AdapterSpec struct {
	ID uint64 `gorm:"primaryKey"`
	// Slug 进 channels.adapter_id（`spec:<slug>`），创建后不可改：
	// 改 slug 等于让所有引用它的渠道一夜之间变成「适配器未注册」。
	Slug string `gorm:"size:32;not null"`
	// Label 是管理台展示名（也进规格详情页标题）。
	Label string `gorm:"size:64;not null"`
	// Vendor 是 vendors.id（裸字符串，与 models.vendor 同规矩：无外键、必须先建后用）。
	// 只用于展示与归组，不参与选路。
	Vendor string `gorm:"size:32"`
	// Status 见 SpecStatus* 常量。停用后 relay 不再解析该规格，
	// 引用它的渠道会报「适配器未注册」——停用前必须先确认没有启用中的渠道在用。
	Status int16 `gorm:"not null;default:1"`
	// Version 每次写入 +1（含改规格、改状态），供提案 apply 的 CAS 与前端「已被他人修改」提示。
	Version int `gorm:"not null;default:1"`
	// Spec 是规格 DSL 原文（结构见 internal/relay/spec.Spec）。
	// 保存前必须过 spec.Decode + Validate；运行时再从库里 Decode 一次（双保险）。
	Spec JSON `gorm:"type:jsonb"`
	// OwnerID 是创建人（admin 用户 id），0 = 系统预置/历史数据。
	//
	// 只用于展示「谁建的」，**不**作为编辑权限边界：当前平台只有管理员一种角色，
	// 按 owner 限制编辑会造出「别人建的规格谁都改不了」的死锁。
	OwnerID   uint64         `gorm:"not null;default:0"`
	CreatedAt time.Time      `gorm:"type:timestamptz"`
	UpdatedAt time.Time      `gorm:"type:timestamptz"`
	DeletedAt gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (AdapterSpec) TableName() string { return "adapter_specs" }

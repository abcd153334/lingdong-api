// Package modelcfg 是「模型 + 绑定」的**唯一写库路径**。
//
// 存在理由：这套配置有两条写入方——管理台人工保存与 AI 编排的 apply。
// 两条路各写一份逻辑时，规则一定会分叉：历史上 AI 侧就漏了
// 「负价拒绝」「vendor 必须存在」「绑定渠道必须启用」「墓碑要物理清理」
// 「绑定要硬删（唯一索引含软删行）」这五条，而它们的共同症状是
// **保存成功、运行期才炸**，排查成本极高。
//
// 因此本包只提供一套函数，controller 与 service/agent 都必须走它。
// 价格结构直接复用 model.Prices —— 价格字段集合只有一个定义，
// 本包不再声明第二份 DTO（历史上有 6 份副本，漏改一处就是"保存成功但价格不生效"）。
package modelcfg

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"lingdong-api/internal/model"
)

// trimSpace 是 strings.TrimSpace 的短名（本包内大量字段需要 trim）。
func trimSpace(s string) string { return strings.TrimSpace(s) }

// nowFn 抽出来便于测试注入（当前直接用 time.Now）。
var nowFn = time.Now

// FieldError 把错误定位到具体字段（提案期要转成 operations[i].data.<field>）。
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Message }

// fe 是 FieldError 的简写。
func fe(field, format string, args ...any) error {
	return &FieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}

var (
	// ErrModelExists 表示同名模型仍然存活（不是墓碑）。
	ErrModelExists = errors.New("modelcfg: 同名模型已存在，请改用更新")
	// ErrVersionConflict 表示乐观锁版本不符（期间被别人改过）。
	ErrVersionConflict = errors.New("modelcfg: 数据已被修改，请刷新后重试")
	// ErrNotFound 表示目标行不存在。
	ErrNotFound = errors.New("modelcfg: 目标不存在")
	// ErrNoEnabledBinding 表示一组绑定里没有任何一条启用。
	ErrNoEnabledBinding = errors.New("modelcfg: 至少要有一条启用的绑定，否则模型不会进入目录")
	// ErrNoUsableBinding 表示「没有任何一条绑定能真正路由」（绑定启用 + 渠道启用）。
	//
	// 与 ErrNoEnabledBinding 的区别：后者说的是**这一组提交的绑定**，前者说的是
	// **库里的现状**（绑定启用了但渠道已停用也算不可用）。启用模型时只能查现状。
	ErrNoUsableBinding = errors.New("modelcfg: 已启用的模型至少需要一条可用绑定（绑定启用且渠道启用），或先停用该模型")
	// ErrBindingNotFound 表示该模型没有绑定到这个渠道（单条解绑的目标不存在）。
	//
	// 单独一个哨兵而不是复用 ErrNotFound：前者是「模型不存在」（404 的语义是
	// 用户找错了模型），后者是「模型在、但这行绑定不在」（并发解除 / 前端用了旧列表），
	// 两者的前端处置不同（前者刷新列表，后者重试一次即可）。
	ErrBindingNotFound = errors.New("modelcfg: 该模型没有绑定到这个渠道")
)

// Input 是模型的可写字段。
//
// 三态语义（每条都必须能区分「没提交」与「显式提交空」）：
//   - Prices 内的每个 lane 是 *int64：nil = 未提交，指向 0 = 显式免费；
//   - BillingMode + BillingModeSet：空串是合法值吗？不是，但"没提交"不能用零值表达，
//     否则一条"只改价格"的更新会把按次模型静默改成按 token（金额全错）；
//   - EndpointsSet / CapabilitiesSet / SpecPricesSet：缺了这个区分，
//     只改显示名会把端点与能力抹成 NULL，右栏 TAB 与文档端点会在下一次读取时悄悄变少。
type Input struct {
	ID string
	// DisplayName / Description 必须是指针：nil = 本次没提交（沿用库内），
	// 非 nil 才能表达"显式清空"（""）。
	//
	// 用字符串零值的后果：Updates 是全列写，而 FillMissing 不补这两个字段，
	// 于是任何"只改价格"的保存都会把展示名与描述写成空串 —— HTTP 200、
	// 无日志、无提示，只有下次打开列表才发现名字没了（2026-10-29 审计 A-01）。
	DisplayName *string
	Description *string
	// Icon 是模型图标 token（"" | builtin:<key> | upload:<ulid>）。
	//
	// **必须是指针**：nil = 本次没提交（沿用库内），非 nil 才能表达"显式清空"（""，
	// 清空后回到「沿用供应商图标」）。用字符串零值的后果与 DisplayName 完全一样：
	// Updates 是**全列写**，任何"只改价格"的保存都会把图标写成空串。
	Icon     *string
	Vendor   string
	Modality string

	BillingMode    string
	BillingModeSet bool
	Enabled        *bool

	Endpoints       []string
	EndpointsSet    bool
	Capabilities    []string
	CapabilitiesSet bool
	// Tags 是营销标签（数组顺序即展示顺序，没有 order 字段）。
	//
	// 与 Endpoints/Capabilities 同一种三态：TagsSet 区分「没提交」与「提交空」。
	// 少了它，任何"只改价格"的保存都会把标签抹成 NULL。
	Tags    []model.ModelTag
	TagsSet bool

	Prices        model.Prices
	SpecPrices    model.SpecPrices
	SpecPricesSet bool

	// ContextWindow / MaxOutputTokens 是模型级覆盖（0 = 继承 agent_settings 的全局值）。
	//
	// 必须指针：nil = 本次没提交（沿用库内），非 nil 才能表达"显式改成 0 = 回到继承"。
	// 用非指针 int 的后果是：只改价格的提交里它们是 0，而 Updates 是全列写 ——
	// 运营填好的窗口会在一次改价后静默变回"继承"，界面上完全看不出来。
	ContextWindow   *int
	MaxOutputTokens *int

	// ClearPrices 列出本次要**显式清空**的 lane 名（键同 laneCN：input/output/... /call）。
	//
	// 为什么必须单独一份名单：lane 是 `*int64`，nil 同时承担"本次没提交"与
	// "要清空"两种语义 —— 前者由 FillMissing 用库内值补齐，于是"清空"永远
	// 落不了库（界面上删掉价格、保存后价格又回来了）。用一个独立名单把两种
	// 意图分开，清空动作就能真正写进 NULL。
	//
	// 非法 lane 名由 Build 直接拒绝（拼错不该静默不生效）。
	ClearPrices []string
}

// Binding 是一条模型绑定（替换集合的元素）。
type Binding struct {
	ChannelID     uint64
	UpstreamModel string
	Weight        int
	Priority      int
	Enabled       bool
}

// FillEnabled 把「未提交」的布尔值补成默认值（新建用 true）。
func (in *Input) fillEnabled(def bool) {
	if in.Enabled == nil {
		v := def
		in.Enabled = &v
	}
}

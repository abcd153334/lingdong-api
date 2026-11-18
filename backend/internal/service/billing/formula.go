package billing

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
)

// 计费口径错误。
var (
	// ErrPriceMissing 表示模型未配置计价格 → 400 price_not_configured（fail-closed 防白嫖）。
	ErrPriceMissing = errors.New("billing: 模型未配置价格")
	// ErrSpecUnsupported 表示按规格计费里未知的 kind。
	ErrSpecUnsupported = errors.New("billing: 该按规格类型不支持")
	// ErrInvalidTier 表示「这次请求与模型价目对不上」，只有两个来源：
	//   - 视频档位在该模型的 second_price 里没有对应键（价目为空或端点与价目错配）；
	//   - 图片张数超过引擎上限（MaxImageN，防一次冻爆余额）。
	//
	// 图片的「档位未配价」不在这里：参数不再被拦截后，它按最高价档计费 + 留痕，
	// 绝不因为运营少配一个档就把用户的生成请求打回去。
	ErrInvalidTier = errors.New("billing: 模型价目与本次请求不匹配")
	// ErrChargeOverLimit 表示单次预估金额超过 MaxCharge（预扣前拒绝，避免冻结不合理金额）。
	ErrChargeOverLimit = errors.New("billing: 单次请求金额超出上限")
	// ErrOverMaxSeconds 表示时长超过模型级 max_seconds（**只在 Estimate 生效**）。
	//
	// 结算（Settle）绝不能因此报错：已冻结的任务若在结算期失败会停在 running
	// 直到 deadline 兜底退款，产物白丢。
	ErrOverMaxSeconds = errors.New("billing: 视频时长超过模型上限")
)

// Semantic 表示上游 usage 的 token 语义（决定缓存 token 是否已包含在 prompt 内）。
type Semantic string

const (
	// SemanticOpenAI：prompt_tokens 已包含 cached_tokens / image_tokens（OpenAI、Gemini、多数兼容网关）。
	SemanticOpenAI Semantic = "openai"
	// SemanticAnthropic：input_tokens 不含缓存，缓存按 cache_read/cache_creation 单独上报。
	SemanticAnthropic Semantic = "anthropic"
	// SemanticUnknown：无法判定，按 OpenAI 语义处理（保守）。
	SemanticUnknown Semantic = "unknown"
)

// Snapshot 是 pricing_snapshot 的运行态视图：Freeze 时序列化落库，Settle 只读快照。
//
// 为什么价格必须进快照：改价绝不能影响在途请求。快照里的价格是**当时的绝对金额**，
// 所以"改一次价，昨天冻结的请求按新价结算"这种事故从结构上不可能发生。
//
// 结构复用 model.Prices / model.SpecPrices：价格字段集合只有一个定义，
// 快照不另立一份（历史上价格字段有 6 份副本，漏改一处就是"保存成功但价格不生效"）。
type Snapshot struct {
	ModelID     string `json:"model_id"`
	BillingMode string `json:"billing_mode"`
	// Price 是 token / 按次价目（微 / 百万 tokens、微 / 次）。
	Price model.Prices `json:"price"`
	// SpecPrice 是按规格价目（微 / 张、微 / 秒、微 / 条、微 / 任务）。
	SpecPrice model.SpecPrices `json:"spec_price"`
	// Discount 是分组折扣的**微比例**（1e6 = 不打折，900000 = 9 折）。
	// 0 或 1e6 表示无折扣。只作用于模型调用，绝不作用于充值。
	Discount int64 `json:"discount,omitempty"`
	// Currency 是冻结时刻的结算币种（账单凭证）。币种锁定后恒等于站点币种，
	// 但仍必须落库：账单要能自证"这笔钱是哪种货币"。
	Currency   string    `json:"currency,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
	// ConfigAnomaly 是构建快照时**纠偏配置**产生的标记（billing_mode / kind 与价目不符）。
	//
	// 放在快照里而不是只在请求上下文：结算读的是快照，在途单/迟到结算时
	// 请求上下文早就没了 —— 而"这一单是按纠偏后的口径收的"必须能留痕。
	ConfigAnomaly string `json:"config_anomaly,omitempty"`
}

// CurrencyValue 返回快照的币种：优先用快照自带值，缺失时回落当前站点币种。
//
// 回落当前而不是固定值：快照缺 currency 只可能出现在"被外部改写"的异常行上，
// 此时按当前口径展示至少与账单一致。
func (s Snapshot) CurrencyValue() string {
	if code, ok := conf.NormalizeCurrency(s.Currency); ok {
		return code
	}
	return conf.SiteCurrency()
}

// Usage 是结算输入的各 token 通道实际用量 + 多模态快照参数（对齐 usages 列）。
type Usage struct {
	PromptTokens       int64
	CompletionTokens   int64
	CacheTokens        int64
	CacheWrite5mTokens int64
	CacheWrite1hTokens int64
	ImageTokens        int64
	AudioInputTokens   int64
	AudioOutputTokens  int64
	// 多模态快照：图片档位/画幅/质量/张数；音频时长/字符数；重排文档数。
	ImageSpec    string
	ImageRatio   string
	ImageQuality string
	ImageN       int64
	// ImageSpecExplicit 表示尺寸是用户明确指定的（写档位或写像素）。
	// 只有它为 true 才做 tier_mismatch 比对：auto / 没传 size 的请求由上游挑档。
	ImageSpecExplicit bool
	// ImageSpecSource 是档位来源（tier/pixels/ratio/auto/empty/other），落 usages.image_spec_source。
	ImageSpecSource   string
	AudioDurationSecs int64
	AudioChars        int64
	RerankDocs        int64
	// 视频快照：分辨率档位（480p/720p/1080p/2k）/秒数/三类素材个数。
	VideoResolution string
	VideoSeconds    int64
	RefImages       int64
	RefVideos       int64
	RefAudios       int64
	// RefVideoSeconds 是参考视频的**累计秒数**（仅在配了 ref_video_second_price 时参与计费）。
	RefVideoSeconds int64
	// Semantic 是上游 usage 的 token 语义（空值按 OpenAI 处理）。
	Semantic Semantic
}

// chargeResult 是内部结算结果（金额 + 口径标记）。
type ChargeResult struct {
	Amount int64
	// Uncapped 是**两级降额之前**的应收额。Amount 可能因为全局上限 / 冻结额 3 倍 /
	// 余额下限被压低，只留 Amount 就无法回答"这条为什么少收了"。落 usages.charge_before_cap。
	Uncapped int64
	Flags    []string
}

// Charge 计算实际应扣额（保留简版签名供任务/测试使用）。
func Charge(s Snapshot, u Usage) (int64, error) {
	r, err := ChargeDetailed(s, u)
	if err != nil {
		return 0, err
	}
	return r.Amount, nil
}

// ChargeDetailed 返回金额与口径标记（negative_input / cache_overlap_clamped / charge_capped）。
//
// 归一规则（唯一权威）：
//   - 所有 token 通道负值归零并打 negative_*；
//   - OpenAI 语义（默认）：prompt 已包含 cache/image/audio_in，故基准 = prompt − Σ这些通道（下限 0）；
//   - Anthropic 语义：prompt 不含缓存，基准 = prompt，缓存通道全额叠加；
//   - 通道费 = Σ(通道 token × 通道单价) / 1e6，**只在最后舍入一次**；
//   - 末乘分组折扣（唯一可选乘数），再截顶。
func ChargeDetailed(s Snapshot, u Usage) (ChargeResult, error) {
	amount, flags, err := baseCharge(s, u, false)
	if err != nil {
		return ChargeResult{}, err
	}
	amount, err = applyDiscountAndFloor(s, amount)
	if err != nil {
		return ChargeResult{}, err
	}
	return capCharge(amount, flags), nil
}

// baseCharge 按计费方式算原始金额（未打折、未截顶）。
func baseCharge(s Snapshot, u Usage, strictLimits bool) (int64, []string, error) {
	amount, flags, err := baseChargeOf(s, u, strictLimits)
	if s.ConfigAnomaly != "" {
		// 配置纠偏标记统一在出口合并：三条结算路径（Settle / LateSettle / Reclaim）
		// 都经过这里，不必各自记得搬一次。
		flags = append(splitFlagList(s.ConfigAnomaly), flags...)
	}
	return amount, flags, err
}

// splitFlagList 把逗号分隔的标记串拆成切片（去空、去空白）。
func splitFlagList(raw string) []string {
	out := make([]string, 0, 4)
	for _, f := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(f); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func baseChargeOf(s Snapshot, u Usage, strictLimits bool) (int64, []string, error) {
	switch s.BillingMode {
	case model.BillingModeCall:
		if s.Price.Call == nil {
			return 0, nil, ErrPriceMissing
		}
		return *s.Price.Call, nil, nil
	case model.BillingModeSpec:
		v, flags, err := specCharge(s, u, strictLimits)
		return v, flags, err
	case model.BillingModeTokens:
		return tokenCharge(s.Price, u)
	default:
		// 未知计费方式一律 fail-closed：宁可 400，也不能当成免费放行。
		return 0, nil, ErrPriceMissing
	}
}

// tokenCharge 按 8 条 token lane 计价。
//
// 每个 lane 的单价取值规则（唯一一处）：
//
//	该 lane 显式配置 → 用它（显式 0 = 该通道免费）
//	该 lane 未配置   → 用 input 单价（同单位，可兜底且不会漏收）
//
// 特例：cache_create_1h 未配置时优先回落 cache_create（5 分钟档），再回落 input。
// 旧实现用 ×1.6 派生 1h 价，那个系数没有任何依据，且让"填了 5m 价"隐式改了 1h 价。
func tokenCharge(p model.Prices, u Usage) (int64, []string, error) {
	if p.Input == nil {
		return 0, nil, ErrPriceMissing
	}
	in := *p.Input
	lane := func(v *int64) int64 {
		if v != nil {
			return *v
		}
		return in
	}
	cacheCreate1h := in
	switch {
	case p.CacheCreate1h != nil:
		cacheCreate1h = *p.CacheCreate1h
	case p.CacheCreate != nil:
		cacheCreate1h = *p.CacheCreate
	}

	u, flags := sanitizeUsage(u)

	base := u.PromptTokens
	if u.Semantic != SemanticAnthropic {
		base -= u.CacheTokens + u.CacheWrite5mTokens + u.CacheWrite1hTokens + u.ImageTokens + u.AudioInputTokens
		if base < 0 {
			base = 0
			flags = append(flags, model.AnomalyCacheOverlapClamped)
		}
	}

	amount, err := money.SumProducts(money.Scale, []money.Product{
		{Qty: base, Unit: in},
		{Qty: u.CacheTokens, Unit: lane(p.Cache)},
		{Qty: u.CacheWrite5mTokens, Unit: lane(p.CacheCreate)},
		{Qty: u.CacheWrite1hTokens, Unit: cacheCreate1h},
		{Qty: u.ImageTokens, Unit: lane(p.Image)},
		{Qty: u.AudioInputTokens, Unit: lane(p.Audio)},
		{Qty: u.CompletionTokens, Unit: lane(p.Output)},
		{Qty: u.AudioOutputTokens, Unit: lane(p.AudioOutput)},
	})
	if err != nil {
		return 0, flags, err
	}
	return amount, flags, nil
}

// sanitizeUsage 把负值通道归零并记录标记（避免上游异常值把总价算成负数或抵扣其它通道）。
func sanitizeUsage(u Usage) (Usage, []string) {
	var flags []string
	clamp := func(name string, v *int64) {
		if *v < 0 {
			*v = 0
			flags = append(flags, name)
		}
	}
	clamp(model.AnomalyNegativePrompt, &u.PromptTokens)
	clamp(model.AnomalyNegativeCompletion, &u.CompletionTokens)
	clamp(model.AnomalyNegativeCache, &u.CacheTokens)
	clamp(model.AnomalyNegativeCacheWrite5m, &u.CacheWrite5mTokens)
	clamp(model.AnomalyNegativeCacheWrite1h, &u.CacheWrite1hTokens)
	clamp(model.AnomalyNegativeImage, &u.ImageTokens)
	clamp(model.AnomalyNegativeAudioInput, &u.AudioInputTokens)
	clamp(model.AnomalyNegativeAudioOutput, &u.AudioOutputTokens)
	if len(flags) > 0 {
		flags = append(flags, model.AnomalyNegativeInput)
	}
	return u, flags
}

// applyDiscount 施加分组折扣（微比例）。0 与满额（1e6）都视为不打折。
func applyDiscount(d, amount int64) (int64, error) {
	// 只跳过「没配」与「正好 1.0」两种；**加价（>1.0）必须生效**。
	//
	// 曾经写成 `d >= money.Scale` 一并跳过 —— 于是所有加价倍率（1.2× / 1.5×）被
	// 静默当成不打折，而配置上界是 10 倍、管理台还写着「1.2 = 加价 20%」。
	// 少收方向无法靠客诉发现，只能靠这里改对。
	if d <= 0 || d == money.Scale || amount == 0 {
		return amount, nil
	}
	// 上界与保存期校验（systemconfig：不超过 10 倍）保持一致：
	// 越界值只可能来自手改库/迁移，此时宁可"不打折"也不能乘出一个荒谬的金额。
	// 保存期已硬拦，所以这里不会掩盖正常的加价配置。
	if d > maxDiscountMultiplier {
		return amount, nil
	}
	return money.Mul(amount, d)
}

// maxDiscountMultiplier 是分组倍率的运行期上界（10 倍），与
// systemconfig 的保存期校验逐字一致：「不超过 10 倍」。
//
// 这么高只是为了"允许极端加价策略"，正常运营不会用到；写成常量而不是魔法数，
// 是为了让保存期与运行期引用同一个值。
const maxDiscountMultiplier = 10 * money.Scale

// applyDiscountAndFloor 先打折、再把金额抬到最低消费（**顺序不可交换**）。
//
// min_price 是「这一单最少收多少」的地板，不是折扣的基数。先抬后折会让
// 5 折把 0.01 元的最低价打成 0.005 元 —— 用户看到的「最低消费」形同虚设。
//
// 折扣与地板必须由**同一个函数**施加：冻结（Estimate）与结算（ChargeDetailed）
// 各写一遍的话，任何一边漏了地板，就会出现「冻结 0.005、结算 0.01」这种
// 冻结额小于应收额的情况，进而被 3 倍截顶吃掉。
func applyDiscountAndFloor(s Snapshot, amount int64) (int64, error) {
	v, err := applyDiscount(s.Discount, amount)
	if err != nil {
		return 0, err
	}
	if floor := specMinPrice(s); floor > 0 && v < floor {
		v = floor
	}
	return v, nil
}

// specMinPrice 返回按规格计价的「最低消费」（0 = 未配置）。
//
// 只有 per_second 有这个概念（音频短文本）；其它 kind 上出现 min_price 会被
// ValidateSpecPrices 的 rejectForeign 拒绝，这里不做二次判断。
func specMinPrice(s Snapshot) int64 {
	if s.BillingMode != model.BillingModeSpec {
		return 0
	}
	// 只有按秒计费（音频）有"最低消费"这个概念。其它 kind 上出现 min_price
	// 只可能来自历史脏数据/迁移：沿用会把每一单都抬到地板（多收）。
	// 地板与折扣由 applyDiscountAndFloor 统一施加，ChargeCeiling 出口也调它 ——
	// 上限与实收是同一套口径，不存在"两套价格"。
	if s.SpecPrice.Kind != SpecKindPerSecond || s.SpecPrice.MinPrice == nil {
		return 0
	}
	return *s.SpecPrice.MinPrice
}

// capCharge 对金额做非负与上限截顶（Uncapped 保留截顶前的值）。
func capCharge(amount int64, flags []string) ChargeResult {
	if amount < 0 {
		amount = 0
	}
	uncapped := amount
	if limit := maxCharge(); amount > limit {
		amount = limit
		flags = append(flags, model.AnomalyChargeCapped)
	}
	return ChargeResult{Amount: amount, Uncapped: uncapped, Flags: flags}
}

// Estimate 计算预扣额（token 用估算值；按规格型用请求参数精确冻结）。
//
// 与 ChargeDetailed 共用同一套取价函数 —— **不允许存在第二份公式**。
// 两处唯一差异：
//  1. token 用估算值，且输入按"可能命中的最高输入单价"加权（媒体通道占比事前不可知，
//     少冻只能截顶少收，多冻可以原路退回）；
//  2. 舍入用向上取整（多冻可退）。
func Estimate(s Snapshot, promptEst, completionEst int64, spec Usage) (int64, error) {
	promptEst, completionEst = nonNeg(promptEst), nonNeg(completionEst)
	var amount int64
	switch s.BillingMode {
	case model.BillingModeCall:
		if s.Price.Call == nil {
			return 0, ErrPriceMissing
		}
		amount = *s.Price.Call
	case model.BillingModeSpec:
		v, _, err := specCharge(s, spec, true)
		if err != nil {
			return 0, err
		}
		amount = v
	case model.BillingModeTokens:
		if s.Price.Input == nil {
			return 0, ErrPriceMissing
		}
		v, err := money.SumProductsCeil(money.Scale, []money.Product{
			{Qty: promptEst, Unit: maxInputUnitPrice(s.Price)},
			{Qty: completionEst, Unit: maxOutputUnitPrice(s.Price)},
		})
		if err != nil {
			return 0, err
		}
		amount = v
	default:
		return 0, ErrPriceMissing
	}
	v, err := applyDiscountAndFloor(s, amount)
	if err != nil {
		return 0, err
	}
	if v > maxCharge() {
		return 0, ErrChargeOverLimit
	}
	return v, nil
}

// maxInputUnitPrice 返回输入侧可能命中的最高单价（含兜底到 input 的通道）。
func maxInputUnitPrice(p model.Prices) int64 {
	if p.Input == nil {
		return 0
	}
	m := *p.Input
	for _, v := range []*int64{p.Cache, p.CacheCreate, p.CacheCreate1h, p.Image, p.Audio} {
		if v != nil && *v > m {
			m = *v
		}
	}
	return m
}

// maxOutputUnitPrice 返回输出侧可能命中的最高单价（off/audio_output 未配置时回落 input）。
func maxOutputUnitPrice(p model.Prices) int64 {
	base := int64(0)
	if p.Input != nil {
		base = *p.Input
	}
	m := base
	if p.Output != nil {
		m = *p.Output
	}
	if p.AudioOutput != nil && *p.AudioOutput > m {
		m = *p.AudioOutput
	}
	return m
}

// nonNeg 把负数归零（上游/请求异常值防御）。
func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// MaxVideoSeconds 是单次视频生成秒数的引擎上限（超限饱和）。
const MaxVideoSeconds = 3600

// audioCharsPerSecond 是「文本 → 时长」的估算系数（中文 TTS 约 4 字/秒）。
//
// 两处用到它，语义不同：
//   - 预扣：产物还不存在，只能按文本估，向上取整（估大只是多冻一会儿，可退）；
//   - 结算兜底：产物存在但**时长不可测**（opus/aac 裸流、流式体又没有 Content-Length），
//     用它估并打 audio_seconds_estimated。为什么不按 0 收：0 等于把整段音频白送，
//     而且是静默的 —— 一条渠道的按秒模型会长期按 0 收，月底对账才发现。
//     估算有偏差但可解释、可审计，比"确定地少收"好。
//
// 这个系数写死在引擎里而不是做成配置项：它一旦可配，每个模型都会填一个
// 对自己有利的值，最终冻结额与实际扣费脱节，对账时无法解释。
const audioCharsPerSecond = 4

// specCharge 按规格型计费（图片 / 视频 / 任务 / 音频按秒）。
//
// strictLimits 只在**预扣（Estimate）**时为 true，用于执行模型级上限
// （max_seconds / max_n）—— 那些只该在提交期是硬约束。
// 结算必须传 false：钱已经冻了、产物已经生成了，此时报错只会让结算失败 →
// 任务停在 running → deadline 全额退款 → **白送**。
// 结算期的上限一律「饱和 + 打标记」，由返回值里的 flags 留痕。
func specCharge(s Snapshot, u Usage, strictLimits bool) (int64, []string, error) {
	sp := s.SpecPrice
	if sp.Kind == "" {
		// 价目压根没配（kind 缺失）≠ 配了引擎不认识的 kind。
		// 前者是"未配置价格"（补价即可），后者是"引擎不支持"（换计费方式）。
		// 两者都 400，但文案必须能指导下一步动作；stats.countPriceMissing 也只统计前者，
		// 混成 ErrSpecUnsupported 会让"启用但没配价"的模型在体检里漏掉。
		return 0, nil, ErrPriceMissing
	}
	switch sp.Kind {
	case SpecKindPerImage:
		v, capped, flags, err := imageCharge(sp, u, strictLimits)
		out := make([]string, 0, 2)
		if flags != "" {
			out = append(out, flags)
		}
		if capped {
			out = append(out, model.AnomalyImageNCapped)
		}
		return v, out, err
	case SpecKindPerVideo:
		v, err := videoCharge(sp, u, strictLimits)
		return v, nil, err
	case SpecKindPerTask:
		v, err := perTaskCharge(sp)
		return v, nil, err
	case SpecKindPerClip:
		v, capped, err := perClipCharge(sp, u, strictLimits)
		out := make([]string, 0, 2)
		if capped {
			out = append(out, model.AnomalyImageNCapped)
		}
		return v, out, err
	case SpecKindPerSecond:
		v, estimated, err := perSecondCharge(sp, u, strictLimits)
		if estimated && !strictLimits {
			// 只在结算期留痕：预扣期按文本估是**正常路径**，标记它会把
			// 每一次正常 TTS 调用都染成"计费异常"，异常标记本身就此失效。
			return v, []string{model.AnomalyAudioSecondsEstimated}, err
		}
		return v, nil, err
	}
	return 0, nil, ErrSpecUnsupported
}

// perSecondCharge 按产物秒数计费（音频）：秒数 × 每秒单价，再抬到最低消费。
//
// 三条边界，每条都对应一次真实错账：
//  1. 秒数首选**产物**真实时长（桥接 extract.duration、音频头探测），不是请求参数：
//     "要 10 秒"与"生成 12 秒"是两件事，按请求参数收就是系统性少收；
//  2. max_seconds 是**模型级硬约束**：预扣期（strictLimits）超限直接拒绝，
//     结算期只饱和并留痕。两种模式都饱和会让长文本按短文本收费（系统性少收）；
//  3. 最低消费是**下限**而不是"另加一笔"：¥0.001/秒 + 最低 ¥0.01，
//     3 秒的音频收 ¥0.01（而不是 ¥0.003 + ¥0.01）。
func perSecondCharge(sp model.SpecPrices, u Usage, strictLimits bool) (int64, bool, error) {
	if sp.PerSecondPrice == nil || *sp.PerSecondPrice < 0 {
		return 0, false, ErrPriceMissing
	}
	secs := nonNeg(u.AudioDurationSecs)
	estimated := false
	if secs == 0 && u.AudioChars > 0 {
		// 时长不可得（预扣期还没有产物；或结算期产物格式探测不出且上游没给时长）。
		// 一律按文本估，不留 0：0 会把这单算成免费或只收最低消费。
		secs = (u.AudioChars + audioCharsPerSecond - 1) / audioCharsPerSecond
		estimated = true
	}
	if secs > MaxPriceSeconds {
		// 引擎上限饱和：一小时以上的"音频"只可能是探测出错或上游返回了错误文件，
		// 按上限收而不是按天文数字收（也保证 int64 不溢出）。
		secs = MaxPriceSeconds
	}
	// max_seconds 在**预扣期是硬闸**，结算期才饱和。
	//
	// 这里曾经两种模式都饱和，后果是系统性少收：配了 max_seconds=60 的 TTS
	// 收 2400 字（估 600 秒）时只按 60 秒冻结、也只按 60 秒结算，产出十分钟音频。
	// 更糟的是"饱和"看起来一切正常（没有任何 anomaly），对账永远发现不了。
	// 与 videoCharge 保持同一语义：提交期拒绝，结算期饱和。
	if sp.MaxSeconds != nil && *sp.MaxSeconds > 0 && secs > *sp.MaxSeconds {
		if strictLimits {
			// 带上实际值与上限：controller 的 defWithReason 只把「：」之后的内容
			// 拼进对外文案，裸 errors.New 会让用户只看到「参数不合法」四个字
			// （真机 B5/B6 实测：31 秒被拒，但没说上限是多少）。
			return 0, estimated, fmt.Errorf("%w：%d 秒超过模型上限 %d 秒", ErrOverMaxSeconds, secs, *sp.MaxSeconds)
		}
		secs = *sp.MaxSeconds
	}
	// 用 MulDiv 而不是裸乘：money 包的不变量是"绝不先乘再判溢出"——
	// 回绕成负数后 `amount > MaxAmount` 为假，会被最低消费静默掩盖成近乎免费。
	amount, err := money.MulDiv(*sp.PerSecondPrice, secs, 1)
	if err != nil {
		return 0, estimated, err
	}
	// 最低消费**不在这里**抬：它是「这一单最少收多少」的地板，必须在折扣之后生效
	// （见 applyDiscountAndFloor）。先抬后折会让 5 折把 0.01 元的最低价打成 0.005，
	// 与对外承诺的「最低消费」不符。
	return amount, estimated, nil
}

// perTaskCharge 按任务计费（音乐/歌词）：一次任务一个固定价。
//
// 不读 u 的任何字段：时长/产物数都不参与（上游一次调用即一个计费单位）。
func perTaskCharge(sp model.SpecPrices) (int64, error) {
	if sp.PerTaskPrice == nil || *sp.PerTaskPrice < 0 {
		// 缺失 = 未配置价；负价是脏数据。两者都 fail-closed。
		return 0, ErrPriceMissing
	}
	// 显式 0 = 免费（0=免费一等公民，区别于"未配置"）。
	return *sp.PerTaskPrice, nil
}

// perClipCharge 按**实际交付条数**计费（音乐/歌词）：每条单价 × 条数。
//
// 与 perTaskCharge 的唯一区别就是"读不读条数"：这正是改口径的全部目的 ——
// 一次任务出 1 首还是 2 首，收的钱不一样。
//
// 三条边界：
//  1. **0 条 = 0 元**（不是"按 1 条收"）。这是与 per_image 最关键的分歧：
//     imageCharge 会把 0 张夹成 1 张（图片请求至少有一次出图），而音乐可能
//     一首都没交付成功，此时收钱无法向用户解释。
//  2. max_n 是**计费条数上限**：上游偶尔回 3 条也只按 2 条收（多给不向用户收费）。
//     预扣期（strictLimits）超限直接拒绝，结算期饱和并打 image_n_capped 标记。
//  3. 单价缺失 = 未配价（ErrPriceMissing），不是免费。
func perClipCharge(sp model.SpecPrices, u Usage, strictLimits bool) (int64, bool, error) {
	if sp.PerClipPrice == nil || *sp.PerClipPrice < 0 {
		return 0, false, ErrPriceMissing
	}
	n := u.ImageN
	if n <= 0 {
		// 0 条：一分钱不收。绝不在这里回落到 1。
		return 0, false, nil
	}
	capped := false
	switch {
	case sp.MaxN != nil && *sp.MaxN > 0 && n > *sp.MaxN:
		if strictLimits {
			return 0, false, fmt.Errorf("%w：本次 %d 条超过模型上限 %d 条", ErrInvalidTier, n, *sp.MaxN)
		}
		n = *sp.MaxN
		capped = true
	case n > MaxMusicClips:
		if strictLimits {
			return 0, false, fmt.Errorf("%w：本次 %d 条超过引擎上限 %d 条", ErrInvalidTier, n, MaxMusicClips)
		}
		n = MaxMusicClips
		capped = true
	}
	amount, err := money.MulDiv(*sp.PerClipPrice, n, 1)
	if err != nil {
		return 0, false, ErrAmountTooLarge
	}
	return amount, capped, nil
}

// imageCharge 图片按张计费：单价由 priceOfImage 一次取出 × 张数。
//
// 取价口径（与 ChargeCeiling / TierInfo 共用同一个函数，不各自读 TierPrice）：
//
//	命中档位 → 该档单价；未命中 → **最高单价档**；档位为空 → 默认档。
//
// 价目为空才报 ErrPriceMissing —— 那是「这个模型根本没配价」，不是用户参数错。
//
// 0 的语义是「免费」而不是「拒绝」：0 价在该引擎里是一等公民（价格填 0 = 免费）。
//
// ResolveImageCount 是「这一次该按几张图计费」的唯一判定，同步与流式共用。
//
// 三个输入的语义：
//
//	billed    上游实际产出的张数（同步=响应 data[] 条数；流式=usage.generated_images）
//	delivered 实际交付给客户端的张数（同步与 billed 相同；流式=已下发的图片帧数）
//	requested 请求声明的张数，仅作兜底（<=0 时按 1）
//
// 为什么必须单源：同步与流式原本各写一份，规则并不一致 —— 同步在「上游报 0 张」时
// 按 1 张计并打 image_count_zero；流式在「上游没报张数」时按 1 张计却不打标。
// 两份实现迟早给出相反结论，而账单只能有一个答案。
//
// partial 表示「上游产出多于实际交付」：同步路径不会发生（body 即交付），
// 流式路径对应「上游说生成了 N 张、只下发 M 张」，由调用方打 stream_partial_delivered。
func ResolveImageCount(billed, delivered, requested int64) (n int64, partial bool) {
	if billed > 0 {
		if delivered < billed {
			// 产出多于交付：按实际交付计（用户没拿到的不该收费）。
			// delivered==0 时 clamp 到 1 —— 一张都没拿到也不能整单免费，
			// 因为上游已经真跑了一次，白送的成本要有账可查。
			return clampImageCount(delivered), true
		}
		return billed, false
	}
	if delivered > 0 {
		return delivered, false
	}
	return clampImageCount(requested), false
}

// clampImageCount 保证张数至少 1：0 张会让整单免费，而「上游 2xx 却没给图」
// 是上游违约，不该由用户白拿（由调用方打 anomaly 让它可见）。
func clampImageCount(n int64) int64 {
	if n < 1 {
		return 1
	}
	return n
}

// 张数上限分两档处理：
//
//	引擎上限（MaxImageN=128）：预扣期超限 → ErrInvalidTier。
//	    它不是"模型能力"，而是防 `n: 100000` 这种请求把余额一次冻爆的硬护栏；
//	    正常调用不可能到 128 张，拒绝不会挡住真实用户。
//	模型上限（max_n）：两个阶段都**饱和**（夹紧）+ 由调用方打 image_n_capped。
//	    预扣期也夹紧的理由：这是运营配的一个上限，用户多要几张不该让整单失败；
//	    上游若不接受会明确报错（平台不改写出站参数），交付张数与计费张数
//	    最终都由「实际交付」收敛。
func imageCharge(sp model.SpecPrices, u Usage, strictLimits bool) (int64, bool, string, error) {
	n := u.ImageN
	if n <= 0 {
		n = 1
	}
	capped := false
	if n > MaxImageN {
		if strictLimits {
			return 0, false, "", ErrInvalidTier
		}
		n, capped = MaxImageN, true
	}
	// MaxN 为 0 视为未配置（保存期要求 ≥1）：钳到 0 会把整单算成免费。
	if sp.MaxN != nil && *sp.MaxN > 0 && n > *sp.MaxN {
		n, capped = *sp.MaxN, true
	}
	// 取价只有一个入口（priceOfImage）：档位命中 → 该档；未命中 → 最高价档；
	// 档位为空 → 默认档。ChargeCeiling / TierInfo 也走它，三处不可能算出不同的价。
	unit, source, ok := priceOfImage(sp, u.ImageSpec)
	if !ok {
		// 价目里一个档位都没有 = 真没配价；但「有价目、只是没配默认档」要分开报：
		// 前者该去补价格，后者该去补 resolution_fixed（或让调用方指定 size）。
		if len(ImageTierMap(sp)) > 0 && strings.TrimSpace(u.ImageSpec) == "" {
			return 0, false, "", ErrImageDefaultTierUnset
		}
		return 0, false, "", ErrPriceMissing
	}
	if unit < 0 {
		return 0, false, "", ErrPriceMissing
	}
	flags := ""
	if source == TierSourceHighest && strings.TrimSpace(u.ImageSpec) != "" {
		// 防御：正常情况下 ResolveImageTier 已经把未配价档换成最高档并留过痕。
		flags = model.AnomalyImageTierUnpriced
	}
	amount, err := money.MulDiv(unit, n, 1)
	return amount, capped, flags, err
}

// UpgradeDeliveredTier 在结算前把计费档抬到**上游实际交付**的更高档（只升不降）。
//
// 为什么必须有：参数不再被拦截，用户可以写 `size=8K` 而价目只配到 2K，
// 平台按兜底价收、上游真出 8K；反过来也可能请求 1K 而上游给了 4K。
// 不补收 = 每一次交付高档都在赔钱，而账单上完全看不出异常。
func UpgradeDeliveredTier(sp model.SpecPrices, u Usage, upstreamSpec string) (Usage, string) {
	if sp.Kind != SpecKindPerImage || strings.TrimSpace(upstreamSpec) == "" {
		return u, ""
	}
	spec, _ := normalizeUpstreamSpec(upstreamSpec)
	if spec == "" {
		return u, ""
	}
	tier, ok := upstreamTier(sp, spec)
	if !ok || tier == "" {
		return u, ""
	}
	upUnit, _, upOK := priceOfImage(sp, tier)
	curUnit, _, curOK := priceOfImage(sp, u.ImageSpec)
	if !upOK || !curOK || upUnit <= curUnit {
		return u, ""
	}
	u.ImageSpec = tier
	u.ImageSpecExplicit = true
	return u, model.AnomalyTierUpgradeCharged
}

// videoCharge 视频计费：
//
//	timeFee = second_price[resolution] × seconds
//	          × long_multiplier（仅当秒数超过阈值；**只乘时长费**，不乘素材费）
//	+ ref_image_price × max(0, refImages − ref_image_free)
//	+ ref_audio_price × refAudios
//	+ 参考视频费（按秒的 ref_video_second_price 或按条的 ref_video_price，二选一）
//
// 为什么超时溢价只乘时长费：参考素材是一次性成本，与视频长短无关；
// 乘它会重复加价，账单也无法向用户解释"为什么加价部分包含参考图"。
func videoCharge(sp model.SpecPrices, u Usage, strictLimits bool) (int64, error) {
	// 上限校验排在取价之前：max_seconds 是模型级约束，与档位是否开通无关。
	if strictLimits && sp.MaxSeconds != nil && *sp.MaxSeconds > 0 && u.VideoSeconds > *sp.MaxSeconds {
		return 0, fmt.Errorf("%w：%d 秒超过模型上限 %d 秒", ErrOverMaxSeconds, u.VideoSeconds, *sp.MaxSeconds)
	}
	unit, ok := sp.SecondPrice[u.VideoResolution]
	if !ok || unit < 0 {
		return 0, ErrInvalidTier
	}
	secs := clampSeconds(u.VideoSeconds)
	timeFee, err := money.MulDiv(unit, secs, 1)
	if err != nil {
		return 0, err
	}
	if mult := longVideoMultiplier(sp, secs); mult > 0 {
		if timeFee, err = money.Mul(timeFee, mult); err != nil {
			return 0, err
		}
	}
	refImageFee, err := refFee(sp.RefImagePrice, billableImages(u.RefImages, sp.RefImageFree))
	if err != nil {
		return 0, err
	}
	refAudioFee, err := refFee(sp.RefAudioPrice, refCount(u.RefAudios))
	if err != nil {
		return 0, err
	}
	refVideoFee := int64(0)
	if len(sp.RefVideoSecondPrice) > 0 {
		// 参考视频秒数是**多段素材的累计值**，不能套用单条视频的 3600 秒上限：
		// 10 段 10 分钟的参考素材共 6000 秒，夹到 3600 就是白送 40% 的素材费。
		refVideoFee, err = refFee(ptrOf(refVideoSecondPriceAt(sp, u.VideoResolution)),
			ClampRefVideoSeconds(u.RefVideoSeconds, u.RefVideos))
	} else {
		refVideoFee, err = refFee(sp.RefVideoPrice, refCount(u.RefVideos))
	}
	if err != nil {
		return 0, err
	}
	return money.Sum(timeFee, refImageFee, refAudioFee, refVideoFee)
}

// refVideoSecondPriceAt 按输出档位取参考视频秒价；未配置该档返回 0（= 该档参考视频免费）。
//
// 「缺档 = 免费」是**有意的产品口径**（同一模型不同档可以有不同素材策略），
// 不是漏算。风险在于"运营只配了 720p、用户跑 1080p"时素材费整笔归零，
// 所以保存期会回一条 warning 提示哪些档位没有参考视频价（见 specprices.go），
// 运行期不擅自改成"回落最高档"—— 那会与显式配 0 的免费策略打架。
func refVideoSecondPriceAt(sp model.SpecPrices, tier string) int64 {
	v, ok := sp.RefVideoSecondPrice[tier]
	if !ok || v < 0 {
		return 0
	}
	return v
}

func ptrOf(v int64) *int64 { return &v }

// refFee 计算素材费：单价缺失/负数 = 免费档；数量为 0 = 0。
func refFee(unit *int64, count int64) (int64, error) {
	if unit == nil || *unit <= 0 || count <= 0 {
		return 0, nil
	}
	return money.MulDiv(*unit, count, 1)
}

// clampSeconds 把秒数限制在 [0, MaxVideoSeconds]（负数按 0，超限饱和）。
// ClampRefVideoSeconds 钳参考视频的**累计**秒数：上限 = 段数 × 单条上限。
//
// 不能直接套 clampSeconds（单条 3600 秒）：那是"一段视频最多多长"，
// 而这里是多段素材的总时长 —— 10 段 10 分钟 = 6000 秒，按 3600 截就是白送 40%。
// 也不能完全放开：上游回报的 InputSeconds 可能是谎报（曾见 86400），
// 所以上限跟着**提交时的段数**走（至少按 1 段算，且最多计 64 段防溢出）。
func ClampRefVideoSeconds(v, refVideos int64) int64 {
	if v < 0 {
		return 0
	}
	n := refVideos
	if n < 1 {
		n = 1
	}
	if n > 64 {
		n = 64
	}
	if limit := int64(MaxVideoSeconds) * n; v > limit {
		return limit
	}
	return v
}

func clampSeconds(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > MaxVideoSeconds {
		return MaxVideoSeconds
	}
	return v
}

// billableImages 返回计费张数：refImageFree 为 nil 或 0 = 无免费张数。
func billableImages(images int64, free *int64) int64 {
	n := refCount(images)
	if free == nil || *free <= 0 {
		return n
	}
	if n <= *free {
		return 0
	}
	return n - *free
}

// longVideoMultiplier 返回超时溢价比例（微）；不适用时返回 0。
// 阈值与倍率必须同时有效（阈值 >0 且倍率 >1e6 即 >1 倍）才生效。
func longVideoMultiplier(sp model.SpecPrices, secs int64) int64 {
	if sp.LongAfterSeconds == nil || sp.LongVideoMultiplier == nil {
		return 0
	}
	if *sp.LongAfterSeconds <= 0 || *sp.LongVideoMultiplier <= money.Scale {
		return 0
	}
	if secs <= *sp.LongAfterSeconds {
		return 0
	}
	return *sp.LongVideoMultiplier
}

// refCount 返回素材个数（负数按 0）。
func refCount(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// 档位来源（落 usages.tier_source）。空串 = 该快照不按档位计价（按量计费）。
const (
	TierSourceFixed      = "fixed"      // 按次，price_call 固定价
	TierSourceResolution = "resolution" // 命中规格档位
	TierSourceDefault    = "default"    // 图片未写尺寸，走默认档
	TierSourceHighest    = "highest"    // 图片请求的档位不在价目，走最高价档
	TierSourcePerTask    = "per_task"   // 一次任务固定价
	TierSourcePerClip    = "per_clip"   // 按实际交付条数（音乐/歌词）
)

// TierInfo 返回本次计费的**档位单价**与**档位来源**，仅用于把金额解释清楚
// （落 usages.tier_price / tier_source），**绝不参与金额计算**。
//
// 返回的是折扣前的配置单价：前端要展示"实际每秒多少钱"时必须自己按
// snapshot.discount 折算，而不是再乘一次快照里的折扣（那是重复计算）。
func TierInfo(s Snapshot, u Usage) (unit int64, source string) {
	switch s.BillingMode {
	case model.BillingModeCall:
		if s.Price.Call == nil {
			return 0, ""
		}
		return *s.Price.Call, TierSourceFixed
	case model.BillingModeSpec:
		sp := s.SpecPrice
		switch sp.Kind {
		case SpecKindPerImage:
			// 与 imageCharge / ChargeCeiling 同一个取价函数：三处口径不可能不同。
			v, src, _ := priceOfImage(sp, u.ImageSpec)
			return v, src
		case SpecKindPerVideo:
			if v, ok := sp.SecondPrice[u.VideoResolution]; ok {
				return v, TierSourceResolution
			}
			return 0, ""
		case SpecKindPerTask:
			if sp.PerTaskPrice != nil {
				return *sp.PerTaskPrice, TierSourcePerTask
			}
			return 0, ""
		case SpecKindPerClip:
			if sp.PerClipPrice != nil {
				return *sp.PerClipPrice, TierSourcePerClip
			}
			return 0, ""
		}
	}
	return 0, ""
}

// ChargeCeiling 返回「按当前价目，这一单最多可能收多少」（微）。0 = 无法给出上界。
//
// 唯一用途是结算期的金额上限。原来的上限只有 3×冻结额，而冻结额本身可能严重偏低：
//   - ASR 的时长在冻结期不存在 → 冻结 0 → 上限退化成「零冻结兜底 2 元」；
//   - 图片按 n 冻结、上游多出图 → 结算额天然大于冻结额。
//
// 结果都是「合法金额被上限削掉」，日志里是一堆 charge_capped，而金额看起来毫无异常。
//
// 算法是把用量替换成**配置允许的最坏值**再走同一套取价函数（imageCharge /
// videoCharge / perSecondCharge），绝不另写一遍公式：上限与实收各用一套价格，
// 比截顶本身更危险（上限高于实收是无声多收，低于实收是无声少收）。
//
// 只对 per_* 规格计价生效；tokens 没有「配置上限」这种东西，返回 0 表示不参与。
func ChargeCeiling(s Snapshot, u Usage) (int64, string) {
	if s.BillingMode != model.BillingModeSpec {
		return 0, ""
	}
	sp := s.SpecPrice
	var (
		amount int64
		err    error
		reason string
	)
	switch sp.Kind {
	case SpecKindPerSecond:
		if sp.MaxSeconds == nil || sp.PerSecondPrice == nil {
			return 0, ""
		}
		w := u
		w.AudioDurationSecs, w.AudioChars = *sp.MaxSeconds, 0
		amount, _, err = perSecondCharge(sp, w, false)
		reason = "per_second_max_seconds"
	case SpecKindPerImage:
		// 上界必须按**最高单价**算：请求低价档时若按命中档算上界，
		// 上游交高档就会被 3× 冻结截顶（系统性少收）。
		//
		// max_n 缺失时用引擎上限（而不是"不给上界"）：漏配 max_n 的模型冻结只有 1 张，
		// 若上界也放弃，3× 冻结会把多交付的图全部削掉（少收）；而 imageCharge 的
		// 饱和上限本来就是 MaxImageN —— 两处必须同一口径。
		top, _, hit := highestPricedTier(sp)
		if !hit {
			return 0, ""
		}
		w := u
		w.ImageSpec = top
		if sp.MaxN != nil && *sp.MaxN >= 1 {
			w.ImageN = *sp.MaxN
			reason = "per_image_max_n"
		} else {
			w.ImageN = MaxImageN
			reason = "per_image_engine_max_n"
		}
		amount, _, _, err = imageCharge(sp, w, false)
	case SpecKindPerVideo:
		if sp.MaxSeconds == nil {
			return 0, ""
		}
		w := u
		w.VideoSeconds = *sp.MaxSeconds
		amount, err = videoCharge(sp, w, false)
		reason = "per_video_max_seconds"
	case SpecKindPerTask:
		amount, err = perTaskCharge(sp)
		reason = "per_task"
	case SpecKindPerClip:
		// 上界 = 每条单价 × 计费条数上限。max_n 缺失时用引擎上限 2（SUNO 默认）：
		// 冻结本来就按 2 条占额度，上界若放弃，3× 冻结会把多交付的条数削掉（少收）。
		w := u
		w.ImageN = MusicClipFreezeCount(sp)
		amount, _, err = perClipCharge(sp, w, false)
		reason = "per_clip_max_n"
	default:
		return 0, ""
	}
	if err != nil || amount <= 0 {
		// 价目在这个档位上取不到价（档位未开通/价目缺失）：不给上界，
		// 由 3×冻结兜底。宁可维持现状，也不要把上限算成一个错的值。
		return 0, ""
	}
	// 上限必须与实收同口径。ChargeDetailed = 取价 → 打折 → 抬到最低消费 → 截顶，
	// 上面 5 个分支只做了第一步，所以漏掉后两步时「上限」实际是**未打折的原价**：
	//   加价分组（1.2×）：实收 12000、上限 10000 → 收的钱被自己的上限削到 10000，
	//   而 cap_reason 记成 configured_max，把「口径不一致」伪装成「配置就这么贵」。
	// 少收方向既不告警也不会被对账发现（对账只比 charge 与流水，两者一致）。
	amount, err = applyDiscountAndFloor(s, amount)
	if err != nil || amount <= 0 {
		return 0, ""
	}
	if err := money.Check(amount); err != nil {
		return 0, ""
	}
	if amount > maxCharge() {
		amount = maxCharge()
	}
	return amount, reason
}

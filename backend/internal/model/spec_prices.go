package model

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// SpecPrices 是 models.spec_prices（JSONB）的**唯一结构定义**。
//
// 键集 = 本结构体的 json tag，任何新增价目键必须加到这里，然后
// billing.ValidateSpecPrices / 管理台 DTO / 前端类型都从同一处派生。
// 历史教训：旧实现用 json.RawMessage + 逐键 map 取值，键名以字符串字面量散落在
// 5 个文件里，改名时必然漏掉一处，症状是"保存成功但计费读到 nil"。
//
// 单位：全部是**微**（1e-6 结算货币单位），与 Prices 同单位。
// 旧键名带 `_quota` 后缀的写法已全部废弃：如果沿用旧键名，灰度期会出现
// "读到旧值是 quota、按微解释"的 100 倍错账；键名全换后未迁移的行会被
// 严格解码直接拒绝（fail-closed），不会静默错。
type SpecPrices struct {
	Kind string `json:"kind,omitempty"` // per_image / per_video / per_task / per_second / per_clip

	// —— per_image（微 / 张）——
	// 键由运营自由配置（建议 1K / 1.5K / 2K / 3K / 4K，见 billing.ImageTierKeys）。
	// 1.5K 是正式档位（官方：与 1K 同价、效果更优），不是 1K 的别名。
	TierPrice map[string]int64 `json:"tier_price,omitempty"`
	MaxN      *int64           `json:"max_n,omitempty"` // 单次张数上限

	// —— per_video（微 / 秒、微 / 张、微 / 条）——
	SecondPrice map[string]int64 `json:"second_price,omitempty"` // 键：480p / 720p / 1080p / 2k / 4k（白名单见 billing.VideoTierKeys）
	MaxSeconds  *int64           `json:"max_seconds,omitempty"`  // 模型级时长上限
	// AllowAutoDuration 表示该模型支持「智能时长」（如 Ark 的 duration=-1）。
	//
	// 为什么必须做成模型级开关而不是"所有视频都放开 -1"：
	// 其它厂商的上游会直接 reject 一个 -1，那会被记成渠道故障 → 误熔断健康渠道。
	// 这里和 max_seconds / resolution_fixed 同层：它们都是**模型事实**，不是渠道配置。
	//
	// 只在 video 模态有意义（image/music/audio 的校验会拒绝这个键）。
	AllowAutoDuration *bool `json:"allow_auto_duration,omitempty"`
	// ResolutionFixed 是「请求没写分辨率时按哪一档计费」，两种模态共用同一个字段：
	//   - 视频：画质由型号决定，必须命中 second_price；
	//   - 图片：**默认档**，必须命中 tier_price；留空时只有一档才能自动确定，
	//     多档则「不传 size」的请求会被拒绝（运行期不替用户挑档）。
	//
	// 为什么不给图片另立一个 default_tier：那会变成"两个字段都表示默认档"，
	// 两边一旦不一致就没人说得清哪个算数。档位查找一律走大小写不敏感匹配，
	// 因此这里的大小写不影响命中（NormalizeSpecPrices 对视频会转小写）。
	ResolutionFixed string `json:"resolution_fixed,omitempty"`

	// 超时溢价：必须成对配置，且倍率 > 1。倍率以微比例存储（1.5 → 1500000），
	// 与 money.Mul 同一单位体系 —— 金额路径里绝不出现 float64。
	LongAfterSeconds    *int64 `json:"long_after_seconds,omitempty"`
	LongVideoMultiplier *int64 `json:"long_video_multiplier,omitempty"`

	// 参考素材：单价 + 免计张数。
	// RefImageFree 语义：前 N 张免费，**0 = 无免费**（旧 free_ref_images 的
	// "0 = 全部免费"是反直觉陷阱，已取消；要全部免费就把单价填 0）。
	RefImagePrice *int64 `json:"ref_image_price,omitempty"`
	RefImageFree  *int64 `json:"ref_image_free,omitempty"`
	RefAudioPrice *int64 `json:"ref_audio_price,omitempty"`
	// 参考视频两套口径互斥：按条（RefVideoPrice）或按秒（RefVideoSecondPrice）。
	// 同时非空 = 保存期直接拒绝，消除"哪个非空用哪个"的隐式优先级。
	RefVideoPrice       *int64           `json:"ref_video_price,omitempty"`
	RefVideoSecondPrice map[string]int64 `json:"ref_video_second_price,omitempty"`

	// —— per_task（微 / 任务）——
	PerTaskPrice *int64 `json:"per_task_price,omitempty"`

	// —— per_clip（微 / 条）——
	//
	// 音乐/歌词专用：一次任务可能交付 1~2 首（歌词 1 篇），按**实际交付条数**计费。
	// 为什么不能复用 per_image：那条链路绑死"档位 × 张数"，计价要命中 tier_price、
	// 且 ResolveImageCount 会把 0 张夹成 1 张 —— 与"0 条交付免单"直接冲突。
	// 为什么不能继续用 per_task：上游出 1 首还是 2 首都收同一价时，
	// 少交付的那一半无法解释，也无法与"按实际交付"的行业口径对齐。
	//
	// MaxN 在 per_clip 下是「单次任务的计费条数上限」（SUNO 实际 2 首），
	// 冻结期按它折算，结算期按实际条数收，两者都不允许超过它。
	PerClipPrice *int64 `json:"per_clip_price,omitempty"`

	// —— per_second（微 / 秒）——
	// 音频（TTS/ASR）按秒计价：上游普遍按"生成音频时长"结算，
	// 按次计价在长短文本混用时会系统性错账（一条 5 秒广告语和一段 10 分钟有声书同价）。
	PerSecondPrice *int64 `json:"per_second_price,omitempty"`
	// MinPrice 是单次最低消费（微）。上游几乎都有最低计费时长
	// （如"¥0.001/秒，最低 ¥0.01"= 至少按 10 秒收），不配最低价会让短文本少收。
	MinPrice *int64 `json:"min_price,omitempty"`
}

// IsZero 报告该价目是否为空（用于决定列值写 NULL 而不是 "{}"）。
func (s SpecPrices) IsZero() bool {
	return s.Kind == "" && len(s.TierPrice) == 0 && s.MaxN == nil &&
		len(s.SecondPrice) == 0 && s.MaxSeconds == nil && s.AllowAutoDuration == nil &&
		s.ResolutionFixed == "" &&
		s.LongAfterSeconds == nil && s.LongVideoMultiplier == nil &&
		s.RefImagePrice == nil && s.RefImageFree == nil && s.RefAudioPrice == nil &&
		s.RefVideoPrice == nil && len(s.RefVideoSecondPrice) == 0 && s.PerTaskPrice == nil &&
		s.PerClipPrice == nil && s.PerSecondPrice == nil && s.MinPrice == nil
}

// specPricesAlias 是 SpecPrices 的**无方法**别名。
//
// 必须存在：DecodeSpecPrices 若直接 decode 到 *SpecPrices，会立刻回调
// UnmarshalJSON → DecodeSpecPrices → 无限递归（栈溢出，不是编译错误）。
type specPricesAlias SpecPrices

// DecodeSpecPrices 严格解码 spec_prices（未知键直接报错）。
//
// 严格是刻意的：旧键名（resolution_prices_quota / free_ref_images / ...）必须
// 立刻暴露，而不是被静默忽略后按"未配置"计费 —— 后者会让收费模型变成免费。
func DecodeSpecPrices(raw []byte) (SpecPrices, error) {
	var out specPricesAlias
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return SpecPrices{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		// 这句话是给运营和 AI 看的，必须说出"怎么改"，而不是只抛 Go 的英文：
		// 最常见的两种错是「把元写成小数」和「用了废弃的 _quota 后缀键名」。
		return SpecPrices{}, fmt.Errorf(
			"spec_prices 结构非法（价格必须是整数微、键名不能带 _quota 后缀、不能出现未定义键）：%w", err)
	}
	return SpecPrices(out), nil
}

func (s *SpecPrices) UnmarshalJSON(b []byte) error {
	v, err := DecodeSpecPrices(b)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

func (s SpecPrices) MarshalJSON() ([]byte, error) {
	type alias SpecPrices
	if s.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(alias(s))
}

// Value 让空价目落成 SQL NULL 而不是 JSONB 'null'（两者在 `= null` 判断里不等价）。
func (s SpecPrices) Value() (driver.Value, error) {
	if s.IsZero() {
		return nil, nil
	}
	type alias SpecPrices
	b, err := json.Marshal(alias(s))
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (s *SpecPrices) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*s = SpecPrices{}
		return nil
	case []byte:
		return s.decodeInto(v)
	case string:
		return s.decodeInto([]byte(v))
	default:
		return fmt.Errorf("model.SpecPrices: 无法从 %T 扫描", value)
	}
}

// specPricesScanFailures 累计 Scan 阶段严格解码失败的行数（进程内）。
//
// Scan **不能**把错误往上抛：一行坏数据会让整张 models 表的查询失败，
// 于是价目目录整体不可用（全站 500）。正确做法是让那一行退化成空价目，
// 由计费侧按 ErrPriceMissing 拒绝该模型（fail-closed 到"不可用"，
// 而不是 fail-closed 到"全站不可用"）；启动体检再用 UnparsableSpecModels
// 把坏行点名报出来。
var specPricesScanFailures atomic.Int64

// SpecPricesScanFailures 返回本次进程内 Scan 解码失败的累计行数（供启动日志使用）。
func SpecPricesScanFailures() int64 { return specPricesScanFailures.Load() }

func (s *SpecPrices) decodeInto(raw []byte) error {
	v, err := DecodeSpecPrices(raw)
	if err != nil {
		specPricesScanFailures.Add(1)
		*s = SpecPrices{}
		return nil
	}
	*s = v
	return nil
}

func (SpecPrices) GormDataType() string { return "jsonb" }

func (SpecPrices) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "postgres" {
		return "JSONB"
	}
	return "JSON"
}

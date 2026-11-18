package appplugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/ulid"
)

// 领域哨兵。controller 用 DefOr 一次映射成对外错误码，避免每个 handler 各写 switch。
var (
	ErrNotFound         = errors.New("appplugins: 插件不存在")
	ErrNotInstalled     = errors.New("appplugins: 该应用未安装此插件")
	ErrAlreadyInstalled = errors.New("appplugins: 该应用已安装此插件")
	ErrDisabled         = errors.New("appplugins: 插件当前不可用")
	ErrEntityNotFound   = errors.New("appplugins: 插件里没有这个数据对象")
	ErrRecordNotFound   = errors.New("appplugins: 记录不存在")
	ErrRecordConflict   = errors.New("appplugins: 该值已被占用")
	ErrVersionConflict  = errors.New("appplugins: 版本已变化")
	ErrLimitExceeded    = errors.New("appplugins: 超出容量上限")
	ErrInUse            = errors.New("appplugins: 仍有应用在使用该插件")
	// ErrPurgeNotAllowed：清除数据必须先卸载。防的是"手指一滑把在用的数据删了"。
	ErrPurgeNotAllowed = errors.New("appplugins: 请先卸载再清除数据")
)

// InvalidError 是**可批量展示**的校验失败（422 + data.problems）。
//
// Breaking 用来区分"第一次就没写对"与"这次升级会破坏存量数据"：
// 后者需要管理员显式勾选 force，前端据它决定显不显示那个勾选框。
type InvalidError struct {
	Problems []Problem
	Breaking bool
}

func (e *InvalidError) Error() string {
	if len(e.Problems) == 0 {
		return "appplugins: 定义不合法"
	}
	return "appplugins: " + e.Problems[0].Message
}

// InUseError 带上引用数：只说"仍在使用"运营无法判断影响面。
type InUseError struct{ Count int64 }

func (e *InUseError) Error() string {
	return fmt.Sprintf("仍有 %d 个应用在使用该插件", e.Count)
}

// DisabledError 是"不可用"的带原因版本：停用、下架、未卸载就想清数据的文案
// 各不相同，混用一句"该插件当前不可用"会让人不知道该点哪里。
type DisabledError struct{ Msg string }

func (e *DisabledError) Error() string { return e.Msg }

func disabledErr(msg string) *DisabledError { return &DisabledError{Msg: msg} }

// LimitError 分两种场景：写入超容量（可以删几条继续）与导出超上限
// （只能缩小范围）。文案不同，所以不能用同一个哨兵。
type LimitError struct {
	Max    int64
	Export bool
}

func (e *LimitError) Error() string {
	if e.Export {
		return fmt.Sprintf("导出最多 %d 行，请加关键词缩小范围或按对象分别导出", e.Max)
	}
	return fmt.Sprintf("这个数据对象最多 %d 条记录，请先删除不再需要的数据", e.Max)
}

// MaxExportRows 是单次导出的硬上限。
//
// 为什么不是"流式想导多少导多少"：CSV 一旦开始写就无法改状态码，
// 超时/断连会留下一个看起来正常的半截文件，运营拿去做统计不会发现缺行。
const MaxExportRows = 50000

// DefOr 把领域哨兵映射成对外错误定义；未知错误交给调用方兜底（通常 Internal）。
func DefOr(err error, fallback errs.Def) errs.Def {
	var invalid *InvalidError
	var inUse *InUseError
	var disabled *DisabledError
	var limit *LimitError
	switch {
	case errors.As(err, &invalid):
		return errs.PluginManifestInvalid
	case errors.As(err, &inUse):
		return errs.PluginInUse.WithMessage(inUse.Error())
	case errors.As(err, &disabled):
		return errs.PluginDisabled.WithMessage(disabled.Error())
	case errors.As(err, &limit):
		return errs.PluginLimitExceeded.WithMessage(limit.Error())
	case errors.Is(err, ErrNotFound):
		return errs.PluginNotFound
	case errors.Is(err, ErrNotInstalled):
		return errs.PluginNotInstalled
	case errors.Is(err, ErrAlreadyInstalled):
		return errs.PluginAlreadyInstalled
	case errors.Is(err, ErrDisabled), errors.Is(err, ErrPurgeNotAllowed):
		return errs.PluginDisabled.WithMessage(err.Error())
	case errors.Is(err, ErrEntityNotFound):
		return errs.PluginEntityNotFound
	case errors.Is(err, ErrRecordNotFound):
		return errs.PluginRecordNotFound
	case errors.Is(err, ErrRecordConflict):
		return errs.PluginRecordConflict
	case errors.Is(err, ErrVersionConflict):
		return errs.PluginVersionConflict
	case errors.Is(err, ErrLimitExceeded):
		return errs.PluginLimitExceeded
	}
	return fallback
}

// NewRecordKey 生成对外记录标识。ULID 是大写，全链路不要折叠大小写：
// 它进 URL 与审计 target_id，大小写不一致会让"同一条记录"在两个地方对不上。
func NewRecordKey() string { return ulid.New() }

// normalizeFields 是做"逐字段校验 + 去空值 + 只留声明字段"的**唯一**实现。
//
// 三件事一起做是刻意的：分成三遍就会出现"校验通过但存了未声明字段"
// 或"存了空串但读出来当成有值"。pathPrefix 让同一套逻辑既能校验记录（data.）
// 也能校验插件配置（config.）。
func normalizeFields(fields []Field, raw map[string]any, pathPrefix string) (map[string]any, []Problem) {
	var ps []Problem
	out := make(map[string]any, len(fields))
	declared := make(map[string]bool, len(fields))
	for i := range fields {
		declared[fields[i].Key] = true
	}
	for k := range raw {
		if !declared[k] {
			ps = append(ps, problem(pathPrefix+k, "没有这个字段"))
		}
	}
	for i := range fields {
		f := &fields[i]
		v, present := raw[f.Key]
		norm, msg := normalizeField(f, v, present)
		if msg != "" {
			ps = append(ps, problem(pathPrefix+f.Key, "%s", msg))
			continue
		}
		if norm == nil {
			if f.Required {
				ps = append(ps, problem(pathPrefix+f.Key, "必填"))
			}
			continue
		}
		out[f.Key] = norm
	}
	return out, ps
}

// normalizeField 返回规范化后的值（nil = 没有值）或一句人话错误。
func normalizeField(f *Field, v any, present bool) (any, string) {
	if !present || v == nil {
		return nil, ""
	}
	switch f.Type {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, "需要文本"
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, ""
		}
		if strings.ContainsAny(s, "\n\r") {
			return nil, "不能包含换行"
		}
		if utf8.RuneCountInString(s) > f.MaxLen {
			return nil, fmt.Sprintf("最多 %d 个字符", f.MaxLen)
		}
		return s, ""
	case TypeText:
		s, ok := v.(string)
		if !ok {
			return nil, "需要文本"
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, ""
		}
		if utf8.RuneCountInString(s) > f.MaxLen {
			return nil, fmt.Sprintf("最多 %d 个字符", f.MaxLen)
		}
		return s, ""
	case TypeInt:
		n, ok := asInt(v)
		if !ok {
			return nil, "需要整数"
		}
		return n, ""
	case TypeBool:
		b, ok := v.(bool)
		if !ok {
			return nil, "需要 true / false"
		}
		return b, ""
	case TypeEnum:
		s, ok := v.(string)
		if !ok {
			return nil, "需要文本"
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, ""
		}
		for _, o := range f.Options {
			if o == s {
				return s, ""
			}
		}
		return nil, fmt.Sprintf("只能是 %s 之一", strings.Join(f.Options, " / "))
	case TypeDate:
		s, ok := v.(string)
		if !ok {
			return nil, "需要日期"
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, ""
		}
		if _, err := parseDate(s); err != nil {
			return nil, "日期格式需要 YYYY-MM-DD"
		}
		return s, ""
	case TypeLink:
		s, ok := v.(string)
		if !ok {
			return nil, "需要链接"
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, ""
		}
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
			return nil, "链接需要以 http:// 或 https:// 开头"
		}
		if len(s) > f.MaxLen {
			return nil, fmt.Sprintf("最多 %d 个字符", f.MaxLen)
		}
		return s, ""
	}
	return nil, "不支持的类型"
}

// maxSafeInt 是 JSON 数字能精确表示的上界（2^53-1）。
// 超过它的整数在不同解析器上会悄悄变成另一个数，宁可直接拒。
const maxSafeInt = int64(1)<<53 - 1

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil || i > maxSafeInt || i < -maxSafeInt {
			return 0, false
		}
		return i, true
	case float64:
		if n != math.Trunc(n) || n > float64(maxSafeInt) || n < -float64(maxSafeInt) {
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// uniqueNormOf 计算业务唯一值。归一规则只有一条：两端空白与大小写不参与比较。
//
// 只归一不复写 data：展示值保留作者原本的大小写（"CN-001" 不该在页面上变成 "cn-001"），
// 唯一性由 unique_norm 承担。两者不同源是刻意的，注释写在这里免得后人合并。
func uniqueNormOf(e *Entity, data map[string]any) *string {
	if e.Unique == "" {
		return nil
	}
	v, ok := data[e.Unique]
	if !ok {
		return nil
	}
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case int64:
		s = fmt.Sprintf("%d", t)
	default:
		s = fmt.Sprint(t)
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > MaxUniqueLen {
		// 校验阶段已经拦住（unique 字段 maxLen ≤ 64），这里只做兜底截断：
		// 截断会制造"两个不同值变成同一个 key"，所以宁可整值返回让唯一索引报冲突。
		return &s
	}
	return &s
}

// DecodeData 把请求体里的 data 解成 map，保留数字精度（json.Number）。
func DecodeData(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]any{}, nil
	}
	return m, nil
}

// DecodeObject 同 DecodeData，但用于配置对象（语义相同，命名让调用点更清楚）。
func DecodeObject(raw json.RawMessage) (map[string]any, error) { return DecodeData(raw) }

// MustJSON 把规范化后的 map 编码成可入库的 JSON。
// 编码的是"理解后的值"而不是原始字节：这样库里不会留下 `"12"` 这种
// 通过了校验但类型与声明不符的历史包袱。
func MustJSON(v any) ([]byte, error) { return json.Marshal(v) }

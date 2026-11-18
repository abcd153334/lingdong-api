package relay

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"lingdong-api/internal/apidoc"
)

// 本文件实现渠道级「声明式适配配置」：让运营用配置接入「结构接近 OpenAI、
// 但路径/鉴权/字段不同」的上游，而不必写 Python 插件。
//
// 三条硬边界（改之前先读）：
//  1. 只作用于内置适配器（实现 DeclarativeSupporter 的那些）；插件渠道由 manifest 管理。
//  2. 请求侧改写只对 JSON 体生效；multipart 端点静默跳过（不是失败）。
//  3. 响应侧映射只作用于非流式 chat 的 2xx；流式帧与错误体一律不动。

// canonical 端点路径。值全部来自 internal/apidoc 的端点表（那里是唯一定义处），
// 这里只是给 path_overrides 与适配器一个可读别名。
//
// 为什么从 const 改成 var：常量无法由函数求值，而这份字面量曾经是**第二处定义**。
// 后果很隐蔽——path_overrides 的键取错不会报错，只是永远不生效（运营配了覆盖却毫无作用，
// 排查时两边都"看起来没错"）。改由 apidoc 派生后，改端点路径只需改一处。
var (
	PathModels              = mustPath(apidoc.KindModelsList)
	PathChatCompletions     = mustPath(apidoc.KindChatCompletions)
	PathCompletions         = mustPath(apidoc.KindCompletions)
	PathEmbeddings          = mustPath(apidoc.KindEmbeddings)
	PathImagesGenerations   = mustPath(apidoc.KindImagesGenerations)
	PathImagesEdits         = mustPath(apidoc.KindImagesEdits)
	PathImagesVariations    = mustPath(apidoc.KindImagesVariations)
	PathAudioSpeech         = mustPath(apidoc.KindAudioSpeech)
	PathAudioTranscriptions = mustPath(apidoc.KindAudioTranscriptions)
	PathAudioTranslations   = mustPath(apidoc.KindAudioTranslations)
	PathModerations         = mustPath(apidoc.KindModerations)
	PathRerank              = mustPath(apidoc.KindRerank)
	PathResponses           = mustPath(apidoc.KindResponses)
	PathResponsesCompact    = mustPath(apidoc.KindResponsesCompact)
	PathVideoSubmit         = mustPath(apidoc.KindVideoSubmit)
	PathVideoPoll           = apidoc.UpstreamVideoPollPath
)

// mustPath 取端点表里的上游路径；缺契约是编程错误（测试会先红），
// 这里 panic 而不是返回空串——空路径会让 BuildURL 拼出上游根地址，全量请求打错地方。
// 实现直接委托 apidoc.MustPath，避免"两处各自 panic 一句话"的分叉。
func mustPath(k apidoc.Kind) string { return apidoc.MustPath(k) }

// canonicalPaths 是 path_overrides 允许的键集合。
// 键取错不会报错但永远不生效，所以保存期必须拦（见 ValidateAdapterConfig）。
var canonicalPaths = map[string]bool{
	PathModels: true, PathChatCompletions: true, PathCompletions: true, PathEmbeddings: true,
	PathImagesGenerations: true, PathImagesEdits: true, PathImagesVariations: true,
	PathAudioSpeech: true, PathAudioTranscriptions: true, PathAudioTranslations: true,
	PathModerations: true, PathRerank: true, PathResponses: true, PathResponsesCompact: true,
	PathVideoSubmit: true, PathVideoPoll: true,
}

// CanonicalPathList 返回全部可覆盖端点（供错误文案与前端提示使用，已排序）。
func CanonicalPathList() []string {
	out := make([]string, 0, len(canonicalPaths))
	for p := range canonicalPaths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// AdapterConfig 是 channels.adapter_config 的结构化视图（零值 = 完全关闭）。
type AdapterConfig struct {
	AuthHeader    string                     `json:"auth_header,omitempty"`
	AuthPrefix    string                     `json:"auth_prefix,omitempty"`
	PathOverrides map[string]string          `json:"path_overrides,omitempty"`
	RequestPatch  map[string]json.RawMessage `json:"request_patch,omitempty"`
	ResponsePath  *ResponsePath              `json:"response_path,omitempty"`
}

// ResponsePath 描述「从非标准响应里取字段」的映射（只作用于非流式 chat）。
type ResponsePath struct {
	Content          string `json:"content"`
	PromptTokens     string `json:"prompt_tokens,omitempty"`
	CompletionTokens string `json:"completion_tokens,omitempty"`
}

// IsZero 判断配置是否为空（空 = 行为与改造前完全一致）。
func (c AdapterConfig) IsZero() bool {
	return c.AuthHeader == "" && c.AuthPrefix == "" && len(c.PathOverrides) == 0 &&
		len(c.RequestPatch) == 0 && c.ResponsePath == nil
}

// ConfigError 是「配置本身有问题」的错误。
//
// Class()="config" → classifyError → outcomeInternal：**不记渠道健康、不冷却密钥**。
// 否则一条写错的 response_path 就能把好渠道自动熔断，而运营只会以为"上游挂了"。
type ConfigError struct {
	Field string
	Msg   string
	// Scope 是报错前缀（如「渠道适配器配置错误」）。
	//
	// 留空 = 声明式配置（历史行为，文案逐字不变）：同一个类型被
	// adapter_config 与 adapter_options 两处复用，前缀写死会让运营改错地方。
	Scope string
}

func (e *ConfigError) Error() string {
	prefix := "渠道声明式配置错误"
	if strings.TrimSpace(e.Scope) != "" {
		prefix = e.Scope
	}
	if e.Field == "" {
		return prefix + "：" + e.Msg
	}
	return prefix + "(" + e.Field + ")：" + e.Msg
}

// Class 实现 relay.errKind（见 outcome.go 的 classifyError）。
func (e *ConfigError) Class() string { return "config" }

var (
	authHeaderPattern   = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	patchSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	indexSegmentPattern = regexp.MustCompile(`^[0-9]{1,6}$`)
)

// forbiddenAuthHeaders 是绝不能由配置改写的头（协议语义由 Transport 决定）。
var forbiddenAuthHeaders = map[string]bool{
	"host": true, "content-length": true, "content-type": true,
	"transfer-encoding": true, "connection": true, "accept": true, "user-agent": true,
}

// reservedPatchTopKeys 禁止 request_patch 触碰的顶层键。
//
// model：计费用客户端模型定价、上游模型由绑定表写入，改了会让 usages.model 与实际调用不一致；
// stream：流式分叉在 dispatch 就定了，改了会让上游回 SSE 而代码按非流式收整段；
// messages/input/prompt：预扣估算在改写之前算完，改了会让预扣与实际系统性偏离；
// size/quality/n/seconds/resolution/response_format 等**计费维度**：同样在改写之前算完，
// 改了就是「上游按改后参数出图/出片，平台按改前收费」——差额由平台承担；
// max_tokens/max_completion_tokens/max_output_tokens：决定预扣额，改了会让预扣与结算系统性偏离。
var reservedPatchTopKeys = map[string]bool{
	"model": true, "stream": true, "messages": true, "input": true, "prompt": true,
	"size": true, "quality": true, "n": true, "seconds": true, "duration": true,
	"resolution": true, "response_format": true, "aspect_ratio": true,
	"max_tokens": true, "max_completion_tokens": true, "max_output_tokens": true,
}

const (
	maxAdapterConfigBytes = 32 << 10
	maxPatchEntries       = 64
	maxPatchBytes         = 16 << 10
	maxPatchDepth         = 8
	maxResponsePathDepth  = 12
	maxUpstreamPathLen    = 512
	maxAuthPrefixRunes    = 32
)

// ParseAdapterConfig 解析并校验 JSONB 列。空/NULL 返回零值。
//
// 运行期也走严格解析：脏数据（手工改库、旧版本残留）应当立刻失败，
// 而不是被静默当成"没配置"——那样排查时会以为配置生效了其实没有。
func ParseAdapterConfig(raw []byte) (AdapterConfig, error) {
	var out AdapterConfig
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return AdapterConfig{}, &ConfigError{Field: "adapter_config", Msg: "不是合法的配置对象：" + err.Error()}
	}
	if err := ValidateAdapterConfig(out); err != nil {
		return AdapterConfig{}, err
	}
	return out, nil
}

// ValidateAdapterConfig 是保存期与运行期共用的唯一校验入口。
//
// 必须在保存时拦截：配置错误若留到运行期，会被记成渠道失败并可能触发自动熔断，
// 而 JSONB 存进去不解析就永远不会报错（直到第一个真实请求）。
func ValidateAdapterConfig(c AdapterConfig) error {
	if err := validateAuth(c.AuthHeader, c.AuthPrefix); err != nil {
		return err
	}
	if err := validatePathOverrides(c.PathOverrides); err != nil {
		return err
	}
	if err := validateRequestPatch(c.RequestPatch); err != nil {
		return err
	}
	if err := validateResponsePath(c.ResponsePath); err != nil {
		return err
	}
	enc, err := json.Marshal(c)
	if err != nil {
		return &ConfigError{Field: "adapter_config", Msg: "无法序列化：" + err.Error()}
	}
	if len(enc) > maxAdapterConfigBytes {
		return &ConfigError{Field: "adapter_config", Msg: fmt.Sprintf("配置过大（%d 字节，上限 %d）", len(enc), maxAdapterConfigBytes)}
	}
	return nil
}

func validateAuth(header, prefix string) error {
	if header != "" {
		if !authHeaderPattern.MatchString(header) {
			return &ConfigError{Field: "auth_header", Msg: "头名只能是 1-64 位字母/数字/连字符"}
		}
		lower := strings.ToLower(header)
		if forbiddenAuthHeaders[lower] {
			return &ConfigError{Field: "auth_header", Msg: "该头由协议层管理，不能改写：" + header}
		}
		// HTTP/2 禁止 proxy-connection 这类头；运维填进来只会让上游直接拒连。
		if strings.HasPrefix(lower, "proxy-") {
			return &ConfigError{Field: "auth_header", Msg: "不支持 proxy-* 头"}
		}
	}
	if len([]rune(prefix)) > maxAuthPrefixRunes {
		return &ConfigError{Field: "auth_prefix", Msg: fmt.Sprintf("前缀不超过 %d 个字符", maxAuthPrefixRunes)}
	}
	if strings.ContainsAny(prefix, "\r\n\x00") {
		return &ConfigError{Field: "auth_prefix", Msg: "前缀不能包含换行或空字符（会造成头注入）"}
	}
	return nil
}

func validatePathOverrides(m map[string]string) error {
	for k, v := range m {
		if !canonicalPaths[k] {
			return &ConfigError{
				Field: "path_overrides",
				Msg:   fmt.Sprintf("未知端点「%s」；可用端点：%s", k, strings.Join(CanonicalPathList(), "、")),
			}
		}
		if err := validateUpstreamPath(k, v); err != nil {
			return &ConfigError{Field: "path_overrides." + k, Msg: err.Error()}
		}
	}
	return nil
}

// validateUpstreamPath 校验覆盖值。
//
// 安全核心：值会被直接拼到 base_url 后面（字符串拼接，不重新解析），
// 一旦允许 @ 或 //，拼出的 URL 的 **host 会变成攻击者**，渠道密钥随之泄露。
func validateUpstreamPath(canonical, v string) error {
	if v == "" {
		return fmt.Errorf("不能为空")
	}
	if len(v) > maxUpstreamPathLen {
		return fmt.Errorf("过长（上限 %d 字节）", maxUpstreamPathLen)
	}
	if !strings.HasPrefix(v, "/") {
		return fmt.Errorf("必须以 / 开头")
	}
	if strings.HasPrefix(v, "//") {
		return fmt.Errorf("不能以 // 开头（会被解析成其它主机）")
	}
	if strings.Contains(v, "://") {
		return fmt.Errorf("不能是绝对 URL（主机固定为渠道 Base URL）")
	}
	if strings.Contains(v, "@") {
		return fmt.Errorf("不能包含 @（会改变 URL 的主机部分）")
	}
	if strings.Contains(v, "..") {
		return fmt.Errorf("不能包含 ..")
	}
	if strings.Contains(v, "\\") {
		return fmt.Errorf("不能包含反斜杠")
	}
	if strings.Contains(v, "#") {
		return fmt.Errorf("不能包含 #（fragment 会截断路径）")
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("不能包含控制字符")
		}
		if r == ' ' || r == '\t' {
			return fmt.Errorf("不能包含空格")
		}
	}
	if strings.Contains(v, "{id}") && canonical != PathVideoPoll {
		return fmt.Errorf("{id} 占位符只能用于 %s", PathVideoPoll)
	}
	return nil
}

func validateRequestPatch(m map[string]json.RawMessage) error {
	if len(m) == 0 {
		return nil
	}
	if len(m) > maxPatchEntries {
		return &ConfigError{Field: "request_patch", Msg: fmt.Sprintf("最多 %d 项", maxPatchEntries)}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if ancestor, descendant, ok := ancestorConflict(keys); ok {
		return &ConfigError{
			Field: "request_patch",
			Msg:   fmt.Sprintf("「%s」与「%s」冲突（同一路径既有整体赋值又有子字段赋值，应用顺序不确定）", ancestor, descendant),
		}
	}
	total := 0
	for _, k := range keys {
		segs, err := splitDotPath(k, maxPatchDepth, false)
		if err != nil {
			return &ConfigError{Field: "request_patch." + k, Msg: err.Error()}
		}
		if reservedPatchTopKeys[segs[0]] {
			return &ConfigError{Field: "request_patch." + k, Msg: "不允许改写「" + segs[0] + "」（计费/流式/内容语义由系统管理）"}
		}
		v := m[k]
		if !json.Valid(v) {
			return &ConfigError{Field: "request_patch." + k, Msg: "值不是合法 JSON"}
		}
		total += len(k) + len(v)
	}
	if total > maxPatchBytes {
		return &ConfigError{Field: "request_patch", Msg: fmt.Sprintf("改写内容过大（%d 字节，上限 %d）", total, maxPatchBytes)}
	}
	return nil
}

func validateResponsePath(rp *ResponsePath) error {
	if rp == nil {
		return nil
	}
	if strings.TrimSpace(rp.Content) == "" {
		return &ConfigError{Field: "response_path.content", Msg: "必须指定正文所在路径"}
	}
	if _, err := splitDotPath(rp.Content, maxResponsePathDepth, true); err != nil {
		return &ConfigError{Field: "response_path.content", Msg: err.Error()}
	}
	hasPrompt := strings.TrimSpace(rp.PromptTokens) != ""
	hasCompletion := strings.TrimSpace(rp.CompletionTokens) != ""
	if hasPrompt != hasCompletion {
		return &ConfigError{
			Field: "response_path",
			Msg:   "prompt_tokens 与 completion_tokens 必须同时配置或同时留空（只配一个会导致计费口径不明）",
		}
	}
	if hasPrompt {
		if _, err := splitDotPath(rp.PromptTokens, maxResponsePathDepth, true); err != nil {
			return &ConfigError{Field: "response_path.prompt_tokens", Msg: err.Error()}
		}
		if _, err := splitDotPath(rp.CompletionTokens, maxResponsePathDepth, true); err != nil {
			return &ConfigError{Field: "response_path.completion_tokens", Msg: err.Error()}
		}
	}
	return nil
}

// splitDotPath 切分点分路径。allowIndex=true 时纯数字段视为数组下标（只读导航用）；
// false 时拒绝纯数字段（改写不支持数组定位，避免与「数字对象键」混淆）。
func splitDotPath(p string, maxDepth int, allowIndex bool) ([]string, error) {
	if p == "" {
		return nil, fmt.Errorf("路径不能为空")
	}
	segs := strings.Split(p, ".")
	if len(segs) > maxDepth {
		return nil, fmt.Errorf("层级过深（%d 层，上限 %d）", len(segs), maxDepth)
	}
	for _, s := range segs {
		if s == "" {
			return nil, fmt.Errorf("存在空层级（连续的点或首尾点）")
		}
		if indexSegmentPattern.MatchString(s) {
			if !allowIndex {
				return nil, fmt.Errorf("层级「%s」是纯数字：请求改写不支持数组定位", s)
			}
			continue
		}
		if !patchSegmentPattern.MatchString(s) {
			return nil, fmt.Errorf("层级「%s」含非法字符（只允许字母/数字/下划线/连字符）", s)
		}
	}
	return segs, nil
}

// ancestorConflict 找出「一个 key 是另一个 key 的父路径」的情况。
func ancestorConflict(sortedKeys []string) (string, string, bool) {
	for i := 0; i < len(sortedKeys); i++ {
		for j := i + 1; j < len(sortedKeys); j++ {
			if strings.HasPrefix(sortedKeys[j], sortedKeys[i]+".") {
				return sortedKeys[i], sortedKeys[j], true
			}
		}
	}
	return "", "", false
}

// BuildURL 由渠道配置与 canonical 路径构造上游 URL。
//
// 它替代了适配器里原先的 endpoint(baseURL, path)，是**唯一**的 URL 拼接点：
// 因为「拿 URL」是发请求的前置，改用它就不可能漏掉 DeclErr 检查。
func BuildURL(cfg *ChannelConfig, canonical string, pathID string) (string, error) {
	if cfg == nil {
		return "", &ConfigError{Field: "channel", Msg: "渠道配置为空"}
	}
	if cfg.DeclErr != nil {
		return "", cfg.DeclErr
	}
	path := canonical
	if v, ok := cfg.Decl.PathOverrides[canonical]; ok {
		path = v
	}
	if pathID != "" && strings.Contains(path, "{id}") {
		path = strings.ReplaceAll(path, "{id}", urlEscapePath(pathID))
	}
	return strings.TrimRight(cfg.BaseURL, "/") + path, nil
}

// BuildRawURL 由渠道配置与「适配器自有的完整路径」构造上游 URL。
//
// 与 BuildURL 的分工：不查 path_overrides、不做 {id} 替换，路径形状完全由适配器决定
// （例如 AutoDL 的 /api/v1/comfyui/comfyui_workflow/{workflow_id}）。
// 但两个守卫必须一模一样（cfg 为空、DeclErr）——「拿 URL」是所有出站请求的前置，
// 只有一处能拿到 URL，才不可能漏掉 fail-closed 检查。
func BuildRawURL(cfg *ChannelConfig, path string) (string, error) {
	if cfg == nil {
		return "", &ConfigError{Field: "channel", Msg: "渠道配置为空"}
	}
	if cfg.DeclErr != nil {
		return "", cfg.DeclErr
	}
	if !strings.HasPrefix(path, "/") {
		return "", &ConfigError{Field: "path", Msg: "适配器内部路径必须以 / 开头：" + path}
	}
	return strings.TrimRight(cfg.BaseURL, "/") + path, nil
}

// urlEscapePath 只转义会破坏路径的字符（不把 / 编成 %2F，那会让上游路径匹配失败）。
func urlEscapePath(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '?' || r == '#' || r == '%':
			b.WriteString(fmt.Sprintf("%%%02X", r))
		case r < 0x20 || r == 0x7f:
			b.WriteString(fmt.Sprintf("%%%02X", r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SetAuthHeader 按配置注入鉴权头。
//
// 语义与 plugin/sandbox.go 的 injectAuth **完全一致**（含"不自动补空格"）：
// 同一套渠道配置在插件与内置适配器上必须产生同一个头，否则运营换个适配器就 401。
// 防错交给前端（它在生成配置时补尾随空格并明示）。
func SetAuthHeader(req *http.Request, cfg *ChannelConfig) {
	if req == nil || cfg == nil || strings.TrimSpace(cfg.APIKey) == "" {
		return
	}
	header := strings.TrimSpace(cfg.Decl.AuthHeader)
	if header == "" {
		header = "Authorization"
	}
	prefix := cfg.Decl.AuthPrefix
	switch {
	case prefix == "":
		prefix = "Bearer "
	case strings.EqualFold(strings.TrimSpace(prefix), "none"):
		prefix = ""
	}
	req.Header.Set(header, prefix+cfg.APIKey)
}

// ApplyRequestPatch 按点分路径改写 JSON 请求体。
//
// 实现要点（两个都是踩过的坑，别改）：
//  1. 逐层用 map[string]json.RawMessage，不用 map[string]any——后者会把 int64 变 float64
//     （9007199254740993 丢精度）、把 1.0 变 1、把 1e10 变 10000000000。叶子值原样保留字节。
//  2. 值 "null" 表示**删除该键**。注意 json.Unmarshal 到 json.RawMessage 时 null 是
//     字面量 []byte("null") 而不是 nil，用 == nil 判断会让删除永远不生效。
//
// 非 JSON 对象体（multipart、数组、标量）原样返回：那是"不适用"，不是失败。
func ApplyRequestPatch(body []byte, patch map[string]json.RawMessage, model string) ([]byte, error) {
	if len(patch) == 0 || len(body) == 0 {
		return body, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body, nil
	}
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := applyPatchAt(root, strings.Split(k, "."), patch[k], model, k); err != nil {
			return nil, err
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, &ConfigError{Field: "request_patch", Msg: "序列化失败：" + err.Error()}
	}
	return out, nil
}

func applyPatchAt(node map[string]json.RawMessage, segs []string, val json.RawMessage, model, field string) error {
	if len(segs) == 0 {
		return &ConfigError{Field: "request_patch." + field, Msg: "路径为空"}
	}
	key := segs[0]
	if len(segs) == 1 {
		if isJSONNull(val) {
			delete(node, key)
			return nil
		}
		node[key] = resolvePatchValue(val, model)
		return nil
	}
	child := map[string]json.RawMessage{}
	if raw, ok := node[key]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &child); err != nil || child == nil {
			return &ConfigError{
				Field: "request_patch." + field,
				Msg:   "「" + key + "」在请求体里不是对象，无法写入其子字段",
			}
		}
	}
	if err := applyPatchAt(child, segs[1:], val, model, field); err != nil {
		return err
	}
	enc, err := json.Marshal(child)
	if err != nil {
		return &ConfigError{Field: "request_patch." + field, Msg: "序列化失败：" + err.Error()}
	}
	node[key] = enc
	return nil
}

func isJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// resolvePatchValue 处理唯一占位符 "$model"（上游字段名不叫 model 时用，如 model_name）。
// 不做通用模板：变量替换一旦放开就成了另一套 SSRF/注入面。
func resolvePatchValue(val json.RawMessage, model string) json.RawMessage {
	if model == "" {
		return val
	}
	if string(bytes.TrimSpace(val)) != `"$model"` {
		return val
	}
	enc, err := json.Marshal(model)
	if err != nil {
		return val
	}
	return enc
}

// ExtractByPath 按点分路径取值（纯数字段按数组下标）。返回值保留原始字节。
func ExtractByPath(root []byte, path string) (json.RawMessage, bool) {
	cur := bytes.TrimSpace(root)
	if len(cur) == 0 {
		return nil, false
	}
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return nil, false
		}
		switch cur[0] {
		case '{':
			var m map[string]json.RawMessage
			if err := json.Unmarshal(cur, &m); err != nil {
				return nil, false
			}
			v, ok := m[seg]
			if !ok {
				return nil, false
			}
			cur = bytes.TrimSpace(v)
		case '[':
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 {
				return nil, false
			}
			var arr []json.RawMessage
			if err := json.Unmarshal(cur, &arr); err != nil || idx >= len(arr) {
				return nil, false
			}
			cur = bytes.TrimSpace(arr[idx])
		default:
			return nil, false
		}
		if len(cur) == 0 {
			return nil, false
		}
	}
	return cur, true
}

// ReconstructChatCompletion 把「非标准响应」重构成标准 OpenAI chat 形状。
//
// 只在非流式 chat 的 2xx 上调用（见 adapters.OpenAI.Chat）。失败一律返回 ConfigError，
// 绝不回落透传：回落会让客户端拿到非 OpenAI 形状的 body（SDK 直接崩），
// 而且这一次调用的 token 会被按估算计费——两个后果都无法从响应里看出来。
func ReconstructChatCompletion(body []byte, rp *ResponsePath, model string) (json.RawMessage, error) {
	if rp == nil {
		return body, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, &ConfigError{Field: "response_path", Msg: "上游响应体为空"}
	}
	if !json.Valid(body) {
		return nil, &ConfigError{Field: "response_path", Msg: "上游响应不是合法 JSON（可能被截断）"}
	}
	raw, ok := ExtractByPath(body, rp.Content)
	if !ok {
		return nil, &ConfigError{Field: "response_path.content", Msg: "路径「" + rp.Content + "」在响应里不存在"}
	}
	content, ok := contentText(raw)
	if !ok {
		return nil, &ConfigError{Field: "response_path.content", Msg: "路径「" + rp.Content + "」不是文本（需要字符串或 [{type:text}] 数组）"}
	}

	out := map[string]any{
		"id":      newCompletionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
	}

	if strings.TrimSpace(rp.PromptTokens) != "" {
		prompt, err := extractTokenCount(body, rp.PromptTokens, "response_path.prompt_tokens")
		if err != nil {
			return nil, err
		}
		completion, err := extractTokenCount(body, rp.CompletionTokens, "response_path.completion_tokens")
		if err != nil {
			return nil, err
		}
		// total_tokens 只为客户端协议兼容；结算只认 prompt/completion（UsageInfo 没有 total 字段）。
		out["usage"] = map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		}
	}
	// 未配置 token 映射时不写 usage：让结算走估算兜底，usage_source=estimated 在调用记录里可见。
	enc, err := json.Marshal(out)
	if err != nil {
		return nil, &ConfigError{Field: "response_path", Msg: "序列化失败：" + err.Error()}
	}
	return enc, nil
}

func extractTokenCount(body []byte, path, field string) (int64, error) {
	raw, ok := ExtractByPath(body, path)
	if !ok {
		return 0, &ConfigError{Field: field, Msg: "路径「" + path + "」在响应里不存在"}
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err != nil {
		return 0, &ConfigError{Field: field, Msg: "路径「" + path + "」不是数字"}
	}
	n, err := num.Int64()
	if err != nil || n < 0 {
		return 0, &ConfigError{Field: field, Msg: "路径「" + path + "」不是非负整数"}
	}
	return n, nil
}

// contentText 提取正文：支持字符串，或 Anthropic 风格的 content blocks 数组
// （按顺序拼接 type=="text" 的 text，其余块忽略）。
func contentText(raw json.RawMessage) (string, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return "", false
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return "", false
		}
		return s, true
	case '[':
		var blocks []json.RawMessage
		if err := json.Unmarshal(t, &blocks); err != nil {
			return "", false
		}
		var b strings.Builder
		for _, blk := range blocks {
			var one struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(blk, &one); err != nil {
				continue
			}
			if one.Type == "text" {
				b.WriteString(one.Text)
			}
		}
		if b.Len() == 0 {
			return "", false
		}
		return b.String(), true
	}
	return "", false
}

func newCompletionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("chatcmpl-cfg-%d", time.Now().UnixNano())
	}
	return "chatcmpl-cfg-" + hex.EncodeToString(b)
}

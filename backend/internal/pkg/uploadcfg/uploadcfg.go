// Package uploadcfg 是 `system_config.uploads` 的**唯一**解析、校验与派生点。
//
// 为什么单独成包（而不是放在 service/upload）：
//   - `upload` 依赖 `db`，而 `db/seed.go` 要写出厂值 —— 反向 import 会成环；
//   - `middleware` 需要按配置推导 `/v1/*` 的请求体门，但中间件不该依赖业务 service。
//
// 本包只依赖 `conf`（零项目内依赖的叶子包），因此 `db / middleware / upload / systemconfig`
// 都能安全引用，配置的字段清单、默认值、校验规则与派生逻辑只有这一份。
package uploadcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"lingdong-api/internal/conf"
)

// 文件类别。类别同时决定「允许的魔数族」与「体积上限」，是配置里唯一的分类维度。
const (
	KindImage    = "image"
	KindAudio    = "audio"
	KindVideo    = "video"
	KindDocument = "document"
	KindText     = "text"
)

// Kinds 是类别的固定顺序（校验、管理台渲染、门户下发共用同一份）。
var Kinds = []string{KindImage, KindAudio, KindVideo, KindDocument, KindText}

// 压缩输出格式。
const (
	// FormatOriginal 保持原后缀（jpeg 按质量重编码、png 走最高压缩级别）。
	FormatOriginal = "original"
	// FormatJPEG 统一转 JPEG（透明区压白底）。
	FormatJPEG = "jpeg"
	// FormatWebP 统一转 WebP（有损 + 保留透明），是体积降幅最大的一档。
	FormatWebP = "webp"
)

// UploadOverhead 是 multipart 表单相对文件本体的额外开销（boundary 与各段头字段）。
//
// 请求体门的唯一来源：`middleware` 与两个上传 controller 都用 RequestCap()，
// 任何地方再写一个数字都会让「配得上、收不下」重新出现。
const UploadOverhead = 1 << 20

// maxLimitMB 是单类型上限的硬顶。再大就不是「文件上传」而是对象直传的场景了。
const maxLimitMB = 10240

// Compress 是图片压缩配置。
type Compress struct {
	Enabled bool `json:"enabled"`
	// Quality 1–100。仅对被压缩的格式生效（JPEG / 有损 WebP）。
	Quality int `json:"quality"`
	// Format 输出格式：original / jpeg / webp。
	Format string `json:"format"`
}

// Settings 是 `uploads` 键的完整形状。
type Settings struct {
	Enabled          bool  `json:"enabled"`
	DailyLimitPerKey int64 `json:"daily_limit_per_key"`
	RetentionHours   int64 `json:"retention_hours"`
	UserTotalMB      int64 `json:"user_total_mb"`
	Concurrency      int64 `json:"concurrency"`
	// Types 是「后缀 → 类别」，即运营可自定义的「支持的文件后缀」。
	// MIME 与魔数族由类别派生（见 MimeFor / familiesForKind），配置注入不了。
	Types map[string]string `json:"types"`
	// Limits 是各类别的单文件上限（MB）。
	Limits   map[string]int64 `json:"limits"`
	Compress Compress         `json:"compress"`
}

// defaultTypes 是出厂支持的后缀。键必须满足 extPattern，值必须是 Kinds 之一。
var defaultTypes = map[string]string{
	"png": KindImage, "jpg": KindImage, "jpeg": KindImage, "webp": KindImage,
	"gif": KindImage, "bmp": KindImage,

	"mp3": KindAudio, "wav": KindAudio, "ogg": KindAudio, "m4a": KindAudio,

	"mp4": KindVideo, "mov": KindVideo, "webm": KindVideo,

	"pdf": KindDocument, "docx": KindDocument, "xlsx": KindDocument,

	"csv": KindText, "txt": KindText, "json": KindText,
}

// defaultLimits 是各类别的出厂单文件上限（MB）。
var defaultLimits = map[string]int64{
	KindImage: 20, KindAudio: 100, KindVideo: 200, KindDocument: 30, KindText: 5,
}

// Defaults 返回出厂配置（唯一默认值：seed、迁移、缺字段兜底都从这里取）。
func Defaults() Settings {
	types := make(map[string]string, len(defaultTypes))
	for k, v := range defaultTypes {
		types[k] = v
	}
	limits := make(map[string]int64, len(defaultLimits))
	for k, v := range defaultLimits {
		limits[k] = v
	}
	return Settings{
		Enabled: true, DailyLimitPerKey: 200, RetentionHours: 24,
		UserTotalMB: 2048, Concurrency: 3,
		Types: types, Limits: limits,
		Compress: Compress{Enabled: true, Quality: 82, Format: FormatWebP},
	}
}

// Current 读取当前生效的上传配置（system_config 热更新即时生效）。
//
// 解析失败或字段缺失时用默认值兜住，**不返回 error**：上传是高频路径，
// 一次配置手误不该让整站上传 4xx/5xx。真正的合法性由保存期的 Validate 把关。
func Current() Settings {
	raw, ok := conf.System().Get("uploads")
	if !ok {
		return Defaults()
	}
	return Normalize(raw)
}

// Normalize 把存库的原始 JSON 补齐成完整形状（缺字段按出厂值补）。
//
// 两种调用方：热路径读当前值（Current），以及管理台回显 —— 存量的 `uploads`
// 可能缺 `types`/`limits`/`compress`，前端拿到残形会直接渲染崩溃
// （压缩那段一行 `form.uploads.compress.enabled` 就够）。同时它保证
// 「GET 回来 → 原样 PUT 回去」永远是完整对象，配置不会越存越薄。
func Normalize(raw json.RawMessage) Settings {
	s := Defaults()
	// Types/Limits 必须先清空再反序列化：json.Unmarshal 对非 nil map 是**合并**语义，
	// 直接解到 Defaults() 上会让「运营删掉的后缀」被出厂值悄悄加回来（配置说了不算）。
	s.Types, s.Limits = nil, nil
	if err := json.Unmarshal(raw, &s); err != nil {
		return Defaults()
	}
	s.normalize()
	return s
}

// normalize 只补「零值」，不覆盖运营显式写入的值。
func (s *Settings) normalize() {
	if s.DailyLimitPerKey <= 0 {
		s.DailyLimitPerKey = Defaults().DailyLimitPerKey
	}
	if s.RetentionHours <= 0 {
		s.RetentionHours = Defaults().RetentionHours
	}
	if s.UserTotalMB <= 0 {
		s.UserTotalMB = Defaults().UserTotalMB
	}
	if s.Concurrency <= 0 {
		s.Concurrency = Defaults().Concurrency
	}
	if len(s.Types) == 0 {
		s.Types = Defaults().Types
	}
	if s.Limits == nil {
		s.Limits = map[string]int64{}
	}
	for _, k := range Kinds {
		if s.Limits[k] <= 0 {
			s.Limits[k] = defaultLimits[k]
		}
	}
	if s.Compress.Quality <= 0 || s.Compress.Quality > 100 {
		s.Compress.Quality = Defaults().Compress.Quality
	}
	switch s.Compress.Format {
	case FormatOriginal, FormatJPEG, FormatWebP:
	default:
		s.Compress.Format = FormatOriginal
	}
}

// extPattern 是后缀的字符集：只允许小写字母与数字，最长 10。
var extPattern = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

// blockedExts 是**无论归到哪一类都拒绝**的后缀。
//
// `/f/{id}/{name}` 用库里的 Content-Type 且 `Content-Disposition: inline`，
// 一个可执行的 HTML/SVG 就是平台域上的存储型 XSS。MIME 已由类别派生（注入不了），
// 这里再挡一层后缀，避免 `text/html` 从「text 类 + html 后缀」的推导里漏出来。
var blockedExts = map[string]bool{
	"svg": true, "html": true, "htm": true, "xhtml": true, "js": true, "mjs": true,
}

// Validate 校验整份配置。所有错误都是**给运营看的中文原文**（保存期直接 400 回显）。
func (s Settings) Validate() error {
	switch {
	case s.DailyLimitPerKey < 1 || s.DailyLimitPerKey > 100000:
		return errors.New("每 KEY 日限须在 1–100000 次")
	case s.RetentionHours < 1 || s.RetentionHours > 72:
		return errors.New("有效期须在 1–72 小时")
	case s.UserTotalMB < 1 || s.UserTotalMB > 1048576:
		return errors.New("每用户在存上限须在 1–1048576 MB")
	case s.Concurrency < 1:
		return errors.New("并发上传数须不小于 1")
	}

	if len(s.Types) == 0 {
		return errors.New("至少保留一种支持的文件后缀")
	}
	if len(s.Types) > 64 {
		return errors.New("支持的文件后缀最多 64 个")
	}
	for ext, kind := range s.Types {
		if !extPattern.MatchString(ext) {
			return fmt.Errorf("后缀 %q 不合法：只能用小写字母与数字，最长 10 位", ext)
		}
		if blockedExts[ext] {
			return fmt.Errorf("后缀 %q 不允许上传（可执行内容会被浏览器在本站直接执行）", ext)
		}
		if !validKind(kind) {
			return fmt.Errorf("后缀 %q 的类别 %q 不合法，只能是 %s", ext, kind, strings.Join(Kinds, " / "))
		}
	}

	for _, k := range Kinds {
		v, ok := s.Limits[k]
		if !ok {
			return fmt.Errorf("缺少 %s 类的单文件上限", kindLabel(k))
		}
		if v < 1 || v > maxLimitMB {
			return fmt.Errorf("%s 类上限须在 1–%d MB", kindLabel(k), maxLimitMB)
		}
	}

	if s.Compress.Quality < 1 || s.Compress.Quality > 100 {
		return errors.New("压缩质量须在 1–100")
	}
	switch s.Compress.Format {
	case FormatOriginal:
	case FormatJPEG:
		if !s.hasExt("jpg") && !s.hasExt("jpeg") {
			return errors.New("输出 JPEG 需要先启用 jpg 或 jpeg 后缀")
		}
	case FormatWebP:
		if !s.hasExt("webp") {
			return errors.New("输出 WebP 需要先启用 webp 后缀")
		}
	default:
		return errors.New("压缩输出格式只能是 original / jpeg / webp")
	}
	return nil
}

func (s Settings) hasExt(ext string) bool {
	for e := range s.Types {
		if e == ext {
			return true
		}
	}
	return false
}

func validKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// kindLabel 是类别在中文文案里的名字。
func kindLabel(kind string) string {
	switch kind {
	case KindImage:
		return "图片"
	case KindAudio:
		return "音频"
	case KindVideo:
		return "视频"
	case KindDocument:
		return "文档"
	case KindText:
		return "文本"
	}
	return kind
}

// NormalizeExt 把调用方给的后缀归一成配置里的键形态（去点、转小写）。
func NormalizeExt(ext string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
}

// TypeOf 返回该后缀的类别；未启用时 ok=false（调用方按「不支持的类型」拒绝）。
func (s Settings) TypeOf(ext string) (kind string, ok bool) {
	kind, ok = s.Types[NormalizeExt(ext)]
	return kind, ok
}

// AllowedExts 返回支持的后缀（升序），供门户渲染 accept 与提示文案。
func (s Settings) AllowedExts() []string {
	out := make([]string, 0, len(s.Types))
	for ext := range s.Types {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// LimitMBOfKind 返回该类别的单文件上限（MB）。
func (s Settings) LimitMBOfKind(kind string) int64 {
	if v := s.Limits[kind]; v > 0 {
		return v
	}
	return defaultLimits[kind]
}

// LimitMB 返回该后缀的单文件上限（MB）；后缀未启用时 0。
func (s Settings) LimitMB(ext string) int64 {
	kind, ok := s.TypeOf(ext)
	if !ok {
		return 0
	}
	return s.LimitMBOfKind(kind)
}

// LimitBytes 返回该后缀的单文件上限（字节）。
func (s Settings) LimitBytes(ext string) int64 {
	return s.LimitMB(ext) << 20
}

// RequestCap 是一次上传请求体的上限：所有类别上限的最大值 + 表单开销。
//
// 这是 `/v1/*` 中间件门、两个上传 controller 的 MaxBytesReader、参考图单文件上限
// **共用的唯一数字**。改造前 middleware 写死 25 MiB，导致「配 50MB 只能收 25MB」。
func (s Settings) RequestCap() int64 {
	var maxMB int64
	for _, v := range s.Limits {
		if v > maxMB {
			maxMB = v
		}
	}
	if maxMB <= 0 {
		maxMB = Defaults().Limits[KindImage]
	}
	return (maxMB << 20) + UploadOverhead
}

// capCacheTTL 是请求体门的缓存时长。
//
// 为什么要缓存：门挂在 `/v1/*` 的**每一个**请求上（含高频 chat 补全），
// 而 Current() 每次都要解析一遍配置 JSON。3 秒的滞后对「改完配置多久生效」
// 完全无感，却把热路径上的解析开销降到零。
const capCacheTTL = 3 * time.Second

var (
	capCacheVal atomic.Int64
	capCacheAt  atomic.Int64 // unix nano
)

// ResetCapCache 让下一次 RequestCapCached 立即重新读取配置。
//
// 保存配置后必须调用：否则「刚把上限调大、请求仍被旧门拦」会有最多 3 秒的窗口，
// 而那个窗口里的失败看起来像配置没生效。
func ResetCapCache() {
	capCacheAt.Store(0)
	capCacheVal.Store(0)
}

// RequestCapCached 返回带短缓存的请求体门（热路径专用）。
//
// 语义与 Settings.RequestCap 完全一致，只是最多晚 3 秒生效。
func RequestCapCached() int64 {
	now := time.Now().UnixNano()
	if v := capCacheVal.Load(); v > 0 && now-capCacheAt.Load() < int64(capCacheTTL) {
		return v
	}
	v := Current().RequestCap()
	capCacheAt.Store(now)
	capCacheVal.Store(v)
	return v
}

// TypeInfo 是门户下发的一行「后缀 / 类别 / 上限」。
type TypeInfo struct {
	Ext   string `json:"ext"`
	Kind  string `json:"kind"`
	MaxMB int64  `json:"max_mb"`
}

// TypeList 返回按后缀升序的类型清单，供门户一次拿全（前端不再维护第二份映射）。
func (s Settings) TypeList() []TypeInfo {
	out := make([]TypeInfo, 0, len(s.Types))
	for _, ext := range s.AllowedExts() {
		out = append(out, TypeInfo{Ext: ext, Kind: s.Types[ext], MaxMB: s.LimitMB(ext)})
	}
	return out
}

// familiesForKind 返回该类别允许的魔数族（与 upload.sniff 的族名逐字对应）。
//
// image/audio 带上 ftyp：heic/avif/m4a 这类容器的头部就是 `ftyp`，
// 少了它这些格式永远过不了魔数校验。真正的安全边界仍是「可执行体一律 danger」。
func familiesForKind(kind string) map[string]bool {
	var list []string
	switch kind {
	case KindImage:
		list = []string{"image", "ftyp"}
	case KindAudio:
		list = []string{"audio", "ftyp"}
	case KindVideo:
		list = []string{"ftyp", "webm"}
	case KindDocument:
		list = []string{"pdf", "zip"}
	case KindText:
		list = []string{"text"}
	}
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}

// FamiliesForKind 供上传域做「族是否与类别相符」的判定。
func FamiliesForKind(kind string) map[string]bool { return familiesForKind(kind) }

// mimeOverrides 是「不能由 `<类别>/<后缀>` 推导出来」的后缀。
//
// 这里刻意是**代码而不是配置**：MIME 决定 `/f/` 的响应头，一旦可由表单注入，
// 一个 `text/html` 就等于在本站开了一个存储型 XSS。
var mimeOverrides = map[string]string{
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",

	"mp3": "audio/mpeg",
	"wav": "audio/wav",
	"ogg": "audio/ogg",
	"m4a": "audio/mp4",

	"mp4":  "video/mp4",
	"mov":  "video/quicktime",
	"webm": "video/webm",

	"pdf":  "application/pdf",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",

	"csv":  "text/csv",
	"txt":  "text/plain",
	"json": "application/json",
}

// MimeFor 返回该后缀落库/下发用的 MIME；后缀未启用时返回空串。
func (s Settings) MimeFor(ext string) string {
	kind, ok := s.TypeOf(ext)
	if !ok {
		return ""
	}
	ext = NormalizeExt(ext)
	if v, ok := mimeOverrides[ext]; ok {
		if kind == KindText && strings.HasPrefix(v, "text/") {
			return v
		}
		return v
	}
	if kind == KindText {
		// 文本类兜底 text/plain：`text/<自定义后缀>` 里藏着 text/html 的可能性必须堵死。
		return "text/plain"
	}
	return kind + "/" + ext
}

// SniffableImage 判断该后缀是否是「我们认得出、也能重编码」的静态图片。
//
// gif 刻意不在列：它有动画，重编码会把动画压成一帧；
// bmp 在列是因为成像库能解它，转 webp/jpeg 收益很大。
func SniffableImage(ext string) bool {
	switch NormalizeExt(ext) {
	case "png", "jpg", "jpeg", "webp", "bmp":
		return true
	}
	return false
}

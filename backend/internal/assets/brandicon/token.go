package brandicon

import (
	"errors"
	"regexp"
	"strings"
)

// 图标取值的三态契约（models.icon / channels.icon / vendors.icon 共用）。
//
//	""                    未设置 —— 由展示层回落（模型→供应商图标→首字）
//	"builtin:<key>"       内置（key 必须命中 catalog）
//	"upload:<ulid>"       运营上传（ulid 必须命中 icons 表，见 service/icon）
//
// 刻意**不支持**的形态与理由：
//   - data URI：门户 JSON 入参上限 1MiB（middleware/bodylimit.go），且模型列表
//     单次最多 5000 行，base64 会把响应撑到几十 MB；
//   - http(s):// 外链：图标会随第三方失效而变成坏图，且等于给前端一个 SSRF/隐私
//     外带通道（浏览器直接请求第三方域）；
//   - SVG：可内嵌 <script>，内联渲染等于把配置变成可执行页面
//     （同类先例见 controller/task.go 对 image/svg+xml 的处理）。
const (
	// BuiltinPrefix 是内置图标的 token 前缀。
	BuiltinPrefix = "builtin:"
	// UploadPrefix 是上传图标的 token 前缀。
	UploadPrefix = "upload:"
	// MaxLen 与三张表的 icon 列宽（size:255）一致。
	MaxLen = 255
)

// ErrInvalid 表示图标取值不合法（对外映射为 400 icon_invalid）。
var ErrInvalid = errors.New("图标不合法")

// uploadIDRe 是 ulid 的**归一后**形态（Crockford Base32，去掉 I/L/O/U）。
//
// 用正则而不是长度判断：upload:<id> 会直接拼进 URL 路径，必须在这里就挡掉
// `..`、`/`、`%2e` 这类能改变路由形态的字符。
//
// 只写小写，是因为 CanonicalUploadID 会先归一大小写再匹配 ——
// ulid 本身是大小写等价的（Crockford Base32），而库里的 id 恒为小写，
// 两边必须落在同一个形态上，否则「上传成功、立刻 404」。
var uploadIDRe = regexp.MustCompile(`^[0-9a-hjkmnp-tv-z]{26}$`)

// builtinKeyRe 是内置 key 的形状约束（与文件名规则一致）。
//
// 即使 catalog 已经保证合法性，这里也要有独立判据：key 会进 URL 路径，
// 而 catalog 是手写的 —— 手写数据加一个路径分隔符进来，不该靠人工 review 兜。
var builtinKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidBuiltinKey 判断一个裸 key 的**形状**是否合法（不查 catalog）。
func ValidBuiltinKey(key string) bool { return builtinKeyRe.MatchString(key) }

// Normalize 校验并归一一个图标取值。
//
// 这是**唯一**的校验点：modelcfg.Build、channel 的写路径、供应商的增改都调它。
// 各处自己写一份 if 判断，迟早出现「管理台能存、AI 提案不能存」这种分叉。
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if len(s) > MaxLen {
		return "", ErrInvalid
	}
	if key, ok := strings.CutPrefix(s, BuiltinPrefix); ok {
		if _, hit := Lookup(key); !hit {
			return "", ErrInvalid
		}
		return BuiltinPrefix + key, nil
	}
	if id, ok := strings.CutPrefix(s, UploadPrefix); ok {
		cid, ok := CanonicalUploadID(id)
		if !ok {
			return "", ErrInvalid
		}
		return UploadPrefix + cid, nil
	}
	return "", ErrInvalid
}

// BuiltinKey 拆出内置图标的 key（非内置返回 false）。
func BuiltinKey(token string) (string, bool) {
	key, ok := strings.CutPrefix(token, BuiltinPrefix)
	if !ok || key == "" {
		return "", false
	}
	return key, true
}

// UploadID 拆出上传图标的 id（非上传返回 false）。
func UploadID(token string) (string, bool) {
	id, ok := strings.CutPrefix(token, UploadPrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// ValidUploadID 判断一个裸 id 是否是合法 ulid（供 /api/icons/u/:id 路由用）。
func ValidUploadID(id string) bool {
	_, ok := CanonicalUploadID(id)
	return ok
}

// CanonicalUploadID 把 ulid 归一到**小写**形态并校验。
//
// 为什么必须有归一这一步：`pkg/ulid.New()` 生成的是**大写** Crockford Base32，
// 而 token 进 URL 路径、进数据库主键，两边形态必须一致。
// 直接拿原始串去查库的后果是：上传成功、立刻访问 404（库里是小写、URL 里是大写），
// 而错误信息只有「图标不存在」—— 从现象上完全看不出是大小写问题。
//
// 归一放在这里（而不是各处 TrimSpace/ToLower）保证「token 的形态」只有一个出口。
func CanonicalUploadID(id string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(id))
	if !uploadIDRe.MatchString(s) {
		return "", false
	}
	return s, true
}

// UploadToken 由图标 id 拼出 token。
//
// 唯一实现：service/icon.Save 的返回值、"上传后立刻选中"的回包、管理台图标列表
// 都从这里拿 token。三处各拼一次 strings，改前缀时必然漏一处，
// 症状是"上传成功但图标不显示"（token 前缀不符而被 Normalize 拒掉）。
//
// 先归一大小写再拼：调用方传进来的可能是库里的值（小写）也可能是刚生成的值，
// 拼出来的 token 必须能通过 Normalize。
func UploadToken(id string) string {
	cid, ok := CanonicalUploadID(id)
	if !ok {
		return ""
	}
	return UploadPrefix + cid
}

// BuiltinToken 由内置 key 拼出 token（与 UploadToken 对称，别处不许手拼前缀）。
//
// key 不命中 catalog 时返回 ""：拼出一个 Normalize 会拒掉的 token 没有意义，
// 调用方拿到空串即「这张图不存在」，正好走首字兜底。
func BuiltinToken(key string) string {
	if _, ok := Lookup(key); !ok {
		return ""
	}
	return BuiltinPrefix + key
}

// Effective 派生「最终展示用」的图标 token。
//
// 全项目只此一处：模型投影（pricing.build）、管理台模型行（toAdminModelItem）
// 都调它。前端拿到的 icon 就是可以直接渲染的值，不再自己拼接回落链 ——
// 回落规则散到前端，等于让"模型没有图标时显示什么"变成两套真相源。
//
// own 优先；own 为空时用 fallback（模型 → 所属供应商）；都为空返回 ""（首字占位）。
func Effective(own, fallback string) string {
	if s := strings.TrimSpace(own); s != "" {
		return s
	}
	return strings.TrimSpace(fallback)
}

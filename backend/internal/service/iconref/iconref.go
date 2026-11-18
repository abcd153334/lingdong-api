// Package iconref 是「图标令牌 → 可渲染地址」的**唯一**实现。
//
// 为什么必须有这个包：图标在库里存的是令牌（`"" | builtin:<key> | upload:<ulid>`），
// 而所有展示位需要的是一个能直接放进 <img src> 的地址。这段映射此前**只存在于
// 浏览器**（web/src/lib/entityIcon.ts），后端从不知道自己图标路由的形状 ——
// 于是任何非 SPA 的消费方（第三方应用、原生客户端、脚本）拿到令牌都无解。
//
// 现在规则只有一份，且在后端：路由形态只在这里出现一次，前端不再拼路径。
// 令牌本身不下发给展示方：令牌是"写回去"用的存储表示，只出现在管理台的编辑面。
package iconref

import (
	"net/url"
	"strings"

	"lingdong-api/internal/assets/brandicon"
)

const (
	builtinPrefix = "/api/icons/b/"
	uploadPrefix  = "/api/icons/u/"
)

// Path 返回令牌对应的站内地址（内置图标带 `?v=`）；空/非法/失效令牌返回 ""。
//
// 两种形态刻意不对称，理由是内容可变性不同：
//
//   - `builtin:<key>`：key 稳定、字节可被后续版本替换 ⇒ 必须带 `?v=<指纹>`；
//   - `upload:<id>`：id 与 sha256 一一对应（icons 表唯一索引 + 上传去重），
//     内容天然不可变 ⇒ 不需要版本，加了反而多一个与 id 冗余的维度。
//
// 内置 key 不在 catalog（或文件缺失）时返回 ""：宁可不下发图标，也不下发一个
// 必然 404 的地址 —— 坏图会让消费方以为"这家没有 logo"，而事实是配置里挂了一个
// 早已不存在的 key。
func Path(token string) string {
	if key, ok := brandicon.BuiltinKey(token); ok {
		// 形状校验不能省：key 会拼进 URL 路径，而 catalog 是手写数据。
		if !brandicon.ValidBuiltinKey(key) {
			return ""
		}
		v := brandicon.AssetVersion(key)
		if v == "" {
			return ""
		}
		return builtinPrefix + key + "?v=" + v
	}
	if id, ok := brandicon.UploadID(token); ok {
		cid, ok := brandicon.CanonicalUploadID(id)
		if !ok {
			return ""
		}
		return uploadPrefix + cid
	}
	return ""
}

// URL 在 Path 之上补对外基址（base 由 siteurl.Base(c) 提供）。
//
// base 为空或不是 http(s) 时**回落 Path** 而不是返回空：相对地址在同源场景仍然可用，
// 而返回空等于把图标整个丢掉。更关键的是不能把配置里的任意 scheme 拼进 <img src> ——
// site.api_base_url 是运营可填字段，不是代码常量。
//
// base 带路径前缀（如 https://x.com/api）不在这里纠正：那是 site 配置的既有契约，
// /v1 示例、/f/ 直链用的都是同一个推导，纠正一处只会制造新的不一致。
func URL(base, token string) string {
	p := Path(token)
	if p == "" {
		return ""
	}
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if !validBase(b) {
		return p
	}
	return b + p
}

// validBase 判断基址是否可以直接拼接（有 host 且 scheme 为 http(s)）。
func validBase(b string) bool {
	if b == "" {
		return false
	}
	u, err := url.Parse(b)
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}

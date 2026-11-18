// Package mediaurl 是「同步产物直链」的唯一签名与改写点。
//
// 为什么需要它：同步图像接口返回的 data[].url 是上游 CDN 直链。直接交给用户有三个问题——
// 暴露上游厂商域名、CDN 防盗链导致预览时好时坏、链接一过期用户再也看不到自己付过钱的图。
// 平台自己签一个**能力 URL**（token 本身就是凭证，不需要 Authorization）之后：
// 图片可以 <img> 直接加载、视频可以拖动（Range 透传）、对外只出现我们自己的域名。
//
// 三条边界（改之前先读）：
//  1. **无状态**：token 里只有「过期时间 + 上游地址 + HMAC」，不落库、不查表。
//     落库会引入一张需要 GC 的表，而且每个请求多一次 DB 往返，收益只是「可撤销」——
//     而同步产物的生命周期本来就由上游 CDN 决定，撤销没有意义。
//  2. **签名之外还要再校验一遍 URL**：密钥泄漏、实现走样、未来某次重构都可能让
//     「签过的地址」变成任意地址。验签通过不等于安全，仍然要过 netguard 的策略。
//  3. **改写只发生在响应出口**（relay 写出前），采集与计费读到的仍是上游原文——
//     排查故障时需要的是上游真实地址，不是我们包装后的地址。
package mediaurl

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/netguard"
)

// 平台自有媒体能力的路径前缀（对外契约见 internal/apidoc：media/proxy）。
//
// 这里必须与 apidoc 的 Path 一致。之所以不 import apidoc：apidoc 是文档层，
// 反向依赖会让「改文档」有机会改坏签名链接。一致性由 mediaurl_test 的断言守住。
const PathPrefix = "/v1/media/"

const (
	// tokenVersion 是 token 格式版本。改格式必须升版本，否则旧链接会被解成垃圾数据，
	// 校验却"差不多能过"（例如把签名前 16 字节当签名）。
	tokenVersion byte = 1
	// tokenVersionArtifact 是**任务产物**能力 URL 的版本（v2）。
	//
	// 与 v1 必须分开：v1 的 payload 是「过期 + 上游 URL」，产物 token 里没有 URL、
	// 而是「任务号 + 序号」，同一个解码器读两种布局只会读出垃圾且验签还能过
	// （长度够、签名对，但语义全错）。
	tokenVersionArtifact byte = 2
	// kindArtifact 是 v2 payload 里的载荷类型（留一个字节，将来可能有「文件能力」等）。
	kindArtifact byte = 2
	// sigLen 是截断后的签名长度（128 位）。够用且让 token 短一截：
	// 每张图都会带这个链接，长 20 个字符乘以几万次请求是实打实的带宽。
	sigLen = 16
	// maxURLBytes 是允许签名的上游地址上限。超过就不改写（原样透传上游链接）：
	// 超长链接多半是内联 base64 伪装的 URL，签进来只会撑爆请求行。
	maxURLBytes = 2048
	// hkdfInfo 是密钥派生上下文：与 Fernet 主密钥用同一份环境密钥，但派生出独立的子密钥，
	// 避免「媒体签名密钥 == 加密渠道密钥」这种跨用途复用。
	hkdfInfo = "lingdong/media-proxy/v1"
	// hmacContext 是签名域的隔离前缀。没有它，同密钥下的其它 HMAC 用途可以互换消息。
	hmacContext = "media:v1:"
	// hmacContextArtifact 是产物 token 的签名域（与 URL 能力 token 隔离）。
	hmacContextArtifact = "media:v2:artifact:"
)

var (
	// ErrInvalid 表示 token 形态/签名不对（含密钥轮换后无法验证的旧链接）。
	ErrInvalid = errors.New("mediaurl: 无效的媒体链接")
	// ErrExpired 表示签名正确但已过期。与 ErrInvalid 分开是为了让调用方能回 410 而不是 404：
	// 410 让客户端知道「别再重试这条路」，而 404 会让它怀疑自己拼错了地址。
	ErrExpired = errors.New("mediaurl: 媒体链接已过期")
	// ErrNoKey 表示进程没有配置 APP_ENCRYPTION_KEY，无法签名。
	// 这不是「用默认密钥兜底」的场景：默认密钥等于公开密钥，任何人都能伪造任意地址。
	ErrNoKey = errors.New("mediaurl: 未配置 APP_ENCRYPTION_KEY")
)

// DefaultTTL 是签名链接的默认有效期。
//
// 24 小时是刻意的：同步产物的上游 CDN 直链通常也是 1 天左右失效，
// 我们把 TTL 设得更长只会得到「我们的链接有效、上游已经删了」的 404。
const DefaultTTL = 24 * time.Hour

// maxTTL 是有效期上限（7 天）。环境变量写错（比如写成秒）时兜住，
// 避免出现一个十年有效的公开直链。
const maxTTL = 7 * 24 * time.Hour

// TTL 返回当前生效的有效期（LD_MEDIA_TTL_HOURS，默认 24 小时）。
func TTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LD_MEDIA_TTL_HOURS"))
	if raw == "" {
		return DefaultTTL
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return DefaultTTL
	}
	d := time.Duration(n) * time.Hour
	if d > maxTTL {
		return maxTTL
	}
	return d
}

// Link 把上游直链包成本站能力 URL（base + /v1/media/{token}）。
//
// ok=false 表示「这条链接不该被改写」，调用方应原样保留上游地址：
//   - 不是 http/https（data:、blob:、相对路径）；
//   - 已经是我们自己的能力 URL（渠道回源到平台时会出现，再签一层会无限套娃）；
//   - 超长或含凭据（netguard 会拒绝，签了也取不到）；
//   - 没有配置签名密钥。
func Link(base, rawURL string, ttl time.Duration) (string, bool) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return "", false
	}
	u := strings.TrimSpace(rawURL)
	if u == "" || strings.HasPrefix(strings.ToLower(u), "data:") {
		return "", false
	}
	// 已经是我们自己的能力 URL 时不再签一层：渠道回源到平台（或未来某条链路把
	// 我们的链接当素材再传一次）会让 token 无限套娃，长度指数增长。
	if strings.HasPrefix(u, base) && strings.Contains(u, PathPrefix) {
		return "", false
	}
	if err := netguard.ValidateURL(u, netguard.PolicyFromEnv()); err != nil {
		return "", false
	}
	tok, err := Sign(u, ttl)
	if err != nil {
		return "", false
	}
	return base + PathPrefix + tok, true
}

// Sign 生成能力 token（base64url(payload) + "." + base64url(sig)）。
func Sign(rawURL string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return SignUntil(rawURL, time.Now().Add(ttl))
}

// SignUntil 是 Sign 的显式到期时间版本。
//
// 单独导出是为了让「到期」这件事可以被直接表达：ttl<=0 在 Sign 里是「没传，用默认」，
// 没法用它签出一个已过期的 token —— 而「过期链接该怎么响应」正是这里最需要被测的分支。
func SignUntil(rawURL string, exp time.Time) (string, error) {
	u := strings.TrimSpace(rawURL)
	if u == "" || len(u) > maxURLBytes {
		return "", ErrInvalid
	}
	key, err := deriveKey()
	if err != nil {
		return "", err
	}
	payload := make([]byte, 9, 9+len(u))
	payload[0] = tokenVersion
	binary.BigEndian.PutUint64(payload[1:], uint64(exp.Unix()))
	payload = append(payload, u...)

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(hmacContext))
	mac.Write(payload)
	sig := mac.Sum(nil)[:sigLen]

	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify 校验 token 并取回上游地址。
func Verify(token string) (string, error) {
	return VerifyAt(token, time.Now())
}

// VerifyAt 是 Verify 的可注入时钟版本（测试用；生产只走 Verify）。
func VerifyAt(token string, now time.Time) (string, error) {
	t := strings.TrimSpace(token)
	dot := strings.LastIndexByte(t, '.')
	if dot <= 0 || dot == len(t)-1 {
		return "", ErrInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(t[:dot])
	if err != nil || len(payload) < 10 {
		return "", ErrInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(t[dot+1:])
	if err != nil || len(sig) != sigLen {
		return "", ErrInvalid
	}
	if payload[0] != tokenVersion {
		return "", ErrInvalid
	}
	key, err := deriveKey()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(hmacContext))
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)[:sigLen]) {
		return "", ErrInvalid
	}
	// 先验签再判过期：反过来会让「换个过期时间重新拼」的伪造 token 拿到 410，
	// 白白告诉攻击者「你的签名结构是对的」。
	exp := int64(binary.BigEndian.Uint64(payload[1:9]))
	if now.Unix() >= exp {
		return "", ErrExpired
	}
	raw := string(payload[9:])
	if err := netguard.ValidateURL(raw, netguard.PolicyFromEnv()); err != nil {
		return "", ErrInvalid
	}
	return raw, nil
}

// RewriteDataURLs 在**已解析**的顶层对象上改写 data[].url，返回改写条数。
//
// 单独导出是为了让响应归一（relay.shapeResponse）只解析一次报文：
// 归一还要同时改 id/model，整份重解析一遍就等于把同一个 8MB 响应
// 读两遍 —— 而图像响应恰恰是最常带大 base64 的那类。
func RewriteDataURLs(top map[string]json.RawMessage, base string, ttl time.Duration) int {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || top == nil {
		return 0
	}
	rawData, ok := top["data"]
	if !ok {
		return 0
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(rawData, &items); err != nil || len(items) == 0 {
		return 0
	}
	changed := 0
	for _, it := range items {
		raw, ok := it["url"]
		if !ok {
			continue
		}
		var u string
		if json.Unmarshal(raw, &u) != nil {
			continue
		}
		link, ok := Link(base, u, ttl)
		if !ok {
			continue
		}
		enc, err := marshalNoEsc(link)
		if err != nil {
			continue
		}
		it["url"] = enc
		changed++
	}
	if changed == 0 {
		return 0
	}
	if data, err := marshalNoEsc(items); err == nil {
		top["data"] = data
	}
	return changed
}

// deriveKey 由 APP_ENCRYPTION_KEY 派生出媒体签名子密钥。
//
// 刻意不做缓存：HKDF 一次不到 1 微秒，而缓存会让「运行期换密钥」这件事
// 变成需要重启才生效的隐式约定。同步产物改写每张图只算一次。
func deriveKey() ([]byte, error) {
	secret := strings.TrimSpace(conf.EnvConfig().AppEncryptionKey)
	if secret == "" {
		return nil, ErrNoKey
	}
	k := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(secret), nil, []byte(hkdfInfo)), k); err != nil {
		return nil, ErrInvalid
	}
	return k, nil
}

// marshalNoEsc 序列化并关闭 HTML 转义。
//
// 默认的 json.Marshal 会把 `<`、`>`、`&` 转成 \u003c 这类转义：
// 图像响应里带 `<` 的概率不高，但 URL 的查询串里出现 & 是常态，
// 转义后虽然合法，却会让「响应体和上游不一致」这种困惑在排查时反复出现。
func marshalNoEsc(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// Claim 是一个产物能力 token 的载荷。
type Claim struct {
	TaskID uint64
	Index  int
	Exp    time.Time
}

// SignArtifact 为「某任务的第 index 个产物」签一个能力 token。
//
// 为什么不用「签名一个自家 URL」再让代理回源：那会绕回需要鉴权的端点，
// 浏览器里的 <img> 依然拿不到字节。token 直接携带 (taskID,index)，
// 代理拿它去库里取字节，鉴权由签名本身承担。
func SignArtifact(taskID uint64, index int, ttl time.Duration) (string, error) {
	if taskID == 0 || index < 0 {
		return "", ErrInvalid
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	key, err := deriveKey()
	if err != nil {
		return "", err
	}
	payload := make([]byte, 22, 22)
	payload[0] = tokenVersionArtifact
	payload[1] = kindArtifact
	binary.BigEndian.PutUint64(payload[2:10], uint64(time.Now().Add(ttl).Unix()))
	binary.BigEndian.PutUint64(payload[10:18], taskID)
	binary.BigEndian.PutUint32(payload[18:22], uint32(index))

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(hmacContextArtifact))
	mac.Write(payload)
	sig := mac.Sum(nil)[:sigLen]
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyArtifact 校验产物 token 并取回声明。
func VerifyArtifact(token string) (Claim, error) {
	return verifyArtifactAt(token, time.Now())
}

func verifyArtifactAt(token string, now time.Time) (Claim, error) {
	t := strings.TrimSpace(token)
	dot := strings.LastIndexByte(t, '.')
	if dot <= 0 || dot == len(t)-1 {
		return Claim{}, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(t[:dot])
	if err != nil || len(payload) != 22 {
		return Claim{}, ErrInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(t[dot+1:])
	if err != nil || len(sig) != sigLen {
		return Claim{}, ErrInvalid
	}
	if payload[0] != tokenVersionArtifact || payload[1] != kindArtifact {
		return Claim{}, ErrInvalid
	}
	key, err := deriveKey()
	if err != nil {
		return Claim{}, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(hmacContextArtifact))
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)[:sigLen]) {
		return Claim{}, ErrInvalid
	}
	exp := int64(binary.BigEndian.Uint64(payload[2:10]))
	if now.Unix() >= exp {
		return Claim{}, ErrExpired
	}
	return Claim{
		TaskID: binary.BigEndian.Uint64(payload[10:18]),
		Index:  int(binary.BigEndian.Uint32(payload[18:22])),
		Exp:    time.Unix(exp, 0),
	}, nil
}

// ArtifactLink 把产物包成本站能力 URL；签不出来（无密钥/无基址）返回 false。
func ArtifactLink(base string, taskID uint64, index int, ttl time.Duration) (string, bool) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return "", false
	}
	tok, err := SignArtifact(taskID, index, ttl)
	if err != nil {
		return "", false
	}
	return base + PathPrefix + tok, true
}

// VerifyAny 是代理端点的唯一入口：按版本号分派「上游地址」与「任务产物」。
func VerifyAny(token string) (string, *Claim, error) {
	t := strings.TrimSpace(token)
	dot := strings.LastIndexByte(t, '.')
	if dot <= 0 {
		return "", nil, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(t[:dot])
	if err != nil || len(payload) == 0 {
		return "", nil, ErrInvalid
	}
	switch payload[0] {
	case tokenVersion:
		u, verr := Verify(token)
		return u, nil, verr
	case tokenVersionArtifact:
		c, cerr := VerifyArtifact(token)
		return "", &c, cerr
	default:
		return "", nil, ErrInvalid
	}
}

// Package channelkeys 是渠道上游密钥池的**唯一**加解密与标识入口。
//
// 为什么单独成包：这段逻辑原先在 service/channel 与 relay 各写了一遍
// （relay 侧是为了避开 import 环），两处的"解密失败"处理还不一致 —— 两边都是
// 静默 continue，导致返回数组的下标与原始 token 下标错位：后续的冷却/归因
// 按错位后的下标去标记，会把「A 坏了」记到「B 头上」。
//
// 本包的三条硬约定：
//  1. Decrypt 的 Entries **与 token 数组一一对应、保序**，失败项保留（Err 非空）；
//  2. 每个 key 有稳定标识 ID（明文 HMAC 指纹），换序/删邻/重新加密都不变；
//  3. 任何 key 级状态（冷却、禁用、用量归属）只能以 ID 为键，数组下标只用于展示。
package channelkeys

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/crypto"
)

// ErrEmptyPool 表示试图把空池加密落库。
//
// 空池不是"清空"的同义词：调用方必须先决定是「不改」（不提交 keys）
// 还是「清空」（另行显式处理），不能靠一个空数组默默抹掉全部凭据。
var ErrEmptyPool = errors.New("channelkeys: 密钥池不能为空")

// 密钥池规格：单渠道 100 把、单条 1024 字符。
//
// 硬上限的理由不是洁癖：创建与增量写都是「一次请求把整池逐条 Fernet 加密」，
// 没有上限时一次误粘贴几十万行会同步阻塞保存请求，并把管理台面板渲染打穿。
const (
	MaxKeysPerChannel = 100
	MaxKeyLen         = 1024
)

// NormalizeKey 归一单条明文密钥：trim + 拒绝空 / 超长 / 内嵌空白。
//
// 「什么算一条合法密钥」只在这里判：创建（channel.Req.normalize）与增量写
// （channel.applyOps 的 add / replace / replace_all）共用同一份规则。
// 分成两处时典型漏项是「创建收 1024 字符、增量收 1MB」，上限等于没有。
//
// 为什么要拒绝内嵌空白：密钥是 opaque token，内部不会出现空格或制表符；
// 出现了就是粘贴错位（从表格、从聊天记录里带出来的），而放进去之后
// 只会得到一个「看起来配好了」的 401，排查方向会跑到「账号被封」。
func NormalizeKey(s string) (string, error) {
	k := strings.TrimSpace(s)
	if k == "" {
		return "", errors.New("密钥不能为空")
	}
	if n := len([]rune(k)); n > MaxKeyLen {
		return "", fmt.Errorf("密钥过长（%d 字符，上限 %d）", n, MaxKeyLen)
	}
	for _, r := range k {
		if r == 0 || unicode.IsSpace(r) {
			return "", errors.New("密钥里不能有空格、制表符或换行")
		}
	}
	return k, nil
}

// idDomain 是 keyID 派生时的域分离标签。
//
// 同一个 APP_ENCRYPTION_KEY 既用于 Fernet 加密（拆成 sign/enc 两半），
// 也用于这里派生指纹子键。不分离的话等于让同一份密钥材料承担两种用途，
// 将来任何一侧的算法调整都会牵动另一侧。
const idDomain = "|ld:channel-key-id:v1"

// idBytes 是 keyID 的字节长度（16 字节 = 32 hex）。64 bit 不够：管理台能决定
// 密钥内容，构造碰撞在理论上可行；128 bit 留足余量，且仍能塞进 32 字符的列。
const idBytes = 16

// shortKeyRunes 是允许展示指纹的最小长度。
//
// 指纹规则是「前 6 + 后 4」，合计暴露 10 个字符。密钥短于 16 个字符时，
// 暴露 10 个字符已经等于把整把 key 交出去，此时一律显示 ******。
const shortKeyRunes = 16

// Entry 是一枚密钥的解密结果。Index 是它在原始 token 数组里的下标，
// **无论其它条目是否解密失败，都不重排**。
type Entry struct {
	Index int
	Token string
	Plain string // 解密失败时为空
	ID    string // 解密失败时为空
	Err   error
}

// OK 表示这枚密钥可用（解密成功）。
func (e Entry) OK() bool { return e.Err == nil && e.Plain != "" }

// Result 是一次解密的全量结果。
type Result struct {
	Entries []Entry // 长度 == token 数；Invalid 时为空
	Failed  int     // 解密失败的条数
	Total   int     // token 总数
	// Invalid 表示 channels.keys 本身不是合法的 []string（列被写坏/迁移残留）。
	// 它与「单条 token 解密失败」是两件事：前者要修数据，后者要换密钥。
	Invalid bool
}

// FailedCount 返回不可用条数（含列损坏这一种）。
func (r Result) FailedCount() int {
	if r.Invalid {
		return 1
	}
	return r.Failed
}

// Decrypt 解密密钥池；**失败项保留、下标永不重排**。
func Decrypt(raw model.JSON) Result {
	if len(raw) == 0 {
		return Result{}
	}
	var tokens []string
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return Result{Invalid: true}
	}
	out := Result{Entries: make([]Entry, 0, len(tokens)), Total: len(tokens)}
	master := crypto.FernetDerive(conf.EnvConfig().AppEncryptionKey)
	sub := subkey(master)
	for i, tok := range tokens {
		e := Entry{Index: i, Token: tok}
		plain, err := crypto.FernetDecrypt(master, tok)
		if err != nil {
			e.Err = err
			out.Failed++
		} else {
			e.Plain = string(plain)
			e.ID = idWith(sub, plain)
		}
		out.Entries = append(out.Entries, e)
	}
	return out
}

// Encrypt 把明文池加密为 Fernet token 数组（落 channels.keys）。
// 空池返回错误：调用方（创建/替换）必须显式决定"不改"还是"清空"。
func Encrypt(plain []string) (model.JSON, error) {
	if len(plain) == 0 {
		return nil, ErrEmptyPool
	}
	master := crypto.FernetDerive(conf.EnvConfig().AppEncryptionKey)
	tokens := make([]string, 0, len(plain))
	for _, k := range plain {
		t, err := crypto.FernetEncrypt(master, []byte(k))
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return model.NewJSON(tokens)
}

// ID 返回明文密钥的稳定标识（32 位小写 hex）。
//
// 为什么用 HMAC 而不是裸 SHA256：自建/本地部署的 key 常是低熵字符串
// （`sk-test`、`test123`），裸哈希一旦随 usages.channel_key_id 落库，
// 拖库方可以拿字典反查出明文。HMAC 需要服务端密钥，杜绝这条路。
func ID(plain []byte) string {
	return idWith(subkey(crypto.FernetDerive(conf.EnvConfig().AppEncryptionKey)), plain)
}

func idWith(sub, plain []byte) string {
	mac := hmac.New(sha256.New, sub)
	mac.Write(plain)
	sum := mac.Sum(nil)
	return hex.EncodeToString(sum[:idBytes])
}

// subkey 从 Fernet 主密钥派生指纹专用子键（与 Fernet 的 sign/enc 两半做域分离）。
func subkey(master []byte) []byte {
	h := sha256.New()
	h.Write(master)
	h.Write([]byte(idDomain))
	return h.Sum(nil)
}

// Fingerprint 返回给人看的指纹（前 6 + … + 后 4）；过短的 key 一律全遮。
func Fingerprint(plain string) string {
	r := []rune(strings.TrimSpace(plain))
	if len(r) < shortKeyRunes {
		return "******"
	}
	return string(r[:6]) + "…" + string(r[len(r)-4:])
}

// Dedupe 按 keyID 去重并保序（保留首次出现），返回去重后的池与被移除的条数。
//
// 必须去重：同一把 key 出现两次时，两行的 ID 相同 —— 面板上会出现两行共享
// 同一个启用开关（关一个另一个也跟着关），而冷却键也只有一个，行为无法解释。
func Dedupe(keys []string) ([]string, int) {
	if len(keys) < 2 {
		return keys, 0
	}
	sub := subkey(crypto.FernetDerive(conf.EnvConfig().AppEncryptionKey))
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	removed := 0
	for _, k := range keys {
		id := idWith(sub, []byte(k))
		if _, dup := seen[id]; dup {
			removed++
			continue
		}
		seen[id] = struct{}{}
		out = append(out, k)
	}
	return out, removed
}

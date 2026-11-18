package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"lingdong-api/internal/conf"
)

// builder 按通道配置构造 Store。
//
// 构造必须**不联网**（只做字段校验与结构初始化），否则缓存预构建、探针草稿、
// UsableBackends 都会变成网络操作。
type builder func(name string, ch map[string]interface{}) (Store, error)

// builders 是后端注册表：能构建哪些后端只由这里决定。
//
// 增删驱动只改这一处；配置页的 storageSchemes、文档里的可选值都必须与它对齐
// （TestStorageSchemesAllImplemented 守这条）。
var builders = map[string]builder{
	"none":  newLocalStore,
	"s3":    newS3Store,
	"minio": newS3Store,
	"oss":   newOSSStore,
	"cos":   newCOSStore,
}

// channelString 读通道配置里的字符串字段（唯一定义点：三个驱动共用）。
func channelString(ch map[string]interface{}, key string) string {
	if v, ok := ch[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// channelBool 读布尔字段，同时接受真正的 bool 与文本（"true"/"1"/"yes"）。
//
// 为什么容忍文本：配置页的通道参数是统一的文本输入框，运营会照提示填 "true"。
// 只认 bool 的话，这个字段会静默失效 —— 表现为「填了 path_style 还是按虚拟主机寻址」。
func channelBool(ch map[string]interface{}, key string, def bool) bool {
	v, ok := ch[key]
	if !ok {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off", "":
			return false
		}
	}
	return def
}

// ValidateChannel 用给定通道配置试构建一次（不联网、不入缓存）。
//
// 供配置保存期使用：它必须校验**本次提交**的通道配置，而 For() 读的是已生效配置。
// 与运行期共用同一份 builders，不另写一套「字段是否填全」的判据。
func ValidateChannel(backend string, ch map[string]interface{}) error {
	b, ok := builders[backend]
	if !ok {
		return fmt.Errorf("%w: 未知存储后端 %s", ErrBackendGone, quoteName(backend))
	}
	if _, err := b(backend, ch); err != nil {
		return backendGonef(err, "后端 %s 无法构建", quoteName(backend))
	}
	return nil
}

// newLocalStore 是 localStore 的构建入口（忽略通道配置）。
func newLocalStore(_ string, _ map[string]interface{}) (Store, error) {
	return &localStore{root: conf.EnvConfig().StorageDir}, nil
}

// storeCacheMax 是缓存条数上限。
//
// 条目数 = 后端数 × 配置变更次数。每次改配置都会留下一条旧条目，
// 所以超过上限就整体重建 —— 代价只是重新构造结构体（http.Transport 是进程级共享，
// 不会因此丢连接池），收益是不需要追踪淘汰顺序。
const storeCacheMax = 64

type cacheKey struct {
	backend     string
	fingerprint string
}

var (
	storeMu    sync.Mutex
	storeCache = map[cacheKey]Store{}
)

// channelFingerprint 是通道配置的稳定指纹（json.Marshal 对 map 键排序，同内容必同指纹）。
func channelFingerprint(ch map[string]interface{}) string {
	if len(ch) == 0 {
		return ""
	}
	raw, err := json.Marshal(ch)
	if err != nil {
		// 值来自 json.Unmarshal，反转必然成功；真失败就退化成"每次重建"，不影响正确性。
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// cachedStore 按 (后端, 配置指纹) 复用 Store 实例。
//
// 为什么需要缓存：改造前每次 Active() 都新建 client/Transport，等于**每个请求
// 一个新连接池 + 一次全量 TLS 握手**。S3 下勉强能跑，OSS/COS 的高 QPS 会被握手耗死。
// 指纹随配置变化，热更新天然失效，不需要订阅回调。
func cachedStore(backend string, ch map[string]interface{}, build func() (Store, error)) (Store, error) {
	k := cacheKey{backend: backend, fingerprint: channelFingerprint(ch)}

	storeMu.Lock()
	if st, ok := storeCache[k]; ok {
		storeMu.Unlock()
		return st, nil
	}
	storeMu.Unlock()

	// 构造放在锁外：它可能派生加密密钥，串行化没有收益。
	st, err := build()
	if err != nil {
		return nil, err
	}

	storeMu.Lock()
	defer storeMu.Unlock()
	if exist, ok := storeCache[k]; ok {
		return exist, nil
	}
	if len(storeCache) >= storeCacheMax {
		storeCache = map[cacheKey]Store{}
	}
	storeCache[k] = st
	return st, nil
}

// ActiveChannel 返回当前配置里某个后端的通道参数（未配置返回 nil）。
//
// 唯一用途是「探针测当前生效配置」：探针需要真实的通道参数才能构建，
// 而它又不该自己去解析 system_config（那是 settingsOf 的唯一职责）。
func ActiveChannel(backend string) map[string]interface{} {
	s, err := settingsOf()
	if err != nil {
		return nil
	}
	return s.Channels[strings.TrimSpace(backend)]
}

// ProbeChannel 用**给定**的通道配置做一次真实的读写探针（写 → 匿名读回 → 删除）。
//
// 与 ValidateChannel 的分工：
//   - ValidateChannel 只回答「这套配置能不能构建」，不联网。保存期用它，快且无副作用；
//   - ProbeChannel 回答「凭证对不对、桶在不在、匿名能不能读、能不能删」，必须真发请求。
//
// 为什么要**匿名**读回：对外地址是不带签名的永久直链（桶公开读）。私有桶时
// 我们带签名能读、浏览器打开是 403，而配置页只会显示「保存成功」——
// 这种「平台内一切正常、用户全打不开」的偏差必须在保存期拦住。
//
// 为什么不干脆「先保存再测」：保存即热生效，一次错误的凭证会让**正在运行**的所有
// 产物转存立刻失败（逐单退款、上游成本白付）。
//
// probeKeyPrefix 下的对象不会被任何清理任务扫到（files/uploads 表里没有对应记录），
// 所以这里必须保证「写完一定删掉」；Delete 失败也算探针失败，因为
// 「能写不能删」会让清理任务永远删不干净，是一种真实的密钥策略错误。
func ProbeChannel(ctx context.Context, backend string, ch map[string]interface{}) (string, error) {
	name := strings.TrimSpace(backend)
	if name == "" {
		name = "none"
	}
	b, ok := builders[name]
	if !ok {
		return name, fmt.Errorf("%w: 未知存储后端 %s", ErrBackendGone, quoteName(name))
	}
	st, err := b(name, ch)
	if err != nil {
		return name, backendGonef(err, "后端 %s 无法构建", quoteName(name))
	}
	key := fmt.Sprintf("_healthcheck/%d.txt", time.Now().UnixNano())
	const payload = "lingdong storage probe"
	if err := st.Put(ctx, key, strings.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		return name, fmt.Errorf("%w: 写入失败: %v", ErrUnavailable, err)
	}
	// 无论后续哪步失败都要删掉探针对象；删除本身的错误单独判（见下）。
	if pu, ok := st.(PublicURLer); ok {
		if url, ok := pu.PublicURL(key); ok {
			if err := probeAnonymousGet(ctx, url, payload); err != nil {
				_ = st.Delete(ctx, key)
				return name, err
			}
		}
	}
	if err := st.Delete(ctx, key); err != nil {
		return name, fmt.Errorf("%w: 删除失败（对象可写但不可删，请检查密钥策略）: %v", ErrUnavailable, err)
	}
	return name, nil
}

// probeAnonymousGet 用**不带任何签名**的 GET 读回探针对象并比对内容。
//
// 这是「桶公开读」这一前提的唯一验证点：地址能拼出来不代表能匿名读。
func probeAnonymousGet(ctx context.Context, url, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: 探针地址非法: %v", ErrUnavailable, err)
	}
	resp, err := guardedClient().Do(req)
	if err != nil {
		return fmt.Errorf("%w: 匿名读取失败（桶需公开读）: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: 匿名读取被拒绝（HTTP %d，桶需公开读）", ErrUnavailable, resp.StatusCode)
	}
	// 探针对象只有 21 字节，限读 1KB 足够，也顺手挡住「读到别人的大对象」。
	got, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return fmt.Errorf("%w: 匿名读取中断: %v", ErrUnavailable, err)
	}
	if string(got) != want {
		return fmt.Errorf("%w: 匿名读回内容不一致（桶可能被 CDN/网关改写）", ErrUnavailable)
	}
	return nil
}

// Active 按当前 system_config 构建激活的存储后端（写路径唯一入口）。
//
// 与 For/ForRef 的分工：Active 回答「现在该往哪写」，配置有问题就是 ErrUnavailable；
// For/ForRef 回答「这条记录当初写到哪」，记录指向的后端没了是 ErrBackendGone。
// 两个问题、两个答案，不能合并。
//
// **缺配置不回落本地**：返回 ErrNotConfigured（同时包 ErrUnavailable，对外 503）。
// 唯一写本地的方式是配置里显式写 `active: "none"`。
func Active() (Store, error) {
	s, err := settingsOf()
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return nil, fmt.Errorf("%w: %w: system_config 缺少 storage 配置", ErrUnavailable, ErrNotConfigured)
		}
		return nil, fmt.Errorf("%w: storage 配置解析失败: %v", ErrUnavailable, err)
	}
	name := strings.TrimSpace(s.Active)
	st, err := buildNamed(name, s.Channels[name])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return st, nil
}

// For 按**记录里的 backend**构建 Store（用该通道当前配置的桶）。
//
// 只用于「这个后端还能不能构建」这类配置语义（UsableBackends/探针）。
// 读/删对象必须用 ForRef —— 它们要的是记录里的桶，不是当前配置的桶。
func For(backend string) (Store, error) { return buildStore(backend, "") }

// ForRef 按记录里的 backend + bucket 构建 Store，与当前激活后端完全无关。
//
// 这是「切换桶之后仍能读/删历史对象」的唯一保证。
// 记录在云后端上却没有桶（写路径漏记）时直接报 ErrBucketMissing：
// **绝不回退当前配置的桶** —— 那会把「删错地方」伪装成「删除成功」。
func ForRef(ref Ref) (Store, error) {
	name := strings.TrimSpace(ref.Backend)
	if name == "" {
		return nil, fmt.Errorf("%w", ErrBackendMissing)
	}
	if name == "none" {
		return buildStore(name, "")
	}
	bucket := strings.TrimSpace(ref.Bucket)
	if bucket == "" {
		return nil, fmt.Errorf("%w: 后端 %s", ErrBucketMissing, quoteName(name))
	}
	return buildStore(name, bucket)
}

// buildStore 是 For/ForRef 共用的构建入口（唯一处）。
//
// bucket 非空时覆盖通道配置里的 bucket：client 的寻址目标因此变成「记录里的桶」，
// 缓存指纹也随之变化（指纹是整份通道 JSON 的哈希），所以 A 桶与 B 桶是两个实例。
func buildStore(backend, bucket string) (Store, error) {
	name := strings.TrimSpace(backend)
	if name == "" {
		// 全新开发没有存量行，空值只可能来自写路径漏记 —— 立刻失败并告警，
		// 不做「先试本地再试当前后端」的懒回填：那是把 bug 静默成"正确"。
		return nil, fmt.Errorf("%w", ErrBackendMissing)
	}
	if name == "none" {
		// 本地后端不依赖 storage 配置：配置坏掉时仍要能读本地对象。
		return buildNamed(name, nil)
	}
	s, err := settingsOf()
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			// 记录说自己在云上，但整份配置都没了：对读路径就是「后端已移除」。
			// 绝不折成「回落到本地试试」—— 那会把配置事故伪装成「文件被删」。
			return nil, backendGonef(ErrNotConfigured, "后端 %s 的配置已不存在", quoteName(name))
		}
		return nil, fmt.Errorf("%w: storage 配置解析失败: %v", ErrUnavailable, err)
	}
	ch := s.Channels[name]
	if bucket != "" {
		cp := make(map[string]interface{}, len(ch)+1)
		for k, v := range ch {
			cp[k] = v
		}
		cp["bucket"] = bucket
		ch = cp
	}
	return buildNamed(name, ch)
}

// buildNamed 是 Active 与 buildStore 共用的构建实现（唯一处）。
func buildNamed(name string, ch map[string]interface{}) (Store, error) {
	if name == "none" {
		// 本地后端**不进缓存**：root 在构造期从 STORAGE_DIR 读取，
		// 缓存会让「改目录」在进程内永久失效（测试里更明显：第一个临时目录被冻结）。
		// 构造本身只是取一个字符串，没有连接池可复用。
		return newLocalStore(name, nil)
	}
	b, ok := builders[name]
	if !ok {
		return nil, fmt.Errorf("%w: 未知存储后端 %s", ErrBackendGone, quoteName(name))
	}
	return cachedStore(name, ch, func() (Store, error) {
		st, err := b(name, ch)
		if err != nil {
			return nil, backendGonef(err, "后端 %s 无法构建", quoteName(name))
		}
		return st, nil
	})
}

// BackendState 是一个后端的可用性与不可用原因（供清理告警与指标消费）。
type BackendState struct {
	Name   string
	Usable bool
	// Reason ∈ ok | not_configured | credential_unreadable | incomplete
	Reason string
}

// backendBrokenCreds 记录「解密失败的凭据属于哪个后端」。
//
// 为什么需要单独记：解密失败会把字段置空（避免把密文当 AK 发出去），
// 之后构建失败的表现与「运营漏填」完全一样。两者的处置不同
// （一个要重新填凭据，一个要检查主密钥），所以必须能区分。
var (
	credMu     sync.RWMutex
	credBroken = map[string]bool{}
)

// MarkCredentialFailures 接收 `channel.field` 形式的字段路径，标记对应后端凭据不可读。
//
// 由配置装载（db.LoadSystemConfig）调用：它是全表装载的唯一入口，
// 启动引导与周期热更新都经过它。
func MarkCredentialFailures(fields []string) {
	credMu.Lock()
	defer credMu.Unlock()
	credBroken = make(map[string]bool, len(fields))
	for _, f := range fields {
		if i := strings.IndexByte(f, '.'); i > 0 {
			credBroken[f[:i]] = true
		}
	}
}

// BackendStates 返回全部已注册后端的可用性快照（顺序固定：none 在最前，其余按名升序）。
func BackendStates() []BackendState {
	out := []BackendState{{Name: "none", Usable: true, Reason: "ok"}}
	names := make([]string, 0, len(builders))
	for name := range builders {
		if name != "none" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	credMu.RLock()
	broken := credBroken
	credMu.RUnlock()

	for _, name := range names {
		if broken[name] {
			out = append(out, BackendState{Name: name, Reason: "credential_unreadable"})
			continue
		}
		_, err := For(name)
		switch {
		case err == nil:
			out = append(out, BackendState{Name: name, Usable: true, Reason: "ok"})
		case errors.Is(err, ErrNotConfigured):
			out = append(out, BackendState{Name: name, Reason: "not_configured"})
		default:
			out = append(out, BackendState{Name: name, Reason: "incomplete"})
		}
	}
	return out
}

// UsableBackends 返回当前**能构建**的全部后端（含 none），供清理任务过滤候选行。
//
// 为什么清理任务需要它：候选查询是 `ORDER BY expires_at LIMIT batch`。
// 若最老的 N 行所属后端不可构建，逐条跳过后每轮都会选中同一批，
// 后面的正常行永远排不上 —— 清理队列被饿死。查询里先过滤掉不可构建的行即可，
// 被过滤掉的数量由清理侧单独 COUNT 并告警（不能静默）。
//
// 判据与 For 完全同源（BackendStates 内部就是 For），不另写一份「配置是否完整」的检查。
func UsableBackends() []string {
	states := BackendStates()
	out := make([]string, 0, len(states))
	for _, st := range states {
		if st.Usable {
			out = append(out, st.Name)
		}
	}
	return out
}

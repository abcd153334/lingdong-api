package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/service/keyhealth"
)

// 选路权重平滑因子：有效权重 = binding.weight + 10（v2.0 §4.6.2 ④ / C-05）。
const weightSmoothing = 10

// ErrNoKey 表示渠道密钥池为空（没配）或密钥列本身损坏。
var ErrNoKey = errors.New("relay: 渠道密钥池为空")

// ErrNoCandidate 表示本渠道已经没有**本次请求未尝试过**的可用密钥。
//
// 它只出现在「同渠道换 key 重试」走到头的情况：不是配置错、也不是上游故障，
// 调用方应当直接换下一个候选渠道，不能记一次内部错误（那会把"重试到上限"
// 误算成"渠道配置全坏"，最终对外回一个错误的 503）。
var ErrNoCandidate = errors.New("relay: 本渠道可用密钥均已尝试")

// NoUsableKeyError 表示池里有 key，但**一把都不可用**（全部被禁用或全部解密失败）。
//
// 与 ErrNoKey 的区别只是"可解释性"，对外都是 5xx：
// 空池是"没配"，全坏是"主密钥变了/数据损坏"，全禁用是"运营手动关了"。
// 三者共用同一个类型 + 计数，指标按 reason 区分，避免为每种情况各造一个 error。
type NoUsableKeyError struct {
	Total    int // token 总数
	Broken   int // 解密失败条数
	Disabled int // 解密成功但被禁用条数
}

func (e *NoUsableKeyError) Error() string {
	switch {
	case e.Broken == e.Total && e.Total > 0:
		return fmt.Sprintf("relay: 渠道 %d 枚密钥全部无法解密（加密主密钥可能已变更）", e.Total)
	case e.Disabled == e.Total && e.Total > 0:
		return fmt.Sprintf("relay: 渠道 %d 枚密钥全部已被禁用", e.Total)
	default:
		return fmt.Sprintf("relay: 渠道密钥均不可用（共 %d 枚，损坏 %d、禁用 %d）", e.Total, e.Broken, e.Disabled)
	}
}

// Reason 返回给指标用的封闭值域（disabled / broken / mixed）。
func (e *NoUsableKeyError) Reason() string {
	switch {
	case e.Broken > 0 && e.Disabled == 0:
		return "broken"
	case e.Disabled > 0 && e.Broken == 0:
		return "disabled"
	default:
		return "mixed"
	}
}

// AllKeysCoolingError 表示该渠道**所有**密钥都在冷却中（P1-8）。
//
// 与 ErrNoKey 必须严格区分，因为对外呈现完全不同：
//   - ErrNoKey 是配置错（密钥池空/解密失败），改配置才能好 → 5xx + 告警；
//   - 全冷却只是「等一会儿」，继续挑一枚冷却中的 key 去打只会把上游账号级限流
//     拖得更久（Ark 的限流是账号级，多枚 key 往往同账号，一次 429 就能全冷却），
//     所以必须对外回 429 + Retry-After，而不是 503 让人以为服务挂了。
type AllKeysCoolingError struct {
	// RetryAfter 是最短剩余冷却时长；≤0 表示未知（Redis 查询失败）。
	RetryAfter time.Duration
	// Auth 表示池中至少有一枚 key 是因**鉴权失败**冷却的：
	// 这种冷却不会自己变好（要换 key），对外必须报 5xx 而不是 429。
	Auth bool
}

func (e *AllKeysCoolingError) Error() string {
	cause := "全部处于冷却中"
	if e.Auth {
		cause = "因鉴权失败被冷却，需要更换密钥"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("relay: 渠道密钥%s（约 %d 秒后恢复）", cause, int(e.RetryAfter/time.Second)+1)
	}
	return "relay: 渠道密钥" + cause
}

// Candidate 是一次可用的转发目标（渠道 + 生效上游模型 + 层内权重 + 绑定层优先级）。
type Candidate struct {
	Channel       *model.Channel
	UpstreamModel string
	Weight        int
	// Priority 是 model_bindings.priority（P3-02）。语义与 channels.priority 一致：**小者优先**。
	// 此前落库时被硬编码为 1、选路完全不读，运营在界面上填了也不生效。
	//
	// 为什么保留两级而不是合并成一个数：渠道优先级是「线路等级」（同一渠道对所有模型一致），
	// 绑定优先级是「模型维度的线路偏好」（同一个渠道对 A 模型是主线路、对 B 模型是兜底）。
	// 合并成一个数会让改一个模型的偏好，连带改变其它模型的选路。
	Priority int
}

// SelectCandidates 组装候选（v2.0 §4.6.2）：model_bindings(enabled) ∩ channels(status=1, 分组命中)，
// 先按 channels.priority 升序分层，层内再按 model_bindings.priority 升序分层，
// 最终同层内按 binding.weight 加权随机（有效权重 = weight+10）。
// 返回空切片表示无可用渠道（路由预检失败，须在冻结前拦截）。
func SelectCandidates(modelID, group string) ([]Candidate, error) {
	return selectCandidates(modelID, group, false)
}

// SelectCandidatesForAdmin 是管理台内部调用（AI 编排）的选路：**不做分组过滤**。
//
// 为什么单独一个入口：管理台没有「调用方分组」这个概念，运营给自己配的 AI
// 不应该因为渠道只挂在 `vip` 分组而不可用 —— 那会让运营看到"模型明明存在却用不了"。
// 其余约束（绑定启用 + 渠道启用）一条不少：它们是"这个模型现在能不能用"的事实，
// 与谁在调用无关。
func SelectCandidatesForAdmin(modelID string) ([]Candidate, error) {
	return selectCandidates(modelID, "", true)
}

// selectCandidates 是 SelectCandidates / SelectCandidatesForAdmin 的唯一实现。
func selectCandidates(modelID, group string, ignoreGroup bool) ([]Candidate, error) {
	var bindings []model.ModelBinding
	if err := db.DB.Where("model_id = ? AND enabled = ?", modelID, true).Find(&bindings).Error; err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return nil, nil
	}
	ids := make([]uint64, 0, len(bindings))
	for _, b := range bindings {
		ids = append(ids, b.ChannelID)
	}
	var channels []model.Channel
	if err := db.DB.Where("id IN ? AND status = ?", ids, 1).Find(&channels).Error; err != nil {
		return nil, err
	}
	byID := make(map[uint64]*model.Channel, len(channels))
	for i := range channels {
		byID[channels[i].ID] = &channels[i]
	}

	out := make([]Candidate, 0, len(bindings))
	for _, b := range bindings {
		ch, ok := byID[b.ChannelID]
		if !ok {
			continue // 渠道不存在或非启用态
		}
		if !ignoreGroup && !groupMatches(ch.Groups, group) {
			continue
		}
		upstream := b.UpstreamModel
		if upstream == "" {
			upstream = modelID
		}
		out = append(out, Candidate{
			Channel: ch, UpstreamModel: upstream, Weight: b.Weight, Priority: b.Priority,
		})
	}

	// 两级分层键：(渠道优先级, 绑定优先级)。两级都升序，组内加权随机。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Channel.Priority != out[j].Channel.Priority {
			return out[i].Channel.Priority < out[j].Channel.Priority
		}
		return out[i].Priority < out[j].Priority
	})
	for i := 0; i < len(out); {
		j := i + 1
		for j < len(out) &&
			out[j].Channel.Priority == out[i].Channel.Priority &&
			out[j].Priority == out[i].Priority {
			j++
		}
		shuffleWeighted(out[i:j])
		i = j
	}
	return out, nil
}

// groupMatches 判断渠道分组是否命中调用分组；渠道未配置分组时视作 default，空分组按 default 处理。
func groupMatches(raw model.JSON, group string) bool {
	groups := decodeStringList(raw)
	if len(groups) == 0 {
		groups = []string{"default"}
	}
	if group == "" {
		group = "default"
	}
	for _, g := range groups {
		if g == group {
			return true
		}
	}
	return false
}

// shuffleWeighted 对同层候选做加权随机洗牌（无放回）。
func shuffleWeighted(list []Candidate) {
	for i := len(list) - 1; i > 0; i-- {
		total := 0
		for _, c := range list[:i+1] {
			total += effWeight(c.Weight)
		}
		if total <= 0 {
			return
		}
		r := rand.Intn(total)
		acc, pick := 0, i
		for k, c := range list[:i+1] {
			acc += effWeight(c.Weight)
			if r < acc {
				pick = k
				break
			}
		}
		list[i], list[pick] = list[pick], list[i]
	}
}

// effWeight 返回平滑后的有效权重。
func effWeight(w int) int {
	if w < 0 {
		w = 0
	}
	return w + weightSmoothing
}

// KeyRef 是选中的一枚密钥。
//
// ID 是明文 HMAC 指纹（稳定标识），Key 是明文，Index 只用于"第几把"的展示。
// **所有 key 级状态（冷却/禁用/用量归属）只能用 ID**：下标会随"编辑保存整体
// 替换密钥池"而指向另一把 key，且不会报任何错。
type KeyRef struct {
	Key   string
	Index int
	ID    string
}

// PickKey 按渠道 multi_key_mode 从密钥池选一枚可用密钥（polling 用 Redis 游标，random 用随机）。
//
// exclude 是本次请求已尝试过的 keyID 集合（同渠道换 key 重试）；nil = 不排除。
// 错误优先级（调用方按它决定对外语义）：
//  1. ErrNoKey          —— 池为空/密钥列损坏（配置事故）
//  2. NoUsableKeyError  —— 有 key 但全部被禁用或无法解密（配置事故）
//  3. ErrNoCandidate    —— 本请求已把可用 key 试完（换下一个渠道即可）
//  4. AllKeysCoolingError —— 剩下的全在冷却（限流 → 429；含鉴权 → 5xx）
func PickKey(ch *model.Channel, exclude map[string]struct{}) (KeyRef, error) {
	return PickKeyDecrypted(ch, channelkeys.Decrypt(ch.Keys), exclude)
}

// PickKeyDecrypted 是 PickKey 的「已解密」版本：轮询批内一次性解密后复用。
//
// 为什么需要它：200 条任务同一渠道时，旧实现要解密 200 次（每轮 5 秒），
// 纯 CPU 浪费。**判定与轮转必须只有一份**（selectUsable + nextKeyIndex），
// 这里只是把"解密"这一步挪到批外，不允许在别处再写一套选钥逻辑。
//
// 不跨轮缓存明文：密钥进长生命周期缓存会带来「轮换后旧 key 仍可用」「内存转储可读」
// 两个问题，而单轮复用已经消掉了 199/200 的开销。
func PickKeyDecrypted(ch *model.Channel, res channelkeys.Result, exclude map[string]struct{}) (KeyRef, error) {
	// 坏 key 必须可见：单条 token 解密失败不会让请求失败（还有别的 key），
	// 没有这条计数时它会一直躺在池子里，直到某天只剩它。
	// 按 kind 分档（token / invalid_column），值域封闭，不做渠道维度（基数控制）。
	if res.Invalid {
		metrics.Inc(metrics.NameChannelKeyDecryptFailures, "kind", "invalid_column")
	} else if res.Failed > 0 {
		metrics.Inc(metrics.NameChannelKeyDecryptFailures, "kind", "token")
	}
	if res.Invalid || res.Total == 0 {
		return KeyRef{}, ErrNoKey
	}
	usable, err := selectUsable(ch.ID, res, channelkeys.ParseMeta(ch.KeyMeta), exclude)
	if err != nil {
		return KeyRef{}, err
	}
	idx := 0
	if ch.MultiKeyMode == "polling" {
		if n, ok := nextKeyIndex(ch.ID, len(usable)); ok {
			idx = n
		} else {
			idx = rand.Intn(len(usable))
		}
	} else {
		idx = rand.Intn(len(usable))
	}
	e := usable[idx]
	return KeyRef{Key: e.Plain, Index: e.Index, ID: e.ID}, nil
}

// selectUsable 返回可选密钥（剔除禁用/解密失败/已尝试/冷却中），是纯判定逻辑。
//
// 抽成独立函数是为了能穷举测「全禁用 vs 全坏 vs 全冷却 vs 已试完」这四种
// 只在故障时才会走到的分支 —— 它们恰好是最难在联调里触发的部分。
func selectUsable(channelID uint64, res channelkeys.Result, meta channelkeys.MetaMap,
	exclude map[string]struct{}) ([]channelkeys.Entry, error) {
	okEnabled := 0
	candidates := make([]channelkeys.Entry, 0, len(res.Entries))
	for _, e := range res.Entries {
		if !e.OK() {
			continue
		}
		if _, enabled := meta.Effective(e.ID); !enabled {
			continue
		}
		okEnabled++
		if _, tried := exclude[e.ID]; tried {
			continue
		}
		candidates = append(candidates, e)
	}
	if len(candidates) == 0 {
		// 区分「池里没有能用的」与「本次都试过了」：前者是配置事故（要 5xx + 告警），
		// 后者只是重试预算用尽（换下一个渠道即可，记内部错误会污染归因）。
		if okEnabled > 0 {
			return nil, ErrNoCandidate
		}
		broken := res.Failed
		if res.Invalid {
			broken = 1
		}
		return nil, &NoUsableKeyError{Total: res.Total, Broken: broken, Disabled: res.Total - broken}
	}

	ids := make([]string, len(candidates))
	for i, e := range candidates {
		ids[i] = e.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cooling := keyhealth.CoolingState(ctx, channelID, ids)
	if len(cooling) == 0 {
		return candidates, nil
	}
	out := make([]channelkeys.Entry, 0, len(candidates))
	for _, e := range candidates {
		if _, cooled := cooling[e.ID]; cooled {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		// 整池冷却：区分「只是限流」与「有鉴权失效」——后者等多久都不会好。
		e := &AllKeysCoolingError{}
		for _, reason := range cooling {
			if reason == keyhealth.ReasonAuth {
				e.Auth = true
				break
			}
		}
		if ttl, ok := keyhealth.MinCoolingTTL(ctx, channelID, ids); ok {
			e.RetryAfter = ttl
		}
		return nil, e
	}
	return out, nil
}

// nextKeyIndex 用 Redis INCR 生成递增游标；Redis 不可用时回落随机。
func nextKeyIndex(channelID uint64, n int) (int, bool) {
	if !db.RedisReady() || n <= 1 {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, err := db.RDB.Incr(ctx, fmt.Sprintf("ld:ch:keyidx:%d", channelID)).Result()
	if err != nil {
		return 0, false
	}
	if v < 0 {
		v = 0
	}
	return int(v % int64(n)), true
}

// decodeStringList 解析 JSONB 字符串数组（非法/空返回 nil）。
func decodeStringList(raw model.JSON) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// channelConfig 由渠道与选定密钥构造适配器配置。
//
// 声明式配置在这里解析一次：解析失败不降级为"无配置"，而是记 DeclErr，
// 由适配器在构造请求前直接返回——脏数据必须 fail-closed，否则请求会打到错误路径而无人知道。
func channelConfig(ch *model.Channel, key string) *ChannelConfig {
	cfg := &ChannelConfig{ID: ch.ID, Name: ch.Name, BaseURL: ch.BaseURL, APIKey: key, Timeout: DefaultAdapterTimeout}
	// 私有配置必须先解析：下面的 DeclErr 分支会提前 return，
	// 放在它后面的话，一条脏 adapter_config 就会让 Options 永远是空的——
	// 适配器于是静默用内置默认 workflow，运营改了配置却看不出没生效。
	if raw := bytes.TrimSpace([]byte(ch.AdapterOptions)); len(raw) > 0 && string(raw) != "null" {
		if json.Valid(raw) {
			cfg.Options = json.RawMessage(raw)
		} else {
			cfg.OptionsErr = &ConfigError{Scope: "渠道适配器配置错误", Field: "adapter_options", Msg: "不是合法的 JSON"}
		}
	}
	decl, err := ParseAdapterConfig([]byte(ch.AdapterConfig))
	if err != nil {
		cfg.DeclErr = err
		return cfg
	}
	cfg.Decl = decl
	return cfg
}

// ChannelConfigFor 由渠道与选定密钥构造适配器配置（S7 异步任务服务复用）。
func ChannelConfigFor(ch *model.Channel, key string) *ChannelConfig { return channelConfig(ch, key) }

// attempt 是尝试队列里的一项：一个候选渠道 + 本次请求已试过的 keyID 集合。
//
// 用队列而不是双重 for：转发主体里有大量 `continue`（跳过当前候选去试下一个），
// 套一层内层循环会让所有 `continue` 的语义悄悄变成"换 key"，
// 那是一处必然出错、且只在故障时才暴露的改动。
type attempt struct {
	cand    Candidate
	exclude map[string]struct{}
}

// insertKeyRetry 把「同渠道、换一把 key」插到当前位置之后（紧邻重试，不插队）。
//
// 不追加到队尾：低优先级渠道会插队，优先级配置在这条路径上就失效了。
func insertKeyRetry(q []attempt, qi int, cand Candidate, exclude map[string]struct{}, keyID string) []attempt {
	next := make(map[string]struct{}, len(exclude)+1)
	for k := range exclude {
		next[k] = struct{}{}
	}
	next[keyID] = struct{}{}
	out := make([]attempt, 0, len(q)+1)
	out = append(out, q[:qi+1]...)
	out = append(out, attempt{cand: cand, exclude: next})
	return append(out, q[qi+1:]...)
}

// shouldSwitchKey 判断这次失败是否值得「换同渠道的另一把 key 再试一次」。
//
// 只认三类：鉴权失败（这把 key 废了）、限流（这把被限了）、连接失败（没拿到响应）。
// 调用方错误（用户参数错）换 key 也还是错；上游 5xx 换 key 大概率还是 5xx，
// 白付一次上游调用；主动超时/提交歧义换 key 会造出两份上游产物 —— 这些一律不换。
func shouldSwitchKey(o outcome, status int, enabled bool, budget int) bool {
	if !enabled || budget <= 0 {
		return false
	}
	switch o {
	case outcomeAuth, outcomeRateLimit:
		return true
	case outcomeUpstream:
		return status <= 0
	}
	return false
}

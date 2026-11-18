package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/channel"
)

// dryRunTTL 是探测凭据的有效期。
//
// 24h 是经验值：短于它，运营第二天回来改个名字就要重探；长于它，
// 上游换证书/换网关地址后旧凭据仍然放行。
const dryRunTTL = 24 * time.Hour

// probeKeyFPNone 是"没有密钥"的指纹哨兵。
//
// 不能用空串表示"无密钥"：空串与"指纹计算出错"无法区分，
// 而两者的处置完全不同（前者允许匹配，后者必须失败）。
const probeKeyFPNone = "-"

// NormalizeBaseURL 把 base_url 归一成可比对的形态（**唯一实现**，写读共用）。
//
// 归一四件事：去空白 → 去全部尾斜杠 → scheme/host 小写 → 去默认端口。
// 不去 path/query：有的网关把租户放在 path、把 token 放在 query，删掉就改了语义。
// 解析失败时退化为「去空白 + 去尾斜杠」，**绝不返回空**——返回空会让目标永远匹配不到，
// 表现是"探测做过了仍然要求探测"。
func NormalizeBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	trimmed := strings.TrimRight(s, "/")
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return trimmed
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	// 去掉默认端口：https://a.com:443 与 https://a.com 是同一个目标。
	if (u.Scheme == "https" && strings.HasSuffix(u.Host, ":443")) ||
		(u.Scheme == "http" && strings.HasSuffix(u.Host, ":80")) {
		u.Host = u.Host[:strings.LastIndex(u.Host, ":")]
	}
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

// probeKeyOf 是一条探测凭据的键（**唯一实现**）：归一后的地址 + 密钥指纹。
//
// 写入（recordProbe）与读取（keyFPsOf）都只能经过它。早前两处各算一遍，
// 读取侧漏了 sessionID（传了 0），指纹恒不相等 —— 表现是"探过了仍要求探测"，
// 而回执还告诉运营"密钥已更换"，把排障方向引到完全错误的地方。
func probeKeyOf(sessionID uint64, adapterID, baseURL, apiKey string) (base, fp string) {
	base = NormalizeBaseURL(baseURL)
	fp = probeKeyFingerprint(sessionID, strings.TrimSpace(adapterID), base, apiKey)
	return base, fp
}

// probeKeyFingerprint 计算密钥指纹（不存明文，也不可逆）。
//
// 输入带 session_id 当盐：同一条密钥在不同会话得到不同指纹 —— 库被读走也无法
// 拿来做跨会话关联或撞库。空密钥返回哨兵而不是空串。
//
// 盐意味着**读写两侧必须传同一个 sessionID**：不要单独调用本函数，
// 一律走 probeKeyOf（它保证地址归一与指纹用同一份输入）。
func probeKeyFingerprint(sessionID uint64, adapterID, baseURL, apiKey string) string {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return probeKeyFPNone
	}
	sum := sha256.Sum256([]byte("v1|" + itoa64(sessionID) + "|" + adapterID + "|" + baseURL + "|" + key))
	return hex.EncodeToString(sum[:])[:16]
}

func itoa64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// RecordDryRun 落一条探测记录。
//
// 60 秒内同会话同目标同指纹已有一条成功记录时直接复用（回执带 cached），
// 避免模型在同一步里连点两次白烧上游额度。
//
// 落库失败**不影响**本次探测结论：探测本身是读操作，凭据写不进去该由闸门
// 在 apply 时拒绝，而不是把"上游其实通了"报成失败。
func RecordDryRun(ctx context.Context, rec *model.AgentDryRun) (bool, error) {
	if rec.ExpiresAt.IsZero() {
		rec.ExpiresAt = rec.CreatedAt.Add(dryRunTTL)
	}
	var prev model.AgentDryRun
	err := db.DB.WithContext(ctx).
		Where("session_id = ? AND adapter_id = ? AND base_url = ? AND key_fp = ? AND ok = ? AND expires_at > ?",
			rec.SessionID, rec.AdapterID, rec.BaseURL, rec.KeyFP, true, time.Now()).
		Order("created_at DESC").Limit(1).Find(&prev).Error
	if err == nil && prev.ID != 0 && rec.OK && time.Since(prev.CreatedAt) < time.Minute {
		rec.Ref = prev.Ref
		return true, nil
	}
	if err := db.DB.WithContext(ctx).Create(rec).Error; err != nil {
		return false, err
	}
	return false, nil
}

// ProbeTarget 是"这条提案需要探测的某个目标"及其现状。
type ProbeTarget struct {
	AdapterID string `json:"adapter_id"`
	BaseURL   string `json:"base_url"`
	// Source 指回是哪条操作要求的（operations[i]），运营据此定位。
	Source string `json:"source"`
	// Satisfied 表示已有有效凭据（同会话 + 同适配器 + 同地址 + 同密钥 + 未过期）。
	Satisfied bool   `json:"satisfied"`
	Reason    string `json:"reason"` // ok | missing | expired | failed | key_mismatch
	Status    int    `json:"status,omitempty"`
	Message   string `json:"message,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// ProbeStatus 是一条提案的探测状态（闸门与提案卡共用同一份计算）。
//
// Required=false 时 Targets 也是空切片而不是 nil：响应里出现 null 会让前端
// `.length` 抛错，而那是 HTTP 200 的整页崩（本项目已经踩过一次）。
type ProbeStatus struct {
	Required  bool          `json:"required"`
	Satisfied bool          `json:"satisfied"`
	Targets   []ProbeTarget `json:"targets"`
}

// probeNeed 是一次"上线前必须探测"的目标（由 plan 推导）。
type probeNeed struct {
	AdapterID string
	BaseURL   string
	KeyFPs    []string
	Source    string
}

// channelRef 是 plan 里引用到的渠道现状（批量预加载，避免逐个查库）。
type channelRef struct {
	ID        uint64
	BaseURL   string
	AdapterID string
	Keys      []string // 已解密：与探测侧同一口径
}

// probeNeedsOf 从提案里推导出需要探测的目标（**纯函数**：渠道现状由调用方预加载）。
//
// 判据（与文档一致）：
//   - 新建渠道：一律需要（没探过就上线等于拿生产流量试错）；
//   - 改渠道：只有触碰 base_url 或 adapter_id 才需要（只改名/改价不需要）。
//
// adapter_id / base_url 为空时用库内现值补齐：只改 base_url 的提案若不补，
// 目标会缺适配器，判定就会退化成"只比地址"，换适配器也能过闸。
func probeNeedsOf(sessionID uint64, p *Plan, chans map[uint64]*channelRef) []probeNeed {
	out := make([]probeNeed, 0, 2)
	for i, op := range p.Operations {
		if op.Entity != EntityChannel || op.Op == OpDisable {
			continue
		}
		var d ChannelData
		if err := strictUnmarshal(op.Data, &d); err != nil {
			continue
		}
		cur := chans[op.ID]
		need := probeNeed{
			AdapterID: strings.TrimSpace(d.AdapterID),
			BaseURL:   NormalizeBaseURL(d.BaseURL),
			Source:    "operations[" + itoa64(uint64(i)) + "]",
		}
		if op.Op != OpCreate {
			if !rawHasKey(op.Data, "base_url") && !rawHasKey(op.Data, "adapter_id") {
				continue
			}
			if cur != nil {
				if need.BaseURL == "" {
					need.BaseURL = NormalizeBaseURL(cur.BaseURL)
				}
				if need.AdapterID == "" {
					need.AdapterID = cur.AdapterID
				}
			}
		}
		if need.BaseURL == "" {
			// 地址为空的目标无法探测也无法比对：跳过（否则闸门永远过不去）。
			continue
		}
		need.KeyFPs = keyFPsOf(sessionID, need, d, op, cur)
		out = append(out, need)
	}
	return out
}

// keyFPsOf 计算这条操作"会用哪些密钥"的指纹集合。
//
// 改渠道但不给 keys 时，必须用**已解密**的现值算 —— 直接拿密文算会得到
// 与探测侧完全不同的指纹，结果是"探过也说密钥换了"，永远过不去。
//
// 指纹用 need 的 adapter/base（补齐后的值），不是 data 里的原值：
// 只改 base_url 的操作，data 里根本没有 adapter_id。
func keyFPsOf(sessionID uint64, need probeNeed, d ChannelData, op Operation, cur *channelRef) []string {
	keys := make([]string, 0, len(d.Keys))
	for _, k := range d.Keys {
		if strings.TrimSpace(k) != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 && op.Op != OpCreate && cur != nil {
		keys = cur.Keys
	}
	if len(keys) == 0 {
		return []string{probeKeyFPNone}
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		_, fp := probeKeyOf(sessionID, need.AdapterID, need.BaseURL, k)
		out = append(out, fp)
	}
	sort.Strings(out)
	return out
}

// loadChannelRefs 批量取 plan 引用到的渠道现状（一次查询）。
func loadChannelRefs(ctx context.Context, gdb *gorm.DB, ids []uint64) (map[uint64]*channelRef, error) {
	out := map[uint64]*channelRef{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []model.Channel
	if err := gdb.WithContext(ctx).
		Select("id", "base_url", "adapter_id", "keys").
		Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = &channelRef{
			ID: r.ID, BaseURL: r.BaseURL, AdapterID: r.AdapterID,
			Keys: channel.DecryptKeys(r.Keys),
		}
	}
	return out, nil
}

// loadDryRuns 批量取这些会话的探测记录（一次查询，按时间倒序）。
func loadDryRuns(ctx context.Context, gdb *gorm.DB, sessionIDs []uint64) (map[uint64][]model.AgentDryRun, error) {
	out := map[uint64][]model.AgentDryRun{}
	if len(sessionIDs) == 0 {
		return out, nil
	}
	var rows []model.AgentDryRun
	if err := gdb.WithContext(ctx).
		Where("session_id IN ?", sessionIDs).
		Order("created_at DESC, id DESC").Limit(2000).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.SessionID] = append(out[r.SessionID], r)
	}
	return out, nil
}

// probeStatusOf 是判定本身的唯一入口：给定目标与已取回的行，算出状态。
func probeStatusOf(sessionID uint64, plan *Plan, chans map[uint64]*channelRef, rows []model.AgentDryRun) ProbeStatus {
	needs := probeNeedsOf(sessionID, plan, chans)
	st := ProbeStatus{Required: len(needs) > 0, Satisfied: true, Targets: make([]ProbeTarget, 0, len(needs))}
	for _, n := range needs {
		t := matchProbe(n, rows)
		if !t.Satisfied {
			st.Satisfied = false
		}
		st.Targets = append(st.Targets, t)
	}
	return st
}

// ProbeStatusFor 计算单条提案的探测状态（apply 闸门与详情端点用）。
func ProbeStatusFor(ctx context.Context, gdb *gorm.DB, sessionID uint64, plan *Plan) (ProbeStatus, error) {
	if gdb == nil {
		return ProbeStatus{}, errors.New("agent: 数据库未就绪，无法判定探测凭据")
	}
	chans, err := loadChannelRefs(ctx, gdb, referencedChannelIDs(plan))
	if err != nil {
		return ProbeStatus{}, err
	}
	runs, err := loadDryRuns(ctx, gdb, []uint64{sessionID})
	if err != nil {
		return ProbeStatus{}, err
	}
	return probeStatusOf(sessionID, plan, chans, runs[sessionID]), nil
}

// ProbeStatusBatch 计算一批提案的探测状态（列表页用）。
//
// 固定 **两次** 查询（channels 一次、agent_dry_runs 一次），与提案条数无关。
// 只算"还没被应用/驳回的变更提案"：已终态的提案算出来也没人看，
// 却要为它们各解一遍 plan。
func ProbeStatusBatch(ctx context.Context, gdb *gorm.DB, props []model.AgentProposal) (map[uint64]ProbeStatus, error) {
	type item struct {
		id        uint64
		sessionID uint64
		plan      *Plan
	}
	out := map[uint64]ProbeStatus{}
	if gdb == nil || len(props) == 0 {
		return out, nil
	}
	items := make([]item, 0, len(props))
	chanIDSet := map[uint64]bool{}
	sessSet := map[uint64]bool{}
	sesses := make([]uint64, 0, 4)
	for _, p := range props {
		if p.Kind != ProposalKindChange || p.Status != model.ProposalDraft {
			continue
		}
		plan, err := DecodePlan(p.Plan)
		if err != nil {
			// 单条解析失败不该拖垮整页：跳过它，前端按"未知"渲染。
			continue
		}
		items = append(items, item{id: p.ID, sessionID: p.SessionID, plan: plan})
		if !sessSet[p.SessionID] {
			sessSet[p.SessionID] = true
			sesses = append(sesses, p.SessionID)
		}
		for _, id := range referencedChannelIDs(plan) {
			chanIDSet[id] = true
		}
	}
	if len(items) == 0 {
		return out, nil
	}
	ids := make([]uint64, 0, len(chanIDSet))
	for id := range chanIDSet {
		ids = append(ids, id)
	}
	chans, err := loadChannelRefs(ctx, gdb, ids)
	if err != nil {
		return out, err
	}
	runs, err := loadDryRuns(ctx, gdb, sesses)
	if err != nil {
		return out, err
	}
	for _, it := range items {
		out[it.id] = probeStatusOf(it.sessionID, it.plan, chans, runs[it.sessionID])
	}
	return out, nil
}

// referencedChannelIDs 收集 plan 里按 id 引用的渠道（批量预加载的键集合）。
func referencedChannelIDs(p *Plan) []uint64 {
	if p == nil {
		return nil
	}
	out := make([]uint64, 0, 2)
	for _, op := range p.Operations {
		if op.Entity == EntityChannel && op.ID != 0 {
			out = append(out, op.ID)
		}
	}
	return out
}

// ProbeTargets 计算"这条提案需要哪些探测、现在满不满足"。
//
// 保留切片返回值：apply 的闸门按"缺哪几个"构造错误，不需要整份状态。
func ProbeTargets(ctx context.Context, gdb *gorm.DB, sessionID uint64, plan *Plan) ([]ProbeTarget, error) {
	st, err := ProbeStatusFor(ctx, gdb, sessionID, plan)
	if err != nil {
		return nil, err
	}
	return st.Targets, nil
}

// matchProbe 在已取回的行里找"这个目标"的凭据。
//
// **只有最新一条参与判定**：rows 已按 created_at DESC 排序。
// 历史行只能解释"为什么最新那条不通过"（例如换了密钥），不能反过来否定它 ——
// 早前把"任意一条历史记录过期"当成"当前已过期"，于是补探成功后仍然报过期，
// 症状与"探过了还要求探测"完全一样。
func matchProbe(n probeNeed, rows []model.AgentDryRun) ProbeTarget {
	t := ProbeTarget{AdapterID: n.AdapterID, BaseURL: n.BaseURL, Source: n.Source}
	fpSet := make(map[string]bool, len(n.KeyFPs))
	for _, fp := range n.KeyFPs {
		fpSet[fp] = true
	}
	var latest, mismatch *model.AgentDryRun
	for i := range rows {
		r := &rows[i]
		if r.AdapterID != n.AdapterID || r.BaseURL != n.BaseURL {
			continue
		}
		if latest == nil {
			latest = r
		}
		if mismatch == nil && !fpSet[r.KeyFP] {
			mismatch = r
		}
	}
	switch {
	case latest == nil && mismatch != nil:
		// 地址探过，但用的是另一把密钥。
		t.Reason = "key_mismatch"
		t.Status = mismatch.Status
		t.Message = mismatch.Message
		t.CheckedAt = mismatch.CreatedAt.Format(time.RFC3339)
		return t
	case latest == nil:
		t.Reason = "missing"
		return t
	case !fpSet[latest.KeyFP]:
		// 最新那次用的是别的密钥：换密钥必须重探（否则拿 B 密钥上线直接 401）。
		t.Reason = "key_mismatch"
		t.Status = latest.Status
		t.Message = latest.Message
		t.CheckedAt = latest.CreatedAt.Format(time.RFC3339)
		return t
	case latest.ExpiresAt.Before(time.Now()):
		t.Reason = "expired"
		t.Status = latest.Status
		t.Message = latest.Message
		t.CheckedAt = latest.CreatedAt.Format(time.RFC3339)
		return t
	case !latest.OK:
		t.Reason = "failed"
		t.Status = latest.Status
		t.Message = latest.Message
		t.CheckedAt = latest.CreatedAt.Format(time.RFC3339)
		return t
	}
	t.Satisfied = true
	t.Reason = "ok"
	t.Status = latest.Status
	t.CheckedAt = latest.CreatedAt.Format(time.RFC3339)
	t.ExpiresAt = latest.ExpiresAt.Format(time.RFC3339)
	return t
}

// unsatisfied 过滤出未满足的目标。
func unsatisfied(ts []ProbeTarget) []ProbeTarget {
	out := make([]ProbeTarget, 0, len(ts))
	for _, t := range ts {
		if !t.Satisfied {
			out = append(out, t)
		}
	}
	return out
}

// ErrDryRunRequired 保留为哨兵：控制器用 errors.Is 分流成 409。
var ErrDryRunRequired = errors.New("agent: 该提案会新建渠道或改上游地址，必须先做一次连通性探测")

// DryRunRequiredError 带"缺哪些目标、怎么补"，而不是一句笼统的拒绝。
//
// 只给一句话的后果是运营只能反复点"确认应用"、反复问模型"我已经探过了"，
// 因为没人告诉他探的是不是同一个地址、同一个密钥。
type DryRunRequiredError struct {
	Targets []ProbeTarget
}

func (e *DryRunRequiredError) Error() string {
	if len(e.Targets) == 0 {
		return strings.TrimPrefix(ErrDryRunRequired.Error(), "agent: ")
	}
	parts := make([]string, 0, len(e.Targets))
	for _, t := range e.Targets {
		parts = append(parts, t.BaseURL+"（"+probeReasonText(t.Reason)+"）")
	}
	return "需要先探测 " + itoa64(uint64(len(e.Targets))) + " 个目标：" +
		strings.Join(parts, "；") + "。在同一会话里让它重新执行一次连通性探测即可，凭据保留 24 小时。"
}

// Is 让 errors.Is(err, ErrDryRunRequired) 仍然成立（控制器分支不改）。
func (e *DryRunRequiredError) Is(target error) bool { return target == ErrDryRunRequired }

func probeReasonText(reason string) string {
	switch reason {
	case "missing":
		return "未探测"
	case "expired":
		return "凭据已过期，需重探"
	case "failed":
		return "上次探测未通过"
	case "key_mismatch":
		return "密钥已更换，需重探"
	default:
		return reason
	}
}

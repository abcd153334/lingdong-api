// Package metrics 是**零依赖**的 Prometheus 文本格式指标注册表（B7 §2.10）。
//
// 为什么不引 prometheus/client_golang：
//  1. 本仓库当前离线可构建（GOPROXY=off、依赖已 vendor 在 GOMODCACHE），新增依赖会让
//     「拿到代码就能 build」这条底线失效；
//  2. 需要的指标只有三种最基础的类型（counter / gauge / histogram），语义完全确定，
//     手写 exposition 比引入一整套 SDK 更少出错面；
//  3. 单进程单实例、无 pushgateway、无 exemplar、无 native histogram —— SDK 的能力用不上。
//
// 设计约束（改这里之前先读）：
//   - **标签基数即内存**：只在「值域可枚举」的维度上打标签（端点用路由模板而不是真实路径，
//     渠道用 id 而不是 base_url）。任何把用户输入/模型名/UUID 当标签的写法都会把进程打爆。
//   - 输出顺序**必须确定**（按 name → labels 排序）：否则每次抓取字节都不同，做 diff 与
//     基线快照都无从谈起。
//   - 采集函数（RegisterGaugeFunc）在抓取时执行：实现里必须有超时，禁止在这里做长耗时查询。
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Type 是指标类型（Prometheus exposition 的 TYPE 行）。
type Type string

// 支持的类型：只有这三种，缺的类型说明需求还没出现，不要提前加。
const (
	TypeCounter   Type = "counter"
	TypeGauge     Type = "gauge"
	TypeHistogram Type = "histogram"
)

// Def 是一条指标的元数据（HELP/TYPE）。
type Def struct {
	Type Type
	Help string
}

// 指标名常量：**唯一权威清单**，调用方只允许用这些常量，避免字面量漂移。
const (
	NameGatewayRequests    = "gateway_requests_total"
	NameGatewayLatency     = "gateway_latency_seconds"
	NameTaskInflight       = "task_inflight"
	NameTaskSettleFailures = "task_settle_failures_total"
	// NameTaskStatusUnmatched 统计「上游状态词不被认识」的次数（按 adapter 分档）。
	//
	// 为什么必须单独一条：不认识的状态此前一律按 running 处理，于是 status_map 少写一个值
	// 的表现是「任务一直跑到 deadline 才退款」—— 既没有日志也没有曲线，
	// 用户已经在投诉"任务卡住了"，而运维手上什么都没有。
	NameTaskStatusUnmatched = "task_status_unmatched_total"
	// NameTaskScopeMissing 统计「查任务时没传归属」的次数（按调用点分档）。
	//
	// 这是**编程错误**的计数器，正常恒为 0。它一旦非 0，就说明某条路径能跨用户
	// 读任务；空 Scope 已从「不加条件」改成「拒绝」，这条曲线是用来抓漏改的调用点。
	NameTaskScopeMissing = "task_scope_missing_total"
	NameChannelFailures     = "channel_failures_total"
	// NameChannelKeyUnusable 统计「渠道里有 key 但一把都用不了」的次数（按 reason 分档：
	// empty / disabled / broken / mixed）。
	//
	// 这三种原因要改的东西完全不同（补 key / 启用 key / 换加密主密钥），混成一个数
	// 等于只告诉运维"坏了"。改造前这种情况完全静默：解密失败被直接跳过，
	// 表现为"渠道看起来有 5 把 key，实际一把都用不了"。
	NameChannelKeyUnusable = "channel_key_unusable_total"
	// NameChannelKeyDecryptFailures 统计密钥池里解密失败的条数（含列本身损坏）。
	//
	// 它回答的是"有没有 key 正在悄悄失效"：单条 token 坏掉不会让请求失败
	// （还有别的 key），所以没有这条曲线时，坏 key 会一直躺在池子里没人发现。
	NameChannelKeyDecryptFailures = "channel_key_decrypt_failures_total"
	NameUsageAnomaly              = "usage_anomaly_total"
	NameTaskPurgedTotal           = "task_purged_total"
	NameTaskRetentionReport       = "task_retention_candidates"
	NameStorageBytes              = "storage_bytes"
	NameStorageDiskFreeRate       = "storage_disk_free_ratio"
	// NamePayNotifyUnconfigured 统计「回调打到一个未配置密钥的支付渠道」的次数。
	// 这不是流量噪音（验签失败的请求才走 NameGatewayRequests），而是配置事故：
	// 有人在收款链路留了一条空密钥渠道，必须让运维看见。
	NamePayNotifyUnconfigured = "pay_notify_unconfigured_total"
	// NamePayNotifyDropped 统计「验签通过、金额与订单相等，但状态串不认识」的回调
	// （按渠道分档）。
	//
	// 这是最容易被忽略的一类丢单：上游新增了终态、改了大小写、或换了事件名，
	// 我们既不认识那个状态、也不会入账，订单永远停在 pending，而用户的钱确实
	// 已经付了。没有这条曲线时，唯一的发现渠道是客诉。
	NamePayNotifyDropped = "pay_notify_dropped_total"
	// NameKlingSound 统计「可灵型号认不出版本号、按支持同声音画处理」的次数。
	//
	// 为什么需要它：sound 不下发时上游照样出片、照样收费，只是没声音 ——
	// 用户要到看完才发现。曲线非零说明有新型号命名没被版本正则覆盖，
	// 需要确认上游是否真的支持同声音画。
	NameKlingSound = "adapter_kling_sound_total"
	// NamePayQueryUnsigned 统计「主动查单的响应没带签名、或无法可靠重算签名」的次数
	// （按渠道分档）。
	//
	// 查单是个**能改订单状态**的入口：响应说"已支付"就直接入账。而它一直没验签，
	// 唯一防线是 HTTPS。这条曲线回答「到底有多少笔入账是没有密码学来源保证的」，
	// 也是决定"能不能升级成强制验签"的唯一依据。
	NamePayQueryUnsigned = "pay_query_unsigned_total"
	// NamePayDuplicatePaid 统计「同一订单收到不同交易号的已支付通知」的次数
	// （按渠道分档）。
	//
	// 这类事件本身不改金额（订单已是终态，幂等短路），但每出现一次都可能是
	// 上游串单 / 重复扣款 / 把别人的单推到我们头上，必须有人看得见。
	NamePayDuplicatePaid = "pay_duplicate_paid_total"
	// NameTaskRecheckTotal 统计终态复查的每轮结果（按 result 分档，标签基数封闭）。
	//
	// 必须分档而不是只数总数：`hit`（上游还在跑）/ `settled`（捞回并补收）/
	// `free`（捞回但没收钱）/ `miss`（上游也不认了）/ `error`（我们这边出错）
	// 这五种的运维含义完全不同 —— 混成一个数，看板上只知道"复查在跑"。
	NameTaskRecheckTotal = "task_recheck_total"
	// NameTaskRecheckCharge 累计迟到补收的金额（微）。
	//
	// 单看 task_recheck_total 看不出"补回来了多少钱"，
	// 而这条曲线正好回答「上游超时到底让我们白送了多少」。
	NameTaskRecheckCharge = "task_recheck_charge_micro_total"
	// NameUsageSettleClosed 统计「会话已被别人结过」的次数（按阶段 settle/release 分档）。
	//
	// 这条曲线是三个静默错账的共同出口：回收任务抢先结算、迟到补结抢先、
	// 任务侧先 Settle 后 CAS 输给 Expire/Cancel。不数它，这些场景永远查不出来。
	NameUsageSettleClosed = "usage_settle_closed_total"
	// NameUsageShortCharge 统计「应收 > 实收」的次数（按 reason 分档）。
	//
	// 与 charge_capped（正常截顶）分开：这条只统计**非预期**少收，
	// 是「平台替用户垫上游成本」的唯一可观测入口。
	NameUsageShortCharge = "usage_short_charge_total"
	// NameUsageReclaim 统计回收任务每轮的处理结果（按 result 分档，值域封闭）。
	//
	// 为什么必须分档：`charged`（补收）/ `released_after_success` / `released_empty` /
	// `released_failed` / `released_superseded` / `released_no_evidence` / `error`
	// 的运维含义完全不同。混成一个数，看板上只知道"回收在跑"，不知道"退了多少"。
	NameUsageReclaim = "usage_reclaim_total"
	// NameUsageReclaimRefund 累计回收退款金额（微，按 result 分档）。
	// 它的 `released_no_evidence` 分档就是「平台可能替用户垫了上游成本」的敞口上限。
	NameUsageReclaimRefund = "usage_reclaim_refund_micro_total"
	// NameUsageOverCharge 统计「按冻结额兜底入账」的次数（charge_calc_failed）。
	NameUsageOverCharge = "usage_over_charge_total"
	// NameRateLimitDegraded 统计 Redis 不可用期间匿名资源面的进程内兜底判定
	// （按 scope 与 result=allow/deny 分档）。
	//
	// 为什么单列：降级期"没拦住"和"拦过头"都是事故，但看板上都表现为"限流没报错"。
	// 有 deny 分档才能判断阈值是否合理，有 allow 分档才能知道降级持续了多久。
	NameRateLimitDegraded = "ratelimit_degraded_total"
	// NameChannelLimitDegraded 统计「渠道并发/RPM 闸回落到进程内」的次数（按 scope 分档）。
	//
	// 多副本部署时，回落意味着真实上限变成「配置值 × 副本数」——那是唯一会导致
	// 上游配额被打爆的降级路径，必须有曲线能看见它持续了多久。
	NameChannelLimitDegraded = "channel_limit_degraded_total"

	// ── 存储写入位置（"现在到底写哪"） ──
	//
	// 这两个指标是「配置丢了但服务还在跑」的唯一可视证据：没有它们，
	// 写路径 503 只能靠用户报错发现，而"上次是哪天开始坏的"无从判断。
	NameStorageWriteReady = "storage_write_ready"
	NameStorageActive     = "storage_active_backend"
	// NameArtifactStoreFailures 是产物转存失败次数（按 reason 分档）。
	// 转存失败不改任务终态（用户仍能实时回源取到），所以只有这个计数能暴露它。
	NameArtifactStoreFailures = "storage_artifact_failures_total"

	// NameUploadCompress 是上传图片压缩的判定结果（applied / skipped / failed）。
	//
	// 压缩是「静默生效」的功能：跳过与失败都不影响上传成功，用户不会察觉，
	// 所以「开关打开了但一张都没压」只能靠这个计数看出来。
	NameUploadCompress = "upload_compress_total"
	// 清理链路（见 defs 里的同名说明）。
	NameStoragePurgeTotal    = "storage_purge_total"
	NameStoragePurgeBlocked  = "storage_purge_blocked"
	NameStorageBackendUsable = "storage_backend_usable"
	NameStorageOrphans       = "storage_orphans"
	NameStorageGCTotal       = "storage_gc_total"
	NameWorkerJobPanic       = "worker_job_panic_total"
)

// defs 是指标元数据表（name → Def）。
var defs = map[string]Def{
	NameGatewayRequests:     {TypeCounter, "网关请求总数（按路由模板与 HTTP 状态码）"},
	NameGatewayLatency:      {TypeHistogram, "网关请求耗时分布（秒）"},
	NameTaskInflight:        {TypeGauge, "处于 queued/running 的异步任务数"},
	NameTaskSettleFailures:  {TypeCounter, "异步任务结算失败次数（按失败环节）"},
	NameTaskStatusUnmatched: {TypeCounter, "上游任务状态词未被识别的次数（按适配器）"},
	NameChannelFailures:     {TypeCounter, "渠道级失败次数（仅计入渠道健康的失败）"},
	NameChannelKeyUnusable: {TypeCounter,
		"渠道密钥全不可用次数（按 reason=empty/disabled/broken/mixed；配置事故，不熔断）"},
	NameChannelKeyDecryptFailures: {TypeCounter,
		"密钥池中解密失败的条数（单条坏 key 不会让请求失败，所以要单独可见）"},
	NameUsageAnomaly: {TypeCounter, "用量异常标记次数（按 anomaly kind）"},
	NameKlingSound: {TypeCounter,
		"可灵 sound 判定次数（按 source；unknown_version 非零说明型号命名没被版本正则覆盖）"},
	NameTaskPurgedTotal:     {TypeCounter, "按保留策略清理的任务数"},
	NameTaskRetentionReport: {TypeGauge, "保留策略候选任务数（retention=0 时仅报告）"},
	NameStorageBytes:        {TypeGauge, "files 表登记的产物总字节数"},
	NameStorageDiskFreeRate: {TypeGauge, "本地存储目录所在盘的可用空间占比（0-1）"},
	NamePayNotifyUnconfigured: {TypeCounter,
		"支付回调命中未配置密钥的渠道次数（配置事故，非流量噪音）"},
	NamePayNotifyDropped: {TypeCounter,
		"验签通过但支付状态不可识别的回调次数（按渠道分档；订单会永远 pending）"},
	NamePayQueryUnsigned: {TypeCounter,
		"查单响应未带签名/无法重算签名的次数（按渠道；到账仍按响应结果处理）"},
	NamePayDuplicatePaid: {TypeCounter,
		"同一订单收到不同交易号的已支付通知次数（按渠道；不改金额，但要人工核）"},
	NameTaskRecheckTotal: {TypeCounter,
		"终态复查每轮的结果（hit/settled/free/miss/skipped/error/artifacts_lost/upstream_failed）"},
	NameTaskRecheckCharge: {TypeCounter, "迟到成功补收的累计金额（微）"},
	NameUsageSettleClosed: {TypeCounter,
		"结算/释放时会话已关闭的次数（按 phase=settle/release）"},
	NameUsageShortCharge: {TypeCounter, "非预期少收次数（按 reason 分档）"},
	NameUsageOverCharge:  {TypeCounter, "按冻结额兜底入账的次数（按原因分档）"},
	NameUsageReclaim:     {TypeCounter, "回收任务处理结果（charged/released_*/error）"},
	NameUsageReclaimRefund: {TypeCounter,
		"回收退款累计金额（微，按 result 分档）"},
	NameRateLimitDegraded: {TypeCounter,
		"Redis 不可用时的进程内兜底限流判定（按 scope 与 result 分档）"},
	NameStorageWriteReady: {TypeGauge,
		"存储写入是否就绪（1=有可用的 storage 配置，0=未配置，写路径会 503）"},
	NameStorageActive: {TypeGauge,
		"当前生效的存储后端（按 backend 标签，命中的为 1）"},
	NameArtifactStoreFailures: {TypeCounter,
		"任务产物转存失败次数（按 reason 分档）"},
	NameUploadCompress: {TypeCounter,
		"上传图片压缩判定次数（按 result=applied/skipped/failed）"},
	// 清理链路的可观测性：没有这三个指标时，「清理没跑」与「没有到期件」
	// 在监控上完全一样，而它们一个是事故、一个是正常。
	NameStoragePurgeTotal: {TypeCounter,
		"到期对象清理的逐行结局（kind=upload/artifact，result=gone/unverified/failed/missed）"},
	NameStoragePurgeBlocked: {TypeGauge,
		"到期但本轮清不掉的行数（reason=backend/bucket，配置修好后应归零）"},
	NameStorageBackendUsable: {TypeGauge,
		"每个存储后端当前是否可构建（1/0，含 credential_unreadable 这类原因）"},
	NameStorageOrphans: {TypeGauge,
		"孤儿对账发现的对象数（按 bucket；DB 无记录但桶里还在）"},
	NameStorageGCTotal: {TypeCounter,
		"孤儿对账逐条结局（按 bucket 与 result）"},
	NameWorkerJobPanic: {TypeCounter,
		"Worker Job panic 次数（已隔离，进程继续）"},
}

// defaultBuckets 是耗时直方图的桶边界（秒）：覆盖 5ms 到 60s，够网关与上游调用用。
var defaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// histogram 是累计桶 + 总和 + 计数（Prometheus 经典直方图）。
type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

// gaugeFunc 是抓取时求值的采集函数。
type gaugeFunc struct {
	name   string
	labels string
	fn     func() float64
}

// seriesKey 是「指标名 + 标签集」的唯一键。
//
// 标签集直接存编码后的字符串（`k="v",k2="v2"`）：它本身就是输出格式，
// 省一次结构体比较，也保证同一组标签只有一条时间序列。
type seriesKey struct {
	name   string
	labels string
}

var (
	mu         sync.Mutex
	counters   = map[seriesKey]float64{}
	gauges     = map[seriesKey]float64{}
	histograms = map[seriesKey]*histogram{}
	gaugeFuncs []gaugeFunc
)

// Inc 计数 +1（counter 语义，永不回退）。
func Inc(name string, labelPairs ...string) { Add(name, 1, labelPairs...) }

// Add 给 counter 加 v（v 必须非负；负数忽略，避免出现违反单调性的时间序列）。
func Add(name string, v float64, labelPairs ...string) {
	if v < 0 || math.IsNaN(v) {
		return
	}
	k := seriesKey{name: name, labels: encodeLabels(labelPairs)}
	mu.Lock()
	counters[k] += v
	mu.Unlock()
}

// Set 覆盖写 gauge 的当前值。
func Set(name string, v float64, labelPairs ...string) {
	k := seriesKey{name: name, labels: encodeLabels(labelPairs)}
	mu.Lock()
	gauges[k] = v
	mu.Unlock()
}

// Observe 记录一次观测值到直方图（负值/NaN 丢弃：耗时不可能为负，出现即上游 bug）。
func Observe(name string, v float64, labelPairs ...string) {
	if v < 0 || math.IsNaN(v) {
		return
	}
	k := seriesKey{name: name, labels: encodeLabels(labelPairs)}
	mu.Lock()
	h := histograms[k]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(defaultBuckets))}
		histograms[k] = h
	}
	for i, b := range defaultBuckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.count++
	mu.Unlock()
}

// RegisterGaugeFunc 注册抓取时求值的采集函数（用于需要查库的指标）。
//
// 与 Set 的分工：能增量维护的用 Set（零额外开销），需要精确全量口径的用本函数
// （例如 task_inflight —— 增量维护在进程重启后会永久偏移，且没法自愈）。
//
// 同一 (name, labels) **重复注册会覆盖**旧函数：路由构造在多处测试里会被调用多次，
// 追加语义会产生重复序列（Prometheus 直接报 duplicate metrics）。
func RegisterGaugeFunc(name string, fn func() float64, labelPairs ...string) {
	if fn == nil {
		return
	}
	key := seriesKey{name: name, labels: encodeLabels(labelPairs)}
	mu.Lock()
	for i := range gaugeFuncs {
		if gaugeFuncs[i].name == key.name && gaugeFuncs[i].labels == key.labels {
			gaugeFuncs[i].fn = fn
			mu.Unlock()
			return
		}
	}
	gaugeFuncs = append(gaugeFuncs, gaugeFunc{name: key.name, labels: key.labels, fn: fn})
	mu.Unlock()
}

// Reset 清空全部时间序列与采集函数（**仅供单测**使用）。
func Reset() {
	mu.Lock()
	counters = map[seriesKey]float64{}
	gauges = map[seriesKey]float64{}
	histograms = map[seriesKey]*histogram{}
	gaugeFuncs = nil
	mu.Unlock()
}

// encodeLabels 把 [k,v,k,v...] 编码成 Prometheus 标签串。
//
// 奇数个参数说明调用方写错了：直接丢弃最后一个（宁可少一个标签，也不要 panic 到业务路径上），
// 并在输出里留下空标签名可被契约测试抓到。
func encodeLabels(pairs []string) string {
	if len(pairs) < 2 {
		return ""
	}
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pairs[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(pairs[i+1]))
		b.WriteByte('"')
	}
	return b.String()
}

// escapeLabelValue 按 exposition 规范转义标签值（反斜杠、双引号、换行）。
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// snapshot 是抓取时刻的一致性快照（持锁复制，避免边输出边被写）。
type snapshot struct {
	counters   map[seriesKey]float64
	gauges     map[seriesKey]float64
	histograms map[seriesKey]histogram
	gaugeFuncs []gaugeFunc
}

// takeSnapshot 在持锁状态下复制全部时间序列（counter 单调累积，抓取不重置）。
func takeSnapshot() snapshot {
	mu.Lock()
	defer mu.Unlock()
	s := snapshot{
		counters:   make(map[seriesKey]float64, len(counters)),
		gauges:     make(map[seriesKey]float64, len(gauges)),
		histograms: make(map[seriesKey]histogram, len(histograms)),
		gaugeFuncs: append([]gaugeFunc(nil), gaugeFuncs...),
	}
	for k, v := range counters {
		s.counters[k] = v
	}
	for k, v := range gauges {
		s.gauges[k] = v
	}
	for k, h := range histograms {
		s.histograms[k] = histogram{
			counts: append([]uint64(nil), h.counts...), sum: h.sum, count: h.count,
		}
	}
	return s
}

// Handler 返回 /metrics 的 HTTP 处理器（Prometheus text exposition v0.0.4）。
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := Render()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(body))
	})
}

// Render 生成 exposition 文本（导出给单测与排障脚本直接调用）。
func Render() string {
	s := takeSnapshot()
	// 采集函数在锁外执行：它们要查库，持锁执行会把所有计数路径一起堵死。
	collected := make(map[seriesKey]float64, len(s.gaugeFuncs))
	for _, g := range s.gaugeFuncs {
		collected[seriesKey{name: g.name, labels: g.labels}] = safeCall(g.fn)
	}

	// 汇总所有出现过的指标名（含只在采集函数里出现的）。
	names := make(map[string]struct{}, len(s.counters)+len(s.gauges)+len(s.histograms)+len(collected))
	for k := range s.counters {
		names[k.name] = struct{}{}
	}
	for k := range s.gauges {
		names[k.name] = struct{}{}
	}
	for k := range s.histograms {
		names[k.name] = struct{}{}
	}
	for k := range collected {
		names[k.name] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)

	var b strings.Builder
	for _, name := range ordered {
		d, known := defs[name]
		if known && d.Help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, d.Help)
			fmt.Fprintf(&b, "# TYPE %s %s\n", name, d.Type)
		}
		switch {
		case known && d.Type == TypeCounter:
			writeSimple(&b, name, seriesOf(s.counters, name))
		case known && d.Type == TypeGauge:
			writeSimple(&b, name, mergeSeries(s.gauges, collected, name))
		case known && d.Type == TypeHistogram:
			writeHistograms(&b, name, s.histograms)
		default:
			// 未登记的指标名：仍然输出（否则线上新加的指标会静默消失），
			// 但没有 HELP/TYPE —— 契约测试按「全部指标名必须登记」来兜底。
			//
			// 同名序列**必须合并成一张表**：counter 与 gauge 各写一遍会输出
			// 重复的 `name{labels}` 行，Prometheus 解析直接报 duplicate metrics。
			merged := seriesOf(s.counters, name)
			for k, v := range seriesOf(s.gauges, name) {
				merged[k] = v
			}
			for k, v := range collected {
				if k.name == name {
					merged[k] = v
				}
			}
			writeSimple(&b, name, merged)
			writeHistograms(&b, name, s.histograms)
		}
	}
	return b.String()
}

// safeCall 执行采集函数并吞掉 panic（一个坏采集器不该让整个 /metrics 500）。
func safeCall(fn func() float64) (v float64) {
	defer func() {
		if r := recover(); r != nil {
			v = math.NaN()
		}
	}()
	return fn()
}

// mergeSeries 把「已存值」与「本次采集值」合并到同一张表（采集值优先）。
func mergeSeries(stored, collected map[seriesKey]float64, name string) map[seriesKey]float64 {
	out := seriesOf(stored, name)
	for k, v := range collected {
		if k.name == name {
			out[k] = v
		}
	}
	return out
}

// seriesOf 过滤出某个指标名下的全部序列。
func seriesOf(src map[seriesKey]float64, name string) map[seriesKey]float64 {
	out := make(map[seriesKey]float64, 4)
	for k, v := range src {
		if k.name == name {
			out[k] = v
		}
	}
	return out
}

// writeSimple 输出 counter/gauge 的序列行（顺序确定）。
func writeSimple(b *strings.Builder, name string, series map[seriesKey]float64) {
	keys := make([]seriesKey, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].labels < keys[j].labels })
	for _, k := range keys {
		b.WriteString(k.name)
		if k.labels != "" {
			b.WriteByte('{')
			b.WriteString(k.labels)
			b.WriteByte('}')
		}
		b.WriteByte(' ')
		b.WriteString(formatFloat(series[k]))
		b.WriteByte('\n')
	}
}

// writeHistograms 输出直方图（_bucket 含 le="+Inf" 兜底桶 + _sum + _count）。
func writeHistograms(b *strings.Builder, name string, series map[seriesKey]histogram) {
	keys := make([]seriesKey, 0, len(series))
	for k := range series {
		if k.name == name {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].labels < keys[j].labels })
	for _, k := range keys {
		h := series[k]
		for i, bound := range defaultBuckets {
			b.WriteString(name)
			b.WriteString("_bucket{")
			if k.labels != "" {
				b.WriteString(k.labels)
				b.WriteByte(',')
			}
			b.WriteString(`le="`)
			b.WriteString(formatFloat(bound))
			b.WriteString(`"} `)
			b.WriteString(strconv.FormatUint(h.counts[i], 10))
			b.WriteByte('\n')
		}
		// +Inf 桶等于总数：少了它 Prometheus 的 rate() 会算错。
		b.WriteString(name)
		b.WriteString("_bucket{")
		if k.labels != "" {
			b.WriteString(k.labels)
			b.WriteByte(',')
		}
		b.WriteString(`le="+Inf"} `)
		b.WriteString(strconv.FormatUint(h.count, 10))
		b.WriteByte('\n')

		b.WriteString(name)
		b.WriteString("_sum")
		writeLabelsSuffix(b, k.labels)
		b.WriteByte(' ')
		b.WriteString(formatFloat(h.sum))
		b.WriteByte('\n')

		b.WriteString(name)
		b.WriteString("_count")
		writeLabelsSuffix(b, k.labels)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatUint(h.count, 10))
		b.WriteByte('\n')
	}
}

// writeLabelsSuffix 输出 `{labels}`（空标签集时什么都不写）。
func writeLabelsSuffix(b *strings.Builder, labels string) {
	if labels == "" {
		return
	}
	b.WriteByte('{')
	b.WriteString(labels)
	b.WriteByte('}')
}

// formatFloat 用最短可逆表示输出浮点（避免 1.1000000000000001 这类噪声）。
func formatFloat(v float64) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	if math.IsInf(v, -1) {
		return "-Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

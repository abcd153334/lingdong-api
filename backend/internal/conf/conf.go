package conf

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
)

// Env 是从环境变量加载的进程级配置。
type Env struct {
	AppEnv        string
	ListenAddr    string
	DatabaseURL   string
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	// JWTSecret 已废弃：登录态改为服务端会话（user_sessions + httpOnly Cookie），
	// 不再签发任何可被离线伪造的令牌，因此没有"签名密钥"这个东西。
	// 字段暂时保留仅为兼容既有 .env（多余环境变量不影响启动），代码中不再读取。
	JWTSecret        string
	AppEncryptionKey string
	LogLevel         string
	// StorageDir 是本地文件存储根目录（storage.active=none|minio 时的本地副本根路径）。
	StorageDir string

	// NodeMode 是**这台机器**的角色：full=门户+API；api=只有 API。
	//
	// api 模式下未登记的对外路径一律 404（见 middleware.APISurfaceOnly）：
	// 门户、管理台、文档站都不存在，域名根也不可访问。
	//
	// 为什么只能放环境变量：这是每台机器不同的属性。放进 system_config 会变成
	// "任何一台保存一次就把所有节点（含主站）切成同一模式"，门户会整体消失。
	NodeMode string
	// ClusterMode 表示这台机器与别的机器共用一个数据库（多节点部署）。
	//
	// 唯一作用是禁止本地磁盘存储：多节点共库时对象只落在一台机器的盘上，
	// 其它节点读同一个 key 必然 410。单机部署保持 0，行为与改造前一致。
	ClusterMode bool

	// TaskRetentionDays 是终态任务的保留天数（B7）：0 = **只报告不删**（默认）。
	//
	// 默认关闭是刻意的：物理删除任务行不可逆，而「对账要不要留全量任务」是运营决策，
	// 不能由一次升级替用户决定。开启后按 created_at 删任务 + 产物对象 + files 行。
	TaskRetentionDays int
	// MetricsHTTP 控制是否暴露 /metrics（默认 0 = 关闭）。
	//
	// 默认关闭：/metrics 会暴露渠道 id、端点、失败率等内部拓扑信息，
	// 在未做网络隔离的部署上等于把内网结构挂在公网。
	MetricsHTTP bool
	// MetricsToken 是 /metrics 的访问口令（LD_METRICS_TOKEN）。
	//
	// **未配置则端点不挂载**（404）：只开 LD_METRICS_HTTP 但没配口令的部署，
	// 等于把一个匿名可读的内部拓扑接口留在公网。抓取端用 X-Metrics-Token 头。
	MetricsToken string

	// TrustedProxies 是允许提供 X-Forwarded-For 的反向代理列表（LD_TRUSTED_PROXIES，逗号分隔）。
	//
	// 空 = 不信任任何代理，ClientIP() 取 TCP 对端地址。**不要**在没搞清拓扑时填 0.0.0.0/0：
	// 那等于把 IP 白名单与限流分桶交给请求头，任何人都能声称自己是任意 IP。
	TrustedProxies string
	// CapacityWarnPercent 是磁盘水位告警阈值（已用百分比，默认 85）。
	CapacityWarnPercent int
	// CapacityWarnMinFreeGB 是磁盘剩余空间的绝对下限（GB，默认 5）：
	// 大容量盘按百分比永远告不了警（500G 盘留 10% 还有 50G），小盘按百分比又会太晚。
	CapacityWarnMinFreeGB int

	// ---- 调用记录详情（C1）：报文留存 ----
	//
	// 全部走环境变量而不是 system_config，理由有两条：
	//  1. `GET /admin/config` 的 27 键被 e2e 快照锁死（config.spec.ts），加第 28 键会让
	//     e2e 失败；更麻烦的是**回滚期旧后端会拒绝不认识的键**，整页保存直接 400。
	//  2. 这些是部署侧容量/合规参数，不是运营每次都要调的运营项。
	//
	// CallIOEnabled 控制是否留存原始报文（默认开）。关闭后详情页只剩扣费与耗时，
	// 已写入的行仍按保留期自然清理（不会立刻消失，否则回滚会看起来像丢数据）。
	CallIOEnabled bool
	// CallIOMaxKB 是单条请求报文的留存上限（默认 256 KiB，resp 取其 1/2、error 取其 1/16）。
	CallIOMaxKB int
	// CallIOBody 控制正文留存量：full（默认）留脱敏后的正文，meta 只留结构摘要。
	//
	// 为什么默认 full：2026-11-04 的多米故障里，排障者看到的是入站摘要，
	// 于是把「平台配置错」误判成「下游返回异常」，多排查了一轮。
	// 数据本身已脱敏（密钥/Bearer/data: URL 都替换为 ******）且 7 天自动清理，
	// 用「看不到用户发了什么」换几 KB 存储并不划算。需要合规收紧时显式设 meta。
	CallIOBody string
	// CallIOSSESave 控制流式是否额外留存完整 SSE 原文（默认关，只留拼接后的最终文本 + 帧数）。
	CallIOSSESave bool
	// CallIORetentionDays 是 call_io 的保留天数（默认 7，0 = 只报告不删）。
	CallIORetentionDays int
	// CallPreflightLog 控制预检期失败（401/402/403/429/400/413/501）是否落 0 费记录（默认开）。
	CallPreflightLog bool
	// UsageRollupDays 是 usage_daily 的重算回看天数（默认 3）。
	// 只回看 1 天会漏掉「跨天结算 / 回收补账 / 手工修数」这类迟到终态。
	UsageRollupDays int

	// Timezone 是业务时区（默认 Asia/Shanghai），决定「今日 / 本月 / 趋势分桶」的日界。
	//
	// 为什么不复用容器 TZ：容器的 TZ 同时影响日志时间戳与 DB 会话时区，
	// 而运营想要的「今天」是一个业务概念，不该被部署形态绑死。
	// 解析与用法唯一处：service/stats/day.go（非法值回落 UTC 并告警，不阻断启动）。
	Timezone string

	// DiagHoldToken 是「长等待探针」的开关与口令（默认空 = 端点不存在）。
	//
	// 用途：一次请求就能量出**整条代理链最多允许一个请求挂多久**。
	// 同步图像要等上游 8~9 分钟，而中间任何一层（nginx/CDN/负载均衡/穿透隧道）
	// 只要用默认读超时就会先断，客户端看到的是那一层的 504，后端却还在正常等——
	// 这类故障光看应用日志永远定位不到，必须能从外网逐层量。
	//
	// 默认关闭且 fail-closed 返回 404：没配口令时不能给公网留一个可以任意睡的端点。
	DiagHoldToken string

	// BuiltinPresets 控制「出厂预设」这一整套能力（默认 true）。
	//
	// 一个开关、三个消费点，语义必须一致：
	//   - 启动时 `presets.Seed` 按组写入内置渠道/模型/绑定（**全部停用、空密钥**）；
	//   - 启动时 `presets.Adopt` 把老库里匹配上的运营行认领为内置（只补标记）；
	//   - 管理台「恢复出厂」把内置行拉回出厂值（关掉时直接拒绝，不绕过 Seed 建行）。
	//
	// 关掉它 = 这台实例不要任何出厂内容（自定义发行版、或已有自己的初始化流程）。
	// 由此**不再有第二个"演示数据"开关**：历史上那个会把一条假密钥渠道写进启用态，
	// 与出厂预设重复表达同一件事，且运营无法区分哪条是假的（已整体删除）。
	BuiltinPresets bool

	// ---- 认证（Cookie 会话）----
	//
	// CookieSecure 控制会话 Cookie 的 Secure 属性（LD_COOKIE_SECURE）。
	//
	// 默认值按环境推导：dev 下为 false（http://localhost 不是安全上下文，
	// 带 Secure 的 Cookie 浏览器会直接丢弃 → 本地怎么登录都失败），
	// 其余环境默认 true。生产必须 true，否则同链路上的任何明文请求都会带走会话票据。
	//
	// **实际写 Cookie 时以 CookieSecureMode 为准**（见 authcookie）：auto 模式要按
	// 每个请求的协议决定，静态布尔值表达不了。这个字段保留给"想要一个粗粒度判断"的
	// 调用方，在 auto 下按环境给出保守值（prod 视为 true）。
	CookieSecure bool
	// CookieSecureMode 是 LD_COOKIE_SECURE 的归一值：auto（默认）/ true / false。
	//
	// auto 的语义：请求经 TLS（或入口声明 X-Forwarded-Proto=https）才加 Secure，
	// 明文 http 不加。这样「本机 http 先验一遍」与「生产上 TLS」用同一份配置，
	// 不需要用户为了第一次登录先配证书（那是 new-api 用两个变量解决、我们用零个）。
	// 代价是：入口忘了传 X-Forwarded-Proto 时，https 请求会拿到不带 Secure 的 Cookie
	// —— 所以模板里必须有那一行（deploy/edge/nginx/snippets/proxy-common.conf 已带）。
	CookieSecureMode string
	// AllowedOrigins 是允许跨域携带凭证的来源（LD_ALLOWED_ORIGINS，逗号分隔）。
	//
	// 空 = 只允许同源（默认）。同源部署（nginx 反代）下 CSRF 靠 Origin 校验即可，
	// 不需要任何 CORS 头。**不要**填 `*`：`*` 与 credentials 互斥，
	// 而写成回显任意 Origin 等于把 CSRF 防护交给攻击者。
	AllowedOrigins string
	// CaptchaTestCode 是测试环境的固定验证码（LD_CAPTCHA_TEST_CODE）。
	//
	// 仅 APP_ENV=dev|test 生效；生产设置它会被 ValidateSecrets 拒绝启动。
	// e2e 无法 OCR 图形验证码，只能靠它；真实校验由 Go 层用例覆盖。
	CaptchaTestCode string
}

// defaultCookieSecure 按环境推导 Cookie 的 Secure 默认值。
func defaultCookieSecure() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV"))) {
	case "", "dev", "test":
		return false
	}
	return true
}

// cookieSecureMode 归一 LD_COOKIE_SECURE。非法值按环境取保守档：
// prod 视作 true（宁可不登录，也不发明文会话），dev/test 视作 auto。
func cookieSecureMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LD_COOKIE_SECURE"))) {
	case "", "auto":
		return "auto"
	case "1", "true", "yes", "on":
		return "true"
	case "0", "false", "no", "off":
		return "false"
	}
	if defaultCookieSecure() {
		return "true"
	}
	return "auto"
}

var current Env

// Load 读取 .env（若存在）与环境变量，缺失项取默认值，并缓存到全局供各组件读取。
func Load() Env {
	_ = godotenv.Load()

	// Cookie 模式只解析一次，两个字段必须同源（分开各调一次迟早分叉）。
	csMode := cookieSecureMode()

	current = Env{
		// 不再默认 "dev"：缺省值会让「忘了设 APP_ENV」的部署自动获得 dev 的全部放宽
		// （Cookie 无 Secure、CSRF 来源放宽、公开示例密钥只告警）。空值由
		// ValidateSecrets 拒绝启动，把配置错误挡在进程入口而不是留到运行期。
		AppEnv:           strings.TrimSpace(os.Getenv("APP_ENV")),
		ListenAddr:       getenv("LISTEN_ADDR", ":8080"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		RedisAddr:        getenv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:    os.Getenv("REDIS_PASSWORD"),
		RedisDB:          getenvInt("REDIS_DB", 0),
		JWTSecret:        os.Getenv("JWT_SECRET"),
		AppEncryptionKey: os.Getenv("APP_ENCRYPTION_KEY"),
		LogLevel:         getenv("LOG_LEVEL", "info"),
		StorageDir:       getenv("STORAGE_DIR", "./data"),

		NodeMode:    nodeMode(os.Getenv("LD_NODE_MODE")),
		ClusterMode: getenvBool("LD_CLUSTER_MODE", false),

		TaskRetentionDays:     getenvInt("LD_TASK_RETENTION_DAYS", 0),
		MetricsHTTP:           getenvBool("LD_METRICS_HTTP", false),
		MetricsToken:          strings.TrimSpace(os.Getenv("LD_METRICS_TOKEN")),
		TrustedProxies:        strings.TrimSpace(os.Getenv("LD_TRUSTED_PROXIES")),
		CapacityWarnPercent:   getenvInt("LD_CAPACITY_WARN_PERCENT", 85),
		CapacityWarnMinFreeGB: getenvInt("LD_CAPACITY_MIN_FREE_GB", 5),

		CallIOEnabled:       getenvBool("LD_CALL_IO", true),
		CallIOMaxKB:         getenvInt("LD_CALL_IO_MAX_KB", 256),
		CallIOBody:          callIOBodyMode(os.Getenv("LD_CALL_IO_BODY")),
		CallIOSSESave:       getenvBool("LD_CALL_IO_SSE", false),
		CallIORetentionDays: getenvInt("LD_CALL_IO_RETENTION_DAYS", 7),
		CallPreflightLog:    getenvBool("LD_CALL_PREFLIGHT_LOG", true),
		UsageRollupDays:     getenvInt("LD_USAGE_ROLLUP_DAYS", 3),
		Timezone:            strings.TrimSpace(os.Getenv("LD_TZ")),

		DiagHoldToken: strings.TrimSpace(os.Getenv("LD_DIAG_HOLD_TOKEN")),

		BuiltinPresets: getenvBool("LD_BUILTIN_PRESETS", true),

		// CookieSecure 只作为粗粒度判断（prod 视为 true）；真正写 Cookie 用 CookieSecureMode。
		CookieSecure:     csMode == "true" || (csMode == "auto" && defaultCookieSecure()),
		CookieSecureMode: csMode,
		AllowedOrigins:   strings.TrimSpace(os.Getenv("LD_ALLOWED_ORIGINS")),
		CaptchaTestCode:  strings.TrimSpace(os.Getenv("LD_CAPTCHA_TEST_CODE")),
	}
	return current
}

// EnvConfig 返回已加载的进程级配置。
func EnvConfig() Env {
	e := current
	// 读边界归一：单测与工具可能不调用 Load，零值不该让对外契约出现空模式。
	// 生产路径上 Load 一定会填，这里只是保证 /api/health 的 mode 恒为 full|api。
	if e.NodeMode == "" {
		e.NodeMode = NodeModeFull
	}
	return e
}

// NodeMode 的两种取值。
const (
	NodeModeFull = "full"
	NodeModeAPI  = "api"
)

// IsAPIMode 表示本机只提供 API（不提供任何前端）。
func (e Env) IsAPIMode() bool { return e.NodeMode == NodeModeAPI }

// nodeMode 归一 LD_NODE_MODE：空 = full；其余保留归一后的值，交给 ValidateNodeMode 判定。
//
// 不在 Load 里直接 Fatal：Load 被测试与工具复用，拒绝启动必须由 main 决定。
func nodeMode(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return NodeModeFull
	}
	return v
}

// ValidateNodeMode 在启动最早期校验模式值。
//
// 为什么非法值必须拒绝启动：拼错（`LD_NODE_MODE=API` 大写没问题，但 `ap` / `only`
// 会被归一成未知值）时若静默按 full 跑，运维以为"已经关掉了前端"，实际门户还开着。
func ValidateNodeMode(e Env) error {
	switch e.NodeMode {
	case NodeModeFull, NodeModeAPI:
		return nil
	}
	return fmt.Errorf("LD_NODE_MODE 只能是 full 或 api（当前 %q）：full=门户+API，api=仅 API", e.NodeMode)
}

// SystemConfig 是 system_config 表的内存镜像（键名规则「模块.字段」），支持热更新。
type SystemConfig struct {
	mu     sync.RWMutex
	values map[string]json.RawMessage
	subs   []func(changed []string)
}

var systemConfig = &SystemConfig{values: make(map[string]json.RawMessage)}

// System 返回全局 system_config 内存镜像。
func System() *SystemConfig { return systemConfig }

// PortalEnabled 是「门户端是否开启」的**唯一读取点**（运营模式）。
//
// 缺键 / 脏值一律回落 true（= 改造前的行为）：把正常站点挡在门外，
// 比多放一次请求糟糕得多。三个消费点（会话闸、发券闸、注册闸）与
// /api/portal/config 的下发都必须走这里 —— 各自 Unmarshal 到 bool 零值
// 会让老库静默变成「纯服务端」。
func PortalEnabled() bool { return System().Bool("portal_enabled", true) }

// Get 按 key 取值（未设置时 ok=false）。
func (s *SystemConfig) Get(key string) (json.RawMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

// Subscribe 注册配置变更订阅者：热更新完成后按本次变更键集回调。
// 回调在写锁之外执行，但禁止在其中再次写配置（避免重入）。
func (s *SystemConfig) Subscribe(fn func(changed []string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

// notify 在锁外逐个回调订阅者。
func (s *SystemConfig) notify(changed []string) {
	s.mu.RLock()
	subs := make([]func([]string), len(s.subs))
	copy(subs, s.subs)
	s.mu.RUnlock()
	for _, fn := range subs {
		fn(changed)
	}
}

// Set 写入单个 key 并通知订阅者。
func (s *SystemConfig) Set(key string, value json.RawMessage) {
	s.mu.Lock()
	s.values[key] = value
	s.mu.Unlock()
	s.notify([]string{key})
}

// SetMany 在同一把写锁内写入整包配置，随后一次性通知订阅者。
//
// 为什么必须比逐个 Set 更强：结算口径由两个互不相干的键组成
// （site.currency / recharge_ratio）。逐个 Set 时存在一个真实窗口 ——
// 币种已改成 USD 而 qpc 还是人民币的 69444，此刻任何一次冻结都会
// 把「$1 = 69444 额度」写进快照，用户实付放大 7.2 倍，且事后无法复现。
// BillingBasis 在同一把读锁内读这两个键，配上 SetMany 的一次性写锁，
// 中间态才真正不可达。
func (s *SystemConfig) SetMany(values map[string]json.RawMessage) {
	if len(values) == 0 {
		return
	}
	keys := make([]string, 0, len(values))
	s.mu.Lock()
	for k, v := range values {
		s.values[k] = v
		keys = append(keys, k)
	}
	s.mu.Unlock()
	s.notify(keys)
}

// All 返回当前键集快照。
func (s *SystemConfig) All() map[string]json.RawMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]json.RawMessage, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// Reload 用给定键集整体替换内存镜像（热更新入口），随后通知订阅者。
func (s *SystemConfig) Reload(values map[string]json.RawMessage) {
	s.mu.Lock()
	s.values = values
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	s.notify(keys)
}

// ---------- 类型化读取（统一配置读取入口，供 S10 及后续步骤消费） ----------

// Int 读取整型键；缺键/解析失败/非正数时取 def。
func (s *SystemConfig) Int(key string, def int64) int64 {
	raw, ok := s.Get(key)
	if !ok {
		return def
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil || v <= 0 {
		return def
	}
	return v
}

// Float 读取浮点键；缺键/解析失败/非正数时取 def。
func (s *SystemConfig) Float(key string, def float64) float64 {
	raw, ok := s.Get(key)
	if !ok {
		return def
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil || v <= 0 {
		return def
	}
	return v
}

// Bool 读取布尔键；缺键/解析失败时取 def。
func (s *SystemConfig) Bool(key string, def bool) bool {
	raw, ok := s.Get(key)
	if !ok {
		return def
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}

// String 读取字符串键；缺键/解析失败时取 def。
func (s *SystemConfig) String(key string, def string) string {
	raw, ok := s.Get(key)
	if !ok {
		return def
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}

// Unmarshal 将键值解析到 out；缺键或解析失败返回 false。
func (s *SystemConfig) Unmarshal(key string, out any) bool {
	raw, ok := s.Get(key)
	if !ok {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// getenvBool 解析布尔型开关：接受 1/true/yes/on 与 0/false/no/off（大小写不敏感）。
//
// 非法值一律回落 def 而不是当成 false：`LD_METRICS_HTTP=treu` 静默变成「关闭」
// 比启动报错更难查（用户以为打开了，抓取端却一直 404）。
func getenvBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// callIOBodyMode 归一正文留存口径：默认 full；只有显式 meta 才收紧为摘要。
//
// 方向选择与 getenvBool 相反，但理由一致：这里的"保守"指**不丢排障信息**。
// 拼错（`LD_CALL_IO_BODY=met`）会落到 full —— 脱敏仍然生效，只是多存正文。
func callIOBodyMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "meta", "summary", "off", "0", "false":
		return "meta"
	}
	return "full"
}

// Package callio 负责调用记录的原始报文留存（C1 详情层）。
//
// 三条硬约束（决定了本包的形态）：
//  1. **零新增存储设施**：报文进 PG text（超 2KB 由 TOAST 自动压缩外置），
//     不引入对象存储 —— 省掉 key 管理、生命周期、跨后端差异与孤儿清理四类问题。
//  2. **原始优先**：请求存原始 body 文本（只脱敏、不重排），上游错误存原始响应体，
//     格式化交给前端。任何"先解析再序列化"的做法都会改变字段顺序与数字精度，
//     排障时反而对不上客户端实际发的东西。
//  3. **先脱敏后截断**：反过来会在上限处把 token 切成两半，使脱敏正则失配 ——
//     这一条已经在 task.sanitizeReason 上踩过一次，不能在报文这条路上重犯。
package callio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	neturl "net/url"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// 上限基准（KB）。resp 取 1/2、error 取 1/16，其余按 LD_CALL_IO_MAX_KB 缩放。
const (
	defaultMaxKB = 256
	respDivisor  = 2
	errDivisor   = 16
	// minLimitBytes 是下限：把开关调到 1KB 会让报文彻底不可读，
	// 但**不拦**（运维可能就是要极低配额），只保证不会被算成 0 而变成"全截断"。
	minLimitBytes = 1024
)

// Limits 是一次留存的大小上限（字节）。0 值表示未启用。
type Limits struct {
	Req   int
	Resp  int
	Error int
}

// LimitsOf 由环境变量计算上限；未启用时返回零值。
func LimitsOf() Limits {
	if !conf.EnvConfig().CallIOEnabled {
		return Limits{}
	}
	kb := conf.EnvConfig().CallIOMaxKB
	if kb <= 0 {
		kb = defaultMaxKB
	}
	base := kb * 1024
	if base < minLimitBytes {
		base = minLimitBytes
	}
	return Limits{
		Req:   base,
		Resp:  base / respDivisor,
		Error: base / errDivisor,
	}
}

// Enabled 报告是否开启报文留存。
func Enabled() bool { return conf.EnvConfig().CallIOEnabled }

// SSESaveEnabled 报告流式是否额外留存完整 SSE 原文（默认关，只留拼接文本 + 帧数）。
func SSESaveEnabled() bool { return conf.EnvConfig().CallIOSSESave }

// RetentionDays 返回保留天数（0 = 只报告不删）。
func RetentionDays() int { return conf.EnvConfig().CallIORetentionDays }

/* ------------------------------ 脱敏 ------------------------------ */

// 脱敏规则（顺序敏感：先整段抹私钥/内联数据，再抹键值，最后抹裸 token 形态）。
//
// 为什么不复用 relay.RedactUpstream：那是"抹上游主机名 + 抹密钥"的**错误文案**出口，
// 面向的是短字符串；报文是整段 JSON，需要按键名与值形态双维度处理。
// 但主机名规则这里也用不上 —— 原始报文本来就要保留 host（排障第一步就是看打到哪）。
var (
	// 1) 私钥整段（PEM 多行）。
	rePrivateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// 2) 内联二进制：data URL 与 b64_json 的值（体积巨大且含敏感图像内容）。
	reDataURL = regexp.MustCompile(`data:[a-zA-Z0-9/+.\-]+;base64,[A-Za-z0-9+/=\s]{16,}`)
	reB64JSON = regexp.MustCompile(`(?i)("b64_json"\s*:\s*)"(?:[^"\\]|\\.)*"`)
	// 3) JSON 键值：键名含密钥语义的，值整段替换。
	//    覆盖 *_key / *_secret / *_token / password / authorization / api-key 等。
	reJSONSecret = regexp.MustCompile(`(?i)("(?:[a-z0-9_.\-]*(?:authorization|api[_-]?key|apikey|secret|password|passwd|token|credential|private[_-]?key)[a-z0-9_.\-]*)"\s*:\s*)"(?:[^"\\]|\\.)*"`)
	// 4) 表单/查询串里的同名键（multipart 文本头、application/x-www-form-urlencoded）。
	reFormSecret = regexp.MustCompile(`(?i)((?:^|[&\s"'?])(?:api[_-]?key|apikey|key|token|access_token|secret|password|sign|signature)=)[^&\s"']+`)
	// 5) 裸 Bearer / sk- / ak- 形态（出现在 header 文本或被拼进 body 的场景）。
	//
	// 前缀形态**不加任何前导边界**：\b 与「前一个字符非字母数字」都会漏掉
	// `prefixsk-...` 这种拼接形态（RE2 没有 lookbehind，写不出来），
	// 而漏一次就是把密钥原样存进库。代价是英文单词里恰好含 `sk-xxx` 的片段会被打码
	// （如 `task-manager`），这在排障文案里只是可读性损失，与泄漏不是一个量级。
	reBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9\-._~+/]{8,}=*`)
	rePrefix = regexp.MustCompile(`(?:sk|ak|rk|pk|ld)-[A-Za-z0-9_\-]{6,}`)
)

// redacted 是统一的脱敏占位（与前端展示约定一致：6 个星号）。
const redacted = "******"

// Sanitize 抹掉报文里的敏感值，返回脱敏后的文本。
//
// 全部替换都保持**值的长度无关紧要**：直接把值换成 ******，不改键名、
// 不改字段顺序 —— 这样"原始"这个词才成立（能看出客户端发了哪些字段）。
func Sanitize(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	s := string(raw)
	s = rePrivateKey.ReplaceAllString(s, "-----BEGIN PRIVATE KEY-----"+redacted+"-----END PRIVATE KEY-----")
	s = reDataURL.ReplaceAllString(s, "data:;base64,"+redacted)
	s = reB64JSON.ReplaceAllString(s, `${1}"`+redacted+`"`)
	s = reJSONSecret.ReplaceAllString(s, `${1}"`+redacted+`"`)
	s = reFormSecret.ReplaceAllString(s, `${1}`+redacted)
	s = reBearer.ReplaceAllString(s, "Bearer "+redacted)
	s = rePrefix.ReplaceAllString(s, redacted)
	return []byte(s)
}

// bodyMeta 报告正文留存口径是否为 meta（只留结构摘要）。
//
// 默认是 **full**（留存脱敏后的正文）：排障时「看不到用户发了什么」的代价，
// 比多占几 KB 存储大得多。要合规收紧时显式设 LD_CALL_IO_BODY=meta。
func bodyMeta() bool { return conf.EnvConfig().CallIOBody == "meta" }

// bodyModeName 是本行两侧正文的口径标记（写进 call_io.body_mode，供前端显示）。
func bodyModeName() string {
	if bodyMeta() {
		return "meta"
	}
	return "full"
}

// metaScalarKeys 是 meta 口径下允许保留取值的标量键。
//
// 只放**枚举型**字段（模型名、对象类型、尺寸、质量、是否流式、条数）：
// 自由文本（prompt / input / instructions / user）一律不进库 —— 那正是用户内容。
var metaScalarKeys = []string{
	"model", "object", "status", "size", "quality", "stream", "n",
	"modalities", "response_format", "encoding_format",
}

// metaCountKeys 是 meta 口径下只保留**数组长度**的键。
var metaCountKeys = []string{"messages", "tools", "input", "choices", "data", "content"}

// summarizeBody 把一段正文替换成结构摘要（meta 口径）。
//
// 判据用 allowlist 而不是"删掉 content"：黑名单一定会漏（新增的多模态字段、
// 透传的 metadata、用户自定义参数…），而漏一次就是把用户内容写进留存 7 天的表。
//
// 返回的 JSON 里 `_meta` 标记"这不是原文"，避免排障时误以为客户端只发了这些字段。
func summarizeBody(raw []byte, kind string) []byte {
	if len(raw) == 0 {
		return raw
	}
	out := map[string]any{"_meta": kind, "bytes": len(raw)}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 非 JSON（multipart / 表单 / 纯文本）：只留大小，绝不猜内容。
		out["format"] = "non-json"
		return mustJSON(out)
	}
	// 多数上游把结果包在 data 里（{"data":[{...}]}）：摘要要下钻一层，
	// 否则响应摘要永远是 `data_count:1`，看不出模型和用量。
	if d, ok := doc["data"].([]any); ok && len(d) == 1 {
		if first, ok := d[0].(map[string]any); ok {
			for k, v := range first {
				if _, exists := doc[k]; !exists {
					doc[k] = v
				}
			}
		}
	}
	for _, k := range metaScalarKeys {
		v, ok := doc[k]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case bool, float64:
			out[k] = val
		case string:
			// 长字符串不是枚举值，是正文（prompt 也可能被塞进 "model" 之外的未知键）。
			if len([]rune(val)) <= 64 {
				out[k] = val
			}
		}
	}
	for _, k := range metaCountKeys {
		if arr, ok := doc[k].([]any); ok {
			out[k+"_count"] = len(arr)
		}
	}
	if u, ok := doc["usage"].(map[string]any); ok {
		nums := make(map[string]any, len(u))
		for k, v := range u {
			switch v.(type) {
			case float64:
				if len(nums) < 16 {
					nums[k] = v
				}
			}
		}
		if len(nums) > 0 {
			out["usage"] = nums
		}
	}
	if n := countImageParts(doc); n > 0 {
		out["image_count"] = n
	}
	return mustJSON(out)
}

// countImageParts 数一遍多模态图片/音频片段（只看形态，不取值）。
func countImageParts(doc map[string]any) int {
	n := 0
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if depth > 6 {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				switch k {
				case "image_url", "image", "input_image", "b64_json", "input_audio", "audio":
					n++
				}
				walk(child, depth+1)
			}
		case []any:
			for _, child := range t {
				walk(child, depth+1)
			}
		}
	}
	walk(doc, 0)
	return n
}

// mustJSON 序列化摘要；失败时回一个最小占位（绝不回退到原文）。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"_meta":"summary_failed"}`)
	}
	return b
}

/* ------------------------------ 写入 ------------------------------ */

// Rec 是一条待落库的报文。
type Rec struct {
	UsageID uint64
	UserID  uint64

	// ReqRaw 是**未脱敏**的原始请求体（Store 内部统一脱敏，调用方不需要也不应该先处理）。
	ReqRaw []byte
	// ReqSource 标注 ReqRaw 的可信度来源（raw/rebuilt/snapshot），空值表示历史行。
	ReqSource string
	RespRaw   []byte
	RespError []byte
	// DeliveredRaw 是**客户端真正收到**的响应体（改写/转码之后）。
	// 与 RespRaw 相同时留空：普通透传占绝大多数，存两份纯属浪费。
	DeliveredRaw []byte
	SSEFrames    int
	Media        []model.MediaItem
	RespBytesFor int64 // 二进制响应的真实字节数（内容不留存，仅记大小）
	// UpstreamRaw 是**实际发给上游**的报文（由传输层 tap 采集）。
	//
	// 与 ReqRaw 分开存：ReqRaw 是客户端发进来的，UpstreamRaw 是平台重建后的。
	// 只留前者时，「字段名/端点到底对不对」无从判断（2026-11-04 排障就卡在这里）。
	UpstreamRaw       []byte
	UpstreamURL       string
	UpstreamStatus    int
	UpstreamAttempts  int
	UpstreamTruncated bool
	// RespSource 标注响应体来源：raw / aggregate（流式拼接）/ usage / none。
	RespSource string
	// BodyMode 是本行正文口径（full/meta）。空值时由 Store 按当前配置补齐。
	BodyMode string
	// Patch 为 true 时只更新"本次确实有值"的列。
	//
	// 为什么必须存在：异步任务是"提交时写请求、终态时写响应"两次写，
	// 而 ON CONFLICT 的 DoUpdates 默认是**全列覆盖** —— 第二次写会把 req_raw 清成空串，
	// 表现为"任务成功那一刻请求参数消失"，且没有任何报错。
	Patch bool
}

// Store 落库一条报文（best-effort：失败只 Warn，绝不影响业务结果）。
//
// 同步执行的理由：它发生在**响应已写出之后**（同步链路的客户端已经拿到结果、
// 流式链路的流已结束），多几毫秒不影响任何人的等待时间；而异步队列会引入
// 「进程退出丢尾批」「测试无法断言」两个额外问题，换来的是这里根本不需要的收益。
func Store(ctx context.Context, rec Rec) bool {
	limits := LimitsOf()
	if limits.Req <= 0 || rec.UsageID == 0 {
		return false
	}
	// 只在**首写**时补口径：Patch 是终态补写，如果它也跟着当前配置改，
	// 运营把 full 切成 meta 之后，历史行（正文是 full 存的）会被标成 meta。
	if rec.BodyMode == "" && !rec.Patch {
		rec.BodyMode = bodyModeName()
	}
	row := model.CallIO{
		UsageID:    rec.UsageID,
		UserID:     rec.UserID,
		ExpiresAt:  expiryFrom(time.Now()),
		ReqSource:  normalizeReqSource(rec.ReqSource),
		BodyMode:   rec.BodyMode,
		RespSource: normalizeRespSource(rec.RespSource),
	}

	// 三段式：预截断 → 脱敏 → 终截断。
	//
	// 预截断的理由（P1-5）：Sanitize 是 7 条正则全量扫描，而异步任务的 multipart 提交
	// 会把文件转成 data: URL 塞进 JSON（单文件上限 10 MiB），body 能到 16 MiB。
	// 直接扫这么长的串会让 202 提交接口多花几十毫秒纯 CPU。
	//
	// 安全性：截断点**之后**的内容既不会被脱敏、也绝不会落库，
	// 所以不存在"未脱敏内容入库"；被切断的半个 token 不构成可用凭据；
	// 而 body 前部（api_key 字段、header 文本）必然落在 2×limits.Req 之内。
	// 字节数与指纹按**原始**长度算：它们回答的是"客户端发了多大 / 是不是同一份"，
	// 与留存口径无关（meta 模式下正文不进库，指纹仍可用来对齐两条记录）。
	row.ReqBytes = int64(len(rec.ReqRaw))
	sum := sha256.Sum256(rec.ReqRaw)
	row.ReqSHA256 = hex.EncodeToString(sum[:])
	if bodyMeta() {
		// meta 口径：正文只留结构摘要（模型/条数/尺寸/是否流式/图片数），不落用户内容。
		// 不置 ReqTruncated —— 那是"原文被切掉"，这里是"从来就没存"，
		// 混成一个标记会让排障时误判"再调大 LD_CALL_IO_MAX_KB 就能看到全文"。
		row.ReqRaw, _ = cutString(summarizeBody(rec.ReqRaw, "request"), limits.Req)
	} else {
		raw := rec.ReqRaw
		preCapped := false
		if preCap := 2 * limits.Req; len(raw) > preCap {
			raw = cutBytesAtRune(raw, preCap)
			preCapped = true
		}
		var cutAgain bool
		row.ReqRaw, cutAgain = cutString(Sanitize(raw), limits.Req)
		row.ReqTruncated = preCapped || cutAgain
	}

	if len(rec.RespError) > 0 {
		row.RespError, _ = cutString(Sanitize(rec.RespError), limits.Error)
	}
	// 响应体同样预截断（P1-10）：同一段 Sanitize 要跑 7 条正则，而图片 b64 响应
	// 单次可达 25 MB（4K × 4 张），整串扫一遍纯 CPU 就是几十毫秒，还全是白扫 ——
	// 超出的部分最终必然被 cutString 丢掉。截断点之后的内容既不脱敏也不落库，
	// 与请求侧同一口径。
	if len(rec.RespRaw) > 0 {
		row.RespRaw = cutResponseBody(rec.RespRaw, limits.Resp)
	}
	// 交付体与响应体同口径（同一段代码）：两份正文不会出现「一份脱敏一份没脱敏」。
	if len(rec.DeliveredRaw) > 0 {
		row.DeliveredRaw = cutResponseBody(rec.DeliveredRaw, limits.Resp)
	}
	row.RespBytes = int64(len(rec.RespRaw))
	if rec.RespBytesFor > 0 {
		row.RespBytes = rec.RespBytesFor
	}
	row.SSEFrames = rec.SSEFrames

	// 出站报文：独立列，脱敏/截断口径与入站一致。
	// 字节数与指纹按**原始**长度算（回答"我们到底发了多大/是不是同一份"）。
	if len(rec.UpstreamRaw) > 0 {
		row.UpstreamBytes = int64(len(rec.UpstreamRaw))
		usum := sha256.Sum256(rec.UpstreamRaw)
		row.UpstreamSHA256 = hex.EncodeToString(usum[:])
		if bodyMeta() {
			row.UpstreamRaw, _ = cutString(summarizeBody(rec.UpstreamRaw, "request"), limits.Req)
			row.UpstreamSource = "tap-meta"
		} else {
			var cut bool
			row.UpstreamRaw, cut = cutString(Sanitize(rec.UpstreamRaw), limits.Req)
			row.UpstreamTruncated = rec.UpstreamTruncated || cut
			row.UpstreamSource = "tap"
		}
	} else if rec.UpstreamAttempts > 0 {
		// 有尝试但没有 body：GetBody 为空（流式上传），不是"没发请求"。
		row.UpstreamSource = "tap-skipped"
	}
	row.UpstreamURL, _ = cutString([]byte(rec.UpstreamURL), 512)
	row.UpstreamStatus = rec.UpstreamStatus
	row.UpstreamAttempts = rec.UpstreamAttempts

	if len(rec.Media) > 0 {
		if raw, err := model.NewJSON(sanitizeMedia(rec.Media)); err == nil {
			row.Media = raw
		}
	}

	err := db.DB.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "usage_id"}},
			// 列清单由 Patch 决定：全量覆盖 / 只更新有值的列。
			DoUpdates: clause.AssignmentColumns(updatableColumns(rec)),
		}).Create(&row).Error
	if err != nil {
		logger.L().Warn("调用报文落库失败（不影响计费与响应）",
			zap.Uint64("usage_id", rec.UsageID), zap.Error(err))
		return false
	}
	return true
}

// updatableColumns 返回 ON CONFLICT 时要覆盖的列。
//
// 非 Patch（首次写入）：全部列，语义与 C1 完全一致。
// Patch：只含"本次真的有值"的列 + expires_at。
//
// 为什么 Patch 也要更新 expires_at：终态写入一定晚于提交写入，
// 把保留期从"提交时刻 + 7 天"推到"终态时刻 + 7 天"才符合"记录保留 7 天"的直觉；
// 反过来（不续期）会让长任务比短任务更早丢报文。
func updatableColumns(rec Rec) []string {
	if !rec.Patch {
		return []string{
			"req_raw", "req_bytes", "req_sha256", "req_truncated", "req_source",
			"resp_raw", "resp_error", "resp_bytes", "sse_frames", "delivered_raw",
			"media", "expires_at",
			"upstream_raw", "upstream_bytes", "upstream_sha256", "upstream_truncated",
			"upstream_source", "upstream_url", "upstream_status", "upstream_attempts",
			"body_mode", "resp_source",
		}
	}
	cols := []string{"expires_at"}
	if len(rec.ReqRaw) > 0 {
		cols = append(cols, "req_raw", "req_bytes", "req_sha256", "req_truncated")
	}
	if rec.ReqSource != "" {
		cols = append(cols, "req_source")
	}
	if len(rec.RespRaw) > 0 {
		cols = append(cols, "resp_raw", "resp_bytes")
	}
	// 交付体同样按有值加入：多数调用里它与 resp_raw 相同（空），
	// 无条件加列会把「提交时算出的交付体」在终态写回时清空。
	if len(rec.DeliveredRaw) > 0 {
		cols = append(cols, "delivered_raw")
	}
	if len(rec.RespError) > 0 {
		cols = append(cols, "resp_error")
	}
	if len(rec.Media) > 0 {
		cols = append(cols, "media")
	}
	// 出站报文必须**按有值**加入：失败释放路径只带 RespError，无条件加列会把它清零。
	if len(rec.UpstreamRaw) > 0 {
		cols = append(cols, "upstream_raw", "upstream_bytes", "upstream_sha256",
			"upstream_truncated", "upstream_source")
	} else if rec.UpstreamAttempts > 0 {
		cols = append(cols, "upstream_source")
	}
	if rec.UpstreamAttempts > 0 {
		cols = append(cols, "upstream_url", "upstream_status", "upstream_attempts")
	}
	if rec.BodyMode != "" {
		cols = append(cols, "body_mode")
	}
	if rec.RespSource != "" {
		cols = append(cols, "resp_source")
	}
	return cols
}

// normalizeRespSource 过滤响应来源标记（空/未知 → "none"）。
func normalizeRespSource(v string) string {
	switch strings.TrimSpace(v) {
	case "raw", "aggregate", "usage":
		return strings.TrimSpace(v)
	}
	return "none"
}

// normalizeReqSource 只放行三个已知取值；其它（含历史空串）一律落空串。
// 不做"猜一个最像的"——错标来源比不标更糟：排障时会照着错的假设去核对。
func normalizeReqSource(s string) string {
	switch s {
	case "raw", "rebuilt", "snapshot":
		return s
	}
	return ""
}

// maxMediaURLLen 是单个媒体链接的长度上限（上游签名链接通常 <400 字符，
// 超过 2 KB 只能是异常数据，存下来既没用又会把 media 撑大）。
const maxMediaURLLen = 2048

// sanitizeMedia 对媒体项做协议白名单与密钥抹除。
//
// 三件事，顺序无关但都不能省：
//  1. Note 过一遍 Sanitize（上游原文可能带 key）；
//  2. URL 只允许 http/https —— data:/blob:/javascript: 一律清空并说明，
//     否则前端把它塞进 <img src> 时，`data:` 会变成"看起来能预览但不能点"的假象；
//  3. 查询串里"值像平台密钥"的参数抹掉。
//
// 第 3 条的边界很重要：签名参数（X-Amz-Signature / sign / expires）是预览的必要条件，
// **必须保留**；只有 `sk-` / `ak-` / `Bearer ` / PEM 这类平台凭据才抹。
// 抹过密钥的链接必然失效，所以要同时改 Note 说明"不可预览"——
// 给一个点了 403 的图，比直接说"不可预览"更浪费时间。
func sanitizeMedia(items []model.MediaItem) []model.MediaItem {
	out := make([]model.MediaItem, 0, len(items))
	for _, it := range items {
		it.Note = string(Sanitize([]byte(it.Note)))
		if it.URL == "" {
			out = append(out, it)
			continue
		}
		if len(it.URL) > maxMediaURLLen {
			it.URL = ""
			it.Note = appendNote(it.Note, "链接过长（>2 KB），已丢弃")
			out = append(out, it)
			continue
		}
		u, err := neturl.Parse(it.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			it.URL = ""
			it.Note = appendNote(it.Note, "非 http 链接，不可预览")
			out = append(out, it)
			continue
		}
		if stripKeyQuery(u) {
			it.URL = u.String()
			it.Note = appendNote(it.Note, "链接含平台密钥已脱敏，不可预览")
		}
		out = append(out, it)
	}
	return out
}

// stripKeyQuery 抹掉查询串里形如平台密钥的值，返回是否改动过。
func stripKeyQuery(u *neturl.URL) bool {
	q := u.Query()
	if len(q) == 0 {
		return false
	}
	changed := false
	for k, vs := range q {
		for i, v := range vs {
			if looksLikeSecret(v) {
				q[k][i] = redacted
				changed = true
			}
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return changed
}

// looksLikeSecret 判断一个查询参数值是否像平台凭据。
func looksLikeSecret(v string) bool {
	s := strings.TrimSpace(v)
	low := strings.ToLower(s)
	return strings.HasPrefix(low, "sk-") || strings.HasPrefix(low, "ak-") ||
		strings.HasPrefix(low, "bearer ") || strings.Contains(s, "-----BEGIN")
}

// appendNote 追加说明（已有内容用分号隔开，保持可读）。
func appendNote(note, extra string) string {
	if note == "" {
		return extra
	}
	if strings.Contains(note, extra) {
		return note
	}
	return note + "；" + extra
}

// expiryFrom 由保留天数算过期时刻；0 天时给一个远期时间（表示"不自动删"）。
func expiryFrom(now time.Time) time.Time {
	days := RetentionDays()
	if days <= 0 {
		// 不删模式：仍要写一个非空值（列 NOT NULL）。
		// 用 100 年而不是 1970，避免"过期时间在过去"被误读成脏数据。
		return now.AddDate(100, 0, 0)
	}
	return now.AddDate(0, 0, days)
}

// cutString 按上限截断并返回字符串（调用方必须**先脱敏再调用**）；
// 第二个返回值表示是否发生截断。
//
// 截断点按 UTF-8 字符边界回退：在字符中间截断会产生非法字节序列，
// 前端 JSON.parse 直接失败 —— 而报文的第一用途就是给人看，宁可少几个字符。
func cutString(b []byte, limit int) (string, bool) {
	if limit <= 0 || len(b) <= limit {
		return string(b), false
	}
	end := limit
	for end > 0 && b[end]&0xC0 == 0x80 { // 落在续字节上 → 往前退
		end--
	}
	return string(b[:end]), true
}

// cutResponseBody 按响应侧口径处理一段正文：预截断 → 脱敏 → 终截断。
//
// 抽出来只有一个理由：RespRaw 与 DeliveredRaw 必须走**同一段**代码。
// 两份正文各写一遍的下场是一份按 meta 口径、另一份按 full 口径，
// 门户与管理台对同一次调用显示两种颗粒度。
func cutResponseBody(raw []byte, limit int) string {
	if bodyMeta() {
		s, _ := cutString(summarizeBody(raw, "response"), limit)
		return s
	}
	if preCap := 2 * limit; len(raw) > preCap {
		raw = cutBytesAtRune(raw, preCap)
	}
	s, _ := cutString(Sanitize(raw), limit)
	return s
}

// cutBytesAtRune 按上限切字节，并把切点回退到 rune 边界（不产生非法 UTF-8）。
//
// 与 cutString 的区别：它不做脱敏、不返回字符串，只负责"把超长输入变短"，
// 供 Store 在脱敏**之前**预截断使用（见 Store 里的三段式说明）。
func cutBytesAtRune(b []byte, limit int) []byte {
	if limit <= 0 || len(b) <= limit {
		return b
	}
	end := limit
	for end > 0 && b[end]&0xC0 == 0x80 { // 落在续字节上 → 往前退
		end--
	}
	return b[:end]
}

/* ------------------------------ 清理 ------------------------------ */

// Purge 删除已过期的报文（每 6h 一轮，单轮 limit 条）。
//
// 没有"先删对象再删行"的排序约束 —— 这正是把报文放 PG 而不是对象存储的核心收益：
// r2 方案里最脆的一环（对象删不掉就得保留行）在这里根本不存在。
func Purge(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 2000
	}
	if RetentionDays() <= 0 {
		// 只报告模式：统计过期存量但不删。
		var n int64
		if err := db.DB.WithContext(ctx).Model(&model.CallIO{}).
			Where("expires_at < ?", time.Now()).Count(&n).Error; err != nil {
			return 0, err
		}
		if n > 0 {
			logger.L().Info("调用报文保留策略未开启（LD_CALL_IO_RETENTION_DAYS=0），仅报告",
				zap.Int64("expired_rows", n))
		}
		return 0, nil
	}
	res := db.DB.WithContext(ctx).Exec(
		`DELETE FROM call_io WHERE id IN (SELECT id FROM call_io WHERE expires_at < ? ORDER BY id LIMIT ?)`,
		time.Now(), limit)
	return res.RowsAffected, res.Error
}

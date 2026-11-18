package model

import "time"

// CallIO 对应 call_io 表：调用记录的原始报文与媒体链接（C1 详情层）。
//
// 设计取舍（与 r2 方案的关键差别）：**不引入对象存储**。
//   - 报文直接进 PG text，超过 ~2KB 由 TOAST 自动压缩外置，实测比"每条一个对象"省掉
//     key 管理、生命周期、跨后端差异、孤儿清理四类问题；
//   - 媒体只存上游给的链接与元数据，**不下载、不转存、不生成缩略图** ——
//     下载会引入生命周期与孤儿风险，且用户看到的本来就是上游产物；
//   - 清理只有一条 `DELETE FROM call_io WHERE expires_at < now()`，
//     没有"先删对象再删行"的排序约束（那条约束是 r2 里最脆的地方）。
//
// 与 usages 的关系：一对零或一。归档（usages → usages_archive）**不搬** call_io：
// usages.id 在归档时不变，详情按 id 反查即可；过期后自然删除，不会留下归档孤儿。
type CallIO struct {
	ID uint64 `gorm:"primaryKey"`
	// UsageID 唯一：一条调用只有一份报文。重复写入走 ON CONFLICT 覆盖（见 service/callio）。
	UsageID uint64 `gorm:"uniqueIndex;not null"`
	// UserID 同时承担越权校验与清理索引（门户详情必须 WHERE user_id = ?）。
	UserID uint64 `gorm:"index;not null"`

	// ReqRaw 是**原始请求体文本**（脱敏后原文，不做结构重排）。
	// 上限由 LD_CALL_IO_MAX_KB 控制（默认 256 KiB），超限按「先脱敏后截断」保留前缀。
	ReqRaw       string `gorm:"type:text"`
	ReqBytes     int64  `gorm:"not null;default:0"` // 截断前的原始字节数
	ReqSHA256    string `gorm:"size:64;not null;default:''"`
	ReqTruncated bool   `gorm:"not null;default:false"`

	// RespRaw 是成功响应体原文（≤128 KiB）。二进制（TTS 音频）不存内容，只记元数据。
	RespRaw string `gorm:"type:text"`
	// DeliveredRaw 是**平台实际返回给客户端**的响应体（脱敏后，≤128 KiB）。
	//
	// 为什么不复用 RespRaw：它采集在媒体直链改写（上游 CDN → 本站签名地址）与
	// 协议转码（/v1/messages ↔ chat）**之前**，里面是服务商地址与上游协议形状。
	// 门户必须看到自己真正收到的那一份；管理台仍看 RespRaw（排障要知道图是谁给的）。
	//
	// 与 RespRaw 逐字节相同时**不写这一列**（普通透传占绝大多数，省一半空间），
	// 读取侧按「空 = 同 RespRaw」处理。失败调用没有 RespRaw，这一列就是客户端收到的错误体。
	DeliveredRaw string `gorm:"type:text"`
	// ReqSource 说明 ReqRaw 的来源。空值表示 C1 之前的历史行，读取侧按 call_kind 推断。
	//
	//	raw      客户端原始 JSON body
	//	rebuilt  multipart 被平台重构成 JSON（文件已转 data:，且已脱敏）
	//	snapshot 只有 tasks.request_params 快照（字段顺序与未知字段已丢失）
	//
	// 为什么要单独一列：异步任务的"原始请求"有三种可信度完全不同的来源，
	// 混成一个 req_raw 会让人误以为"我发的就是这个"，排障方向直接走偏。
	ReqSource string `gorm:"size:16;not null;default:''"`
	// RespError 是**上游错误原文**（≤16 KiB，脱敏）。列表里的 error_msg 仍是 500 字符短文本，
	// 二者分工不同：短文本用于扫列表，原文用于排障时看清上游到底说了什么。
	RespError string `gorm:"type:text"`
	RespBytes int64  `gorm:"not null;default:0"`
	SSEFrames int    `gorm:"not null;default:0"`

	// Media 是产物/媒体清单（只存链接与元数据），形状见 MediaItem。
	// 空数组与 NULL 都表示"无媒体"，读取侧统一按空切片处理。
	Media JSON `gorm:"type:jsonb"`

	// ---------- 实际出站报文（2026-11 新增）----------
	//
	// ReqRaw 是**客户端发进来**的；下面这组是**平台重建后真正发给上游**的。
	// 只留前者时，「字段名对不对、端点打到哪」无从判断 —— 2026-11-04 的多米故障
	// 就是排障时看到入站摘要、误以为下游返回异常。
	UpstreamRaw       string `gorm:"type:text"`
	UpstreamBytes     int64  `gorm:"not null;default:0"`
	UpstreamSHA256    string `gorm:"size:64;not null;default:''"`
	UpstreamTruncated bool   `gorm:"not null;default:false"`
	// UpstreamSource：tap / tap-meta / tap-skipped（GetBody 为空）/ snapshot。
	UpstreamSource string `gorm:"size:16;not null;default:''"`
	// UpstreamURL 是实际请求的完整 URL（含 path override 与 ?async=true）。
	UpstreamURL string `gorm:"size:512;not null;default:''"`
	// UpstreamStatus 是上游 HTTP 状态码；0 = 未收到响应（连接失败/超时）。
	UpstreamStatus int `gorm:"not null;default:0"`
	// UpstreamAttempts 是 failover 实际出网次数（报文记最后一次）。
	UpstreamAttempts int `gorm:"not null;default:0"`

	// BodyMode 是本行两侧正文的口径：full / meta。
	// 前端据此决定是否提示「这是摘要」——不再靠猜 JSON 里的 _meta。
	BodyMode string `gorm:"size:8;not null;default:''"`
	// RespSource：raw / aggregate（流式拼接）/ usage（任务兜底）/ none。
	RespSource string `gorm:"size:16;not null;default:''"`

	// ExpiresAt 由写入时的保留天数计算，清理任务只看这一列（不 join usages）。
	ExpiresAt time.Time `gorm:"type:timestamptz;index;not null"`
	CreatedAt time.Time `gorm:"type:timestamptz"`
}

// TableName 固定表名（GORM 默认会拼成 call_ios）。
func (CallIO) TableName() string { return "call_io" }

// MediaItem 是 call_io.media 的元素。
//
// 全部字段都可零值：一条记录可能只知道"有 1 张图但内容没留存"（上游回 b64_json），
// 此时 URL 为空、Note 说明原因 —— 这比"什么都不存"更有利于事后判断是否丢了产物。
type MediaItem struct {
	Kind string `json:"kind"` // image | audio | video | text
	MIME string `json:"mime,omitempty"`
	// Bytes 是内联数据的字节数（0 = 上游直接给链接，未知大小）。
	Bytes int64 `json:"bytes,omitempty"`
	// URL 只允许 http/https（上游给的产物链接）；内联数据留空并在 Note 说明。
	URL string `json:"url,omitempty"`
	// ExpiresHint 是上游声明的链接有效期（秒，0 = 未声明）。只作展示提示，不做校验。
	ExpiresHint int64 `json:"expires_hint,omitempty"`
	// ArtifactIndex 是异步任务的产物序号（-1 = 非任务产物），前端据此走既有产物端点预览。
	ArtifactIndex int `json:"artifact_index"`
	// Note 是给人看的补充说明（如"上游返回内联数据，未留存"）。
	Note string `json:"note,omitempty"`
}

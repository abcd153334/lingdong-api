package model

import "time"

// Artifact 是对外产物条目的**唯一**定义。
//
// 三处响应（/api/{portal,admin,user}/tasks/{id} 的 contents、调用详情的 task.contents）
// 曾经各有一个结构体：task.ArtifactEntry 没有 kind，logs.ArtifactRef 有 kind，
// 开放 API 又在 handler 里手工拼 gin.H。同一份产物在三个面显示成不同形状，
// 前端只能写三套映射，且「调用记录里是文件、任务页里是图片」这类漂移无法被测试发现。
//
// 这里只放对外字段：不带存储路径、不带上游签名链接（那两项永远不下发）。
// Backend/Bucket/Key 是**进程内**取地址用的（json:"-"），序列化时不会泄漏存储位置。
type Artifact struct {
	// Index 是对外序号：0 起、稠密编号（失败与过期条目都不占号），
	// 与控制面 `/contents/{index}`、协议面 `/artifacts/{index}/content` 的取数顺序严格一致。
	Index       int    `json:"index"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
	// Kind 是渲染档：image / video / audio / text / file。
	// 由 MediaKind 按「MIME → 扩展名」派生；前端不再各写一份前缀判断。
	Kind string `json:"kind"`

	// Backend/Bucket/Key 是该产物写入时的位置（后端 + 桶 + key），仅供
	// storage.URLOr 派生对外地址。
	//
	// 必须是 json:"-"：这个结构是三处响应的唯一对外定义，漏了就等于把存储路径发出去。
	// Bucket 必须一起带上：换桶之后老产物仍在旧桶，只带 backend 会把直链拼到新桶上（404）。
	Backend string `json:"-"`
	Bucket  string `json:"-"`
	Key     string `json:"-"`
}

// Readable 过滤出当前可读的产物行（**唯一**过滤点）。
//
// 判据只有 File.ReadableAt 一处；清单、取数、调用记录都经这里，
// 避免「清单跳过过期、取数没跳过」造成的序号错位。
func Readable(rows []File, now time.Time) []File {
	out := make([]File, 0, len(rows))
	for i := range rows {
		if rows[i].ReadableAt(now) {
			out = append(out, rows[i])
		}
	}
	return out
}

// ArtifactListing 把 files 行归一成对外产物清单。
//
// 规则只在这一个函数里实现（task 与 logs 都调它）：
//  1. 先经 Readable 去掉已过期的行 —— 过期文件仍留在库里等清理，但对外不可读；
//  2. 序号用 len(out) 稠密重编号 —— 库里的 artifact_index 会因失败条目留空洞（0,2），
//     而取数端点按同一份可读集合索引，两者不一致会「清单给了 2 个、点第 2 个 410」；
//  3. MIME 走 EffectiveContentType —— application/octet-stream 的历史行按扩展名补全，
//     否则 kind 会退化成 file，同一产物在任务页是图片、在调用记录里是文件。
func ArtifactListing(rows []File, now time.Time) []Artifact {
	readable := Readable(rows, now)
	if len(readable) == 0 {
		return nil
	}
	out := make([]Artifact, 0, len(readable))
	for i := range readable {
		ct := readable[i].EffectiveContentType()
		out = append(out, Artifact{
			Index:       len(out),
			ContentType: ct,
			Bytes:       readable[i].SizeBytes,
			Kind:        ArtifactKind(ct),
			Backend:     readable[i].StorageBackend,
			Bucket:      readable[i].StorageBucket,
			Key:         readable[i].StorageKey,
		})
	}
	return out
}

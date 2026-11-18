package model

import "time"

// Icon 对应 icons 表：运营上传的图标图片元数据。
//
// 为什么单独一张表、而不是复用 uploads（开发者临时文件）：
//   - uploads 有 24h 过期（upload.PurgeExpired 会物理删对象），而图标是被
//     models/vendors/channels 引用的**配置数据**，过期即等于「所有用了它的实体
//     图标变空白」，且没有任何告警；
//   - uploads 按 (user_id, key_id) 归属，图标是全局配置，没有归属语义；
//   - uploads 的白名单含 gif/bmp 与音视频，图标只需要 png/jpeg/webp。
//
// 刻意**没有** deleted_at：图标不提供删除入口（选了就不许删，避免「删了之后
// 引用它的实体静默退回占位图而运营查不出原因」）。真要清理走运维脚本。
//
// SHA256 是去重键：同一张图被两个管理员先后上传时只留一行，
// 唯一索引 uq_icons_sha256（db/indexes.go）是并发下的最终判据。
type Icon struct {
	ID  string `gorm:"size:26;primaryKey"`
	Ext string `gorm:"size:8;not null"`
	// MIME 是落库时白名单化的 content-type（image/png | image/jpeg | image/webp）。
	// 读路径直接用它回 Content-Type，不再重新嗅探（嗅探要给每个请求读头部，
	// 而这张图的内容在写入时就已定型、之后不会变）。
	MIME string `gorm:"column:mime;size:64;not null"`
	// Size 是对象字节数（读路径用来回 Content-Length）。
	Size int64 `gorm:"not null"`
	// SHA256 是 64 位小写 hex，同时充当去重键与 ETag 的来源。
	SHA256 string `gorm:"column:sha256;size:64;not null"`
	// StorageBackend 是**写入时**激活的后端标识；读取必须按它路由。
	//
	// 图标是被配置长期引用的对象，切换存储后端之后它仍在旧后端上：
	// 按 active 去找的症状是「老图标全裂、新图标正常」，而库里每行都完好。
	StorageBackend string `gorm:"size:16;not null;default:''"`
	// StorageBucket 与 files/uploads 同形：换桶之后图标仍留在旧桶里，
	// 只按 backend 路由会让所有老图标同时 404（而库里每行都完好）。
	StorageBucket string `gorm:"size:128;not null;default:''"`
	// CreatedBy 是上传者管理员 user_id（仅审计用，不参与权限判定）。
	CreatedBy uint64    `gorm:"index"`
	CreatedAt time.Time `gorm:"type:timestamptz;not null"`
}

// TableName 显式指定表名（不依赖 GORM 复数化规则）。
func (Icon) TableName() string { return "icons" }

// ObjectKey 返回该图标在对象存储里的 key。
//
// 唯一实现：写入（service/icon.Save）与读取（service/icon.Open）必须用同一个 key，
// 两边各拼一次就可能出现「写进 icons/x.png、读 icons/x.jpg」这种 404，
// 而库里有行、接口 200，只有浏览器图裂。
func (i Icon) ObjectKey() string { return "icons/" + i.ID + "." + i.Ext }

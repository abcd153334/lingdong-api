// Package brandicon 是「实体图标」的**唯一**实现：内置图库、取值契约、展示值派生。
//
// 为什么图标要打进二进制（而不是放前端 public/）：
// 同一个图标要被管理台、门户、对外文档站三方消费，且「哪些 key 合法」必须由
// 写库方（后端）校验。放前端等于让后端去猜 key 是否存在，或者维护第二份清单。
//
// 代价要认：72 个 PNG 共约 378KB 进二进制（当前 34.8MB，+1.1%）。
// 后续若要加图，先算体积、再看是否真的需要 —— 不要把它当成随手的素材目录。
// 与 internal/controller/swaggerui 同一取舍（那边是 1.9MB）。
package brandicon

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
)

//go:embed icons/*.png
var assets embed.FS

// Group 是选择器里的分组（仅用于展示排序，不影响取值合法性）。
type Group string

const (
	// GroupLLM 对话 / 推理 / 通用大模型厂商。
	GroupLLM Group = "llm"
	// GroupMedia 图像 / 视频 / 音频模型。
	GroupMedia Group = "media"
	// GroupPlatform 云平台 / 推理服务 / 本地运行工具。
	GroupPlatform Group = "platform"
)

// Item 是一条内置图标。
type Item struct {
	Key string `json:"key"`
	// Name 是选择器里显示的名字（英文品牌名，避免中英混排错位）。
	Name    string   `json:"name"`
	Group   Group    `json:"group"`
	Aliases []string `json:"aliases"`
}

var (
	once    sync.Once
	byKey   map[string]Item
	files   map[string][]byte
	etagOf  map[string]string
	verOf   map[string]string
	keyList []string
	initErr error
)

// load 一次性把 catalog 与内嵌文件对齐。
//
// 对齐失败（catalog 里有 key 但没文件，或反之）**不 panic**：
// 生产环境不该因为一个漏提交的 png 起不来。缺文件时该 key 从 List 里剔除，
// 并可由 embed_test 在 CI 阶段抓住（go:embed 缺文件编译照样过，这是唯一防线）。
func load() {
	byKey = make(map[string]Item, len(catalog))
	files = make(map[string][]byte, len(catalog))
	etagOf = make(map[string]string, len(catalog))
	verOf = make(map[string]string, len(catalog))
	keyList = make([]string, 0, len(catalog))

	for _, it := range catalog {
		raw, err := assets.ReadFile("icons/" + it.Key + ".png")
		if err != nil {
			if initErr == nil {
				initErr = fmt.Errorf("brandicon: 内置图标缺失 %s.png: %w", it.Key, err)
			}
			continue
		}
		sum := sha256.Sum256(raw)
		byKey[it.Key] = it
		files[it.Key] = raw
		etagOf[it.Key] = `"` + hex.EncodeToString(sum[:16]) + `"`
		verOf[it.Key] = hex.EncodeToString(sum[:4])
		keyList = append(keyList, it.Key)
	}
	sort.Strings(keyList)
}

func ensure() {
	once.Do(load)
}

// InitError 报告内嵌资源与 catalog 的不一致（缺失文件）。
//
// 启动期由 controller 记一条 ERROR 日志，而不是 fatal：缺一张图不该让整站不可用。
func InitError() error {
	ensure()
	return initErr
}

// List 返回全部内置图标（按 key 升序）。
//
// 返回切片是**副本**：调用方（管理台列表接口）会拼 URL 后 append，共享底层数组会串数据。
func List() []Item {
	ensure()
	out := make([]Item, 0, len(keyList))
	for _, k := range keyList {
		it := byKey[k]
		// Aliases 也要复制：JSON 序列化是只读的，但调用方可能 sort/append。
		if it.Aliases != nil {
			it.Aliases = append([]string(nil), it.Aliases...)
		}
		out = append(out, it)
	}
	return out
}

// Keys 返回全部合法 key（升序）。
func Keys() []string {
	ensure()
	return append([]string(nil), keyList...)
}

// Lookup 查一条内置图标。
func Lookup(key string) (Item, bool) {
	ensure()
	it, ok := byKey[key]
	return it, ok
}

// Asset 返回图标字节与预计算的 ETag。
//
// ETag 在 init 里算好：图标是只读内嵌资源，无需每次请求重算（与 writeETag 的
// 通用实现相比，这里只是省掉重复哈希，缓存语义完全一致）。
func Asset(key string) (raw []byte, etag string, ok bool) {
	ensure()
	raw, ok = files[key]
	if !ok {
		return nil, "", false
	}
	return raw, etagOf[key], true
}

// AssetVersion 返回某个内置图标的内容指纹前缀（8 hex）；key 不在库中返回 ""。
//
// 唯一的用途是拼进图标地址的 `?v=`。内置图的 key 稳定、内容却会随后续版本被替换，
// 而图片响应带一年强缓存 —— 不带版本时"换了 PNG，用户一年都看不到新的"，
// 且 ETag 在 max-age 内根本不参与协商。
//
// **按 key 取版本，不用全局单值**：全局版本会让任意一张图变动导致全部内置 URL
// 同时失效，所有客户端一起回源，撞 controller 的每分钟限流。
//
// 返回 "" 同时表示"这个 key 既不在 catalog、也没有文件"，调用方据此不产出死链。
func AssetVersion(key string) string {
	ensure()
	return verOf[key]
}

// Count 返回可用内置图标数（供启动日志与健康检查）。
func Count() int {
	ensure()
	return len(keyList)
}

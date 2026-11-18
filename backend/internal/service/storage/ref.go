package storage

import (
	"context"
	"io"
)

// Ref 定位一个已写入的对象：后端标识 + 后端内的桶 + 桶内的 key。
//
// 它是「一个归属」约定的载体：写路径产出 Ref 并原样落库（三列），
// 读/删路径只接受 Ref，不读当前配置里的 active —— 也不读当前配置里的 bucket。
//
// 为什么必须带 Bucket：运营把 `channels.oss.bucket` 从 A 改成 B 之后，
// 历史对象仍在 A；只按 Backend 路由会去 B 删 A 的 key，得到 404，
// 而「404 = 已不存在」的旧契约会让我们把 DB 行也删掉 —— 对象从此再无线索。
type Ref struct {
	Backend string
	Bucket  string
	Key     string
}

// PutActive 写入当前激活后端，返回可落库的 Ref。
//
// size 是唯一长度真相：<= 0 直接拒绝，不尝试"边读边数"。
// 理由见包注释第 3 条 —— OSS 在长度探测失败时会把 Content-Length 覆盖成 0，
// 结果是 200 + 0 字节对象，账单照扣。
func PutActive(ctx context.Context, key string, r io.Reader, size int64, contentType string) (Ref, error) {
	if size <= 0 {
		return Ref{}, ErrNeedLength
	}
	st, err := Active()
	if err != nil {
		return Ref{}, err
	}
	if err := st.Put(ctx, key, r, size, contentType); err != nil {
		return Ref{}, err
	}
	return Ref{Backend: st.Name(), Bucket: st.Bucket(), Key: key}, nil
}

// OpenRef 按记录路由读取对象。
func OpenRef(ctx context.Context, ref Ref) (io.ReadCloser, int64, error) {
	st, err := ForRef(ref)
	if err != nil {
		return nil, 0, err
	}
	return st.Open(ctx, ref.Key)
}

// OpenRangeRef 按记录路由区间读取；后端不支持区间时返回 ErrRangeUnsupported。
func OpenRangeRef(ctx context.Context, ref Ref, offset, length int64) (io.ReadCloser, error) {
	st, err := ForRef(ref)
	if err != nil {
		return nil, err
	}
	ro, ok := st.(RangeOpener)
	if !ok {
		return nil, ErrRangeUnsupported
	}
	return ro.OpenRange(ctx, ref.Key, offset, length)
}

// URLOr 返回对象的对外地址：云后端用该渠道的公开地址，本地/后端不可用时用 fallback。
//
// 这是**所有**对外地址出口调用的唯一函数（上传件、任务产物、参考图）。
// fallback 由调用方按资源类型传入（`/f/{id}/{name}`、`/api/*/tasks/{id}/contents/{i}`），
// 因为"平台自己的地址"对不同资源本来就不同；但"云地址优先"这条判断只有一份。
func URLOr(ref Ref, fallback string) string {
	if u, ok := PublicURL(ref); ok {
		return u
	}
	return fallback
}

// PublicURL 按记录路由返回对象的公开地址。
//
// 返回 ok=false 有两种合法情形：本地后端没有对外地址；记录的后端已从配置里消失
// （此时按 ForRef 的语义，读路径本来就会得到 503 storage_backend_missing）。
// 因此调用方拿到 false 时应回落到**平台自己的地址**，而不是把它当错误往上抛 ——
// 平台地址在配置缺失时会自然暴露成 503，语义一致，不需要第二套判断。
//
// 不发网络请求、不校验对象是否存在：地址是纯拼装，可达性由桶策略决定
// （公开读在配置阶段由 ProbeChannel 校验）。
func PublicURL(ref Ref) (string, bool) {
	st, err := ForRef(ref)
	if err != nil {
		return "", false
	}
	u, ok := st.(PublicURLer)
	if !ok {
		return "", false
	}
	return u.PublicURL(ref.Key)
}

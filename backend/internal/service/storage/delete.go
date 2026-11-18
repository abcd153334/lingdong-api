package storage

import (
	"context"
	"errors"
	"fmt"
)

// DeleteOutcome 是「删除 + 复核」的结论。
//
// 只有 Gone 允许调用方删记录行：Unverified 意味着对象可能还在，
// 删掉行就等于放弃对象的唯一定位线索（这正是本次整改要消灭的形态）。
type DeleteOutcome string

const (
	// DeleteGone 表示复核确认对象已不存在（本次删掉或本来就没有）。
	DeleteGone DeleteOutcome = "gone"
	// DeleteUnverified 表示无法确认删除是否生效（对象仍在 / 复核失败 / 桶不可用）。
	DeleteUnverified DeleteOutcome = "unverified"
)

// DeleteRefVerified 按记录删除对象，并用 Head 复核结果。
//
// 与旧 DeleteRef 的区别只有一条，但它是本次整改的核心：
// **Delete 的返回码不参与判定**。云存储的 404 既可能是「已经不存在」，
// 也可能是「删错了桶」「网关/CDN 拦截」「桶名写错」—— 后者按旧契约会被当成成功，
// 结果是 DB 行被删、对象永久留在旧桶且再无任何线索。
//
// 顺序固定为 CheckBucket → Delete → Head：
//   - CheckBucket 先排除「桶不存在 / 版本控制」这两种「看起来删掉了」的形态；
//   - Delete 的错误被刻意忽略（对象可能本来就不在，或请求已生效但响应丢失）；
//   - Head 是唯一判据：404 → Gone，其余（含 200/403/网络错误）→ Unverified。
//
// 返回的 error 恒与 outcome 语义一致：Gone 时 err == nil，Unverified 时 err != nil
// （包 ErrDeleteUnverified，便于调用方与告警统一分类）。
func DeleteRefVerified(ctx context.Context, ref Ref) (DeleteOutcome, error) {
	st, err := ForRef(ref)
	if err != nil {
		return DeleteUnverified, err
	}
	state, err := st.CheckBucket(ctx)
	if err != nil {
		return DeleteUnverified, err
	}
	if !state.Exists {
		return DeleteUnverified, fmt.Errorf("%w: 桶 %s 不存在", ErrBackendGone, quoteName(ref.Bucket))
	}
	if state.Versioned {
		// 版本控制下 Delete 只产生 delete marker：Head 看不到旧版本，但数据与账单都还在。
		// 这一档必须显式拦住，否则「复核通过」本身就是假的。
		return DeleteUnverified, fmt.Errorf("%w: 桶 %s 开启版本控制，删除只产生删除标记",
			ErrDeleteUnverified, quoteName(ref.Bucket))
	}

	// 删除错误刻意不参与判定：见函数注释。
	delErr := st.Delete(ctx, ref.Key)

	if _, herr := st.Head(ctx, ref.Key); herr == nil {
		if delErr != nil {
			return DeleteUnverified, fmt.Errorf("%w: %s/%s/%s（删除请求失败: %v）",
				ErrDeleteUnverified, ref.Backend, ref.Bucket, ref.Key, delErr)
		}
		return DeleteUnverified, fmt.Errorf("%w: %s/%s/%s 仍然存在",
			ErrDeleteUnverified, ref.Backend, ref.Bucket, ref.Key)
	} else if !errors.Is(herr, ErrObjectNotFound) {
		return DeleteUnverified, fmt.Errorf("%w: 复核失败: %v", ErrDeleteUnverified, herr)
	}
	return DeleteGone, nil
}

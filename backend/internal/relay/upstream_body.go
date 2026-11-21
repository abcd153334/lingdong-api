package relay

import (
	"fmt"
	"io"
)

// ReadUpstreamBody 读取上游响应体，最多 limit 字节；**超限报错而不是静默截断**。
//
// 这是全仓唯一的上游读体实现。此前 8 处各写一份，其中 5 处是
// `io.ReadAll(io.LimitReader(body, limit))` —— 超限时把响应**截断**后当正常体返回，
// 于是 JSON 解析失败、错误被归因成「上游返回了坏数据」，排查方向从一开始就错了。
//
// 读 limit+1 而不是 limit：只有多读一个字节才知道"刚好等于上限"与"已超限"的区别。
//
// limit <= 0 时回落到 JSON 上限，避免调用方漏传变成无限读（内存打爆）。
func ReadUpstreamBody(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = MaxUpstreamBodyBytes
	}
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, fmt.Errorf("%w（上限 %d 字节）", ErrUpstreamBodyTooLarge, limit)
	}
	return buf, nil
}

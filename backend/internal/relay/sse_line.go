package relay

import (
	"bufio"
	"errors"
)

// MaxSSELineBytes 是单帧（单行）长度上限。
//
// 与 MaxUpstreamBodyBytes 同值（8 MiB）：一个 b64 内联图可以合法地很大，
// 设小了会把正常帧误杀成「上游故障」。超过这个量的单行只可能是恶意/故障体。
const MaxSSELineBytes = 8 << 20

// ErrSSELineTooLong 表示上游 SSE 单帧超过 MaxSSELineBytes。
//
// 必须是可识别的哨兵：调用方据此判**上游故障**（502 + 渠道健康度），
// 不能判成客户端断开（499），否则会退款且把责任记到用户头上。
var ErrSSELineTooLong = errors.New("sse: 单帧长度超过上限")

// ReadLineLimited 读一行（包含结尾的 '\n'），累计超过 max 返回 ErrSSELineTooLong。
//
// 为什么不用 ReadBytes('\n')：它在超长行上会先把**整行**读进内存才返回，
// 一个不换行的 100 GiB 响应体足以把进程打爆。ReadSlice 在内部缓冲（4 KiB）
// 上滑动，超限时我们手里的累计量最多 max + 一块缓冲。
//
// 返回形态与 ReadBytes('\n') 一致（含 '\n'）：读到 EOF 但仍有残余数据时返回 (残余, nil)，
// 下一次调用才返回 (nil, io.EOF)。这样调用方的循环结构不用改。
func ReadLineLimited(br *bufio.Reader, max int) ([]byte, error) {
	if max <= 0 {
		max = MaxSSELineBytes
	}
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(frag) > 0 {
			if len(line)+len(frag) > max {
				return nil, ErrSSELineTooLong
			}
			line = append(line, frag...)
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			// 一行比 bufio 的内部缓冲长：正常现象（大帧），继续累加。
			continue
		default:
			if len(line) > 0 {
				// 与 ReadBytes 同语义：先交出已有数据，EOF 留给下一次。
				return line, nil
			}
			return nil, err
		}
	}
}

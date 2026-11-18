//go:build !windows

package disk

import "syscall"

// Usage 用 Statfs 读取文件系统容量（Linux / macOS）。
func Usage(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// Bsize / Bavail 的类型在不同平台不一致（Linux 是 int64/uint64，macOS 是 uint32），
	// 统一转 uint64 再乘，避免在 32 位平台上静默溢出。
	total := uint64(st.Blocks) * uint64(st.Bsize)
	avail := uint64(st.Bavail) * uint64(st.Bsize)
	return total, avail, nil
}

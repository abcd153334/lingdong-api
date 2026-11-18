//go:build windows

package disk

import "golang.org/x/sys/windows"

// Usage 用 GetDiskFreeSpaceEx 读取盘符容量（Windows）。
//
// Go 标准库的 syscall 包**没有**导出这个 API（只有 cmd/vendor 里的内部副本），
// 所以这里用已在 go.mod 中的 golang.org/x/sys/windows —— 它是本仓库既有的间接依赖，
// 不引入新的下载项，离线构建不受影响。
func Usage(path string) (uint64, uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &totalFree); err != nil {
		return 0, 0, err
	}
	return total, freeToCaller, nil
}

// Package disk 提供跨平台的磁盘容量查询（B7 §2.10 容量监控）。
//
// 为什么单独成包：`syscall.Statfs` 在 Windows 上根本不存在，而
// `syscall.GetDiskFreeSpaceEx` 只存在于 Windows。写在一个文件里必然编译不过，
// 用 `os/exec` 调 df 又会在最小化镜像里缺命令。build tag 拆两文件是唯一稳的做法。
//
// 两个实现的语义必须一致（改一处必须同步另一处）：
//
//	Usage(path) 返回 (总容量, 非特权用户可用容量)，单位字节。
//	avail 取的是 Bavail / freeBytesAvailableToCaller，而不是 Free / totalFree：
//	ext4 默认给 root 预留 5%，用 Free 会在「快要写满」时高估可用空间 ——
//	恰好是告警最不该失准的时刻。
package disk

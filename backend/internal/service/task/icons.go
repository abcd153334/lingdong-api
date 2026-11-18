package task

import (
	"context"

	"lingdong-api/internal/service/taskview"
)

// 本文件只做一件事：把 taskview.FillModelIcons 暴露在 task 包下。
//
// 实现与判据在 service/taskview（唯一一份）；controller 只需要 import task，
// 不必为了填图标再认识一个包。
func FillModelIcons[T any, PT interface {
	*T
	taskview.IconTarget
}](ctx context.Context, items []T, base string) {
	taskview.FillModelIcons[T, PT](ctx, items, base)
}

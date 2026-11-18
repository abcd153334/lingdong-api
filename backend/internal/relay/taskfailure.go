package relay

import (
	"fmt"
	"strings"
)

// 内容审核相关的关键词表：异步任务失败后，上游只会给一句自然语言原因，
// 平台必须自己判断"这是调用方的问题（违规 prompt/文案）还是上游的问题"。
//
// 单份实现的原因：同步桥接（规格 bridge）与内置同步适配器（duomi）原本各有一份，
// 词表一旦只更新一处，同一条违规文案在两条链路上会被分成 400 和 502 ——
// 前者不记渠道健康度、后者会把一条健康渠道连续打熔断。
var moderationKeywords = []string{
	"审核", "违规", "敏感", "涉黄", "涉政",
	"policy", "violat", "moderation", "nsfw", "blocked",
}

// IsModerationRejection 判断一段上游文案是否属于「内容审核/违规拒绝」。
//
// 抽成导出函数的唯一理由：任务失败（本文件）与**同步图片响应**（image_verdict.go）
// 必须共用同一份词表。分成两份的后果是同一句「内容违规」在任务链路算调用方错（400、
// 不记渠道健康），在同步图片链路算上游故障（502、把健康渠道打熔断）——
// 而两处看到的其实是上游同一个拦截动作。
//
// 大小写不敏感：上游英文文案有 "Blocked" / "blocked" / "BLOCKED" 三种写法。
func IsModerationRejection(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	for _, k := range moderationKeywords {
		if strings.Contains(lower, k) || strings.Contains(trimmed, k) {
			return true
		}
	}
	return false
}

// TaskFailure 把上游任务失败文案折成带归因的错误。
//
//   - 内容审核/违规拒绝 → CallerParamError（400，不记渠道健康度）；
//   - 其余 → 普通错误（调用方按上游故障处理，502 并记渠道）。
//
// taskID 必须出现在文案里：这条记录会写进 usages.error_msg，没有任务号
// 运维既补不了产物，也查不出上游那边到底发生了什么。
func TaskFailure(msg, taskID string) error {
	text := strings.TrimSpace(msg)
	if text == "" {
		text = "上游任务失败（未返回原因）"
	}
	if IsModerationRejection(text) {
		return &CallerParamError{Msg: "内容审核未通过：" + text}
	}
	return fmt.Errorf("上游任务 %s 失败：%s", taskID, text)
}

package agent

import (
	"strings"

	"lingdong-api/internal/service/pricing"
)

// ResolveChatModel 解析"这一轮用哪个对话模型"。
//
// 优先级：设置里显式指定的 → 系统派生的默认对话模型（pricing.Get().Defaults["chat"]）。
//
// 为什么不用 models.is_default 判定：那只是一次种子搬迁的来源，
// 系统真正的默认值由 deriveDefaults 按「configured 优先、否则首个启用模型」派生。
// 这里若自己读 is_default，会出现"页面显示的默认模型"与"AI 实际用的模型"不是同一个 ——
// 而两者都看起来合理，没人会怀疑。
//
// 返回 false 表示当前没有任何可用对话模型。此时调用方必须**明确拒绝并给出原因**，
// 不能静默降级成空模型名：那会让上游返回一个 404，运营看到的是"AI 坏了"。
func ResolveChatModel(settingsModelID string) (string, bool) {
	snap, err := pricing.Get()
	if err != nil || snap == nil {
		return "", false
	}
	want := strings.TrimSpace(settingsModelID)
	if want == "" {
		want = strings.TrimSpace(snap.Defaults["chat"])
	}
	if want == "" {
		return "", false
	}
	for i := range snap.Models {
		m := snap.Models[i]
		if m.ID != want {
			continue
		}
		// Available 恒为 true（目录只收「启用且有启用绑定」的模型），
		// 但两个条件都判：将来目录放宽过滤时这里不需要跟着改。
		if m.Enabled && m.Available {
			return m.ID, true
		}
		return "", false
	}
	return "", false
}

// ChatModelCandidates 列出可选的对话模型（设置页下拉用）。
//
// 只收 modality=chat 且可用的模型：embeddings/rerank 之类模型放进下拉，
// 选中后每一轮都会在"不支持 tools"上失败，而报错发生在用户敲完需求之后。
func ChatModelCandidates() []pricing.ModelItem {
	snap, err := pricing.Get()
	if err != nil || snap == nil {
		return nil
	}
	out := make([]pricing.ModelItem, 0, 8)
	for i := range snap.Models {
		m := snap.Models[i]
		if m.Modality == "chat" && m.Enabled && m.Available {
			out = append(out, m)
		}
	}
	return out
}

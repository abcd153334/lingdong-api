package billing

import "lingdong-api/internal/model"

// usageOfRow 把 usages 行的归一列还原成计费入参。
//
// 只服务**回收补结**（ReclaimSettle）：那时拿不到真实用量，只有预扣期写下的估算列。
//
// 两条不能改的细节：
//   - PromptTokens 取 **EstPromptTokens**。PromptTokens 是上游实测列，pending 行恒为 0，
//     用它会让 tokens 模式的整单算成免费。
//   - ImageSpecExplicit 保持 false。它是「用户明确指定过尺寸」的标记，用来做
//     tier_mismatch 比对；回收期没有可比对的实测档位，置 true 只会凭空产出异常标记。
func usageOfRow(m *model.Usage) Usage {
	if m == nil {
		return Usage{Semantic: SemanticUnknown}
	}
	return Usage{
		PromptTokens:      m.EstPromptTokens,
		ImageSpec:         m.ImageSpec,
		ImageQuality:      m.ImageQuality,
		ImageN:            m.ImageN,
		AudioDurationSecs: m.AudioDurationSecs,
		AudioChars:        m.AudioChars,
		RerankDocs:        m.RerankDocs,
		VideoResolution:   m.VideoResolution,
		VideoSeconds:      m.VideoSeconds,
		RefImages:         m.RefImages,
		RefVideos:         m.RefVideos,
		RefAudios:         m.RefAudios,
		RefVideoSeconds:   m.RefVideoSeconds,
		Semantic:          SemanticUnknown,
	}
}

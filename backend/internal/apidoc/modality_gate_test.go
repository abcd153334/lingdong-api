package apidoc

import (
	"testing"

	"lingdong-api/internal/pkg/modality"
)

// TestModalityTableIsComplete 守「词表 ↔ 默认端点」两步一致。
//
// 两处漂移的后果不同，但都不报错：
//   - 词表有、默认端点没有 ⇒ 该模态的模型在 `models.endpoints` 为空时派生不出端点，
//     表现是"模型在售但永远路由不到"（探针会判它不可用）；
//   - 默认端点有、词表没有 ⇒ 那段配置是死的，且文档里查不到这个模态。
func TestModalityTableIsComplete(t *testing.T) {
	for _, m := range modality.All {
		if len(modalityDefaults[m]) == 0 {
			t.Errorf("模态 %q 没有默认端点（modalityDefaults 缺键）", m)
		}
	}
	for m := range modalityDefaults {
		if !modality.IsValid(m) {
			t.Errorf("modalityDefaults 里有词表之外的键 %q", m)
		}
	}
}

// TestModalityLanguagePartition 守「语言类模态」这条分界线只有一个判据。
//
// SectionOfModel 用它决定"建不建模型文档"。判据与词表分叉时，
// chat 会被当成生成类建出一篇模型文档（而协议篇里已经有一份），
// 同一份内容出现两处且没人知道该改哪一份。
func TestModalityLanguagePartition(t *testing.T) {
	lang := []string{"chat", "embedding", "rerank", "moderation"}
	for _, m := range modality.All {
		want := false
		for _, l := range lang {
			if l == m {
				want = true
			}
		}
		if got := modality.Language(m); got != want {
			t.Errorf("modality.Language(%q)=%v，期望 %v", m, got, want)
		}
		if want {
			if sec := SectionOfModel(ModelFacts{Modality: m}); sec != SectionProtocols {
				t.Errorf("%q 是语言类，应落协议篇，实际 %q", m, sec)
			}
		}
	}
}

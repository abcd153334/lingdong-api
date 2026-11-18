package db

import "testing"

// 合并规则只有一件事要说清：**页面上读起来是几段，库里就是几段**。
// 这里用真实的两种字段做样本（size 的两段讲同一件事、quality 的两段讲两件事）。
func TestMergeFieldText(t *testing.T) {
	sizeGroups := []byte(`[{"text":"比例写法（如 ` + "`16:9`" + `）按下表换算成像素；也可直接填像素值。","rows":[{"k":"1:1","v":"1024x1024 · 2048x2048"}]}]`)
	got := mergeFieldText(
		"出图尺寸：比例、档位名或像素值。",
		"比例写法（如 16:9）、档位名（如 2K）或像素「宽x高」（如 2048x1152）。",
		sizeGroups,
	)
	want := "出图尺寸：比例、档位名或像素值。\n" +
		"比例写法（如 `16:9`）按下表换算成像素；也可直接填像素值。\n" +
		"1:1　1024x1024 · 2048x2048"
	if got != want {
		t.Fatalf("同一件事的两段应合并成一段：\n got=%q\nwant=%q", got, want)
	}

	qGroups := []byte(`[{"text":"只支持 ` + "`auto`" + `。","models":["gpt-image-2","gpt-image-2.5"]}]`)
	got = mergeFieldText("画质档位。", "不写按模型默认。", qGroups)
	want = "画质档位。\n不写按模型默认。\ngpt-image-2、gpt-image-2.5：只支持 `auto`。"
	if got != want {
		t.Fatalf("讲两件事的两段都要留：\n got=%q\nwant=%q", got, want)
	}

	// 没有 note / groups 时一个字都不动（幂等的前提）。
	if got := mergeFieldText("只有说明", "", nil); got != "只有说明" {
		t.Fatalf("不该改动：%q", got)
	}
}

package presets

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
)

// 本文件只做一件事：把 `presets_builtin.json`（由 tools/presetdump 生成）解成
// `[]presetGroup`。数据与代码分开是为了让 16 个模型的价目/能力在评审里可 diff，
// 而不是埋进上千行 Go 字面量。
//
// 严格解码（DisallowUnknownFields）：字段拼错在**启动时**就 panic，而不是被静默忽略
// —— 一个拼错的价目键会让模型"看起来有价、结算时读不到"，比启动失败难查得多。

//go:embed presets_builtin.json
var builtinPresetJSON []byte

func builtinPresetGroups() []presetGroup {
	if len(bytes.TrimSpace(builtinPresetJSON)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(builtinPresetJSON))
	dec.DisallowUnknownFields()
	var out []presetGroup
	if err := dec.Decode(&out); err != nil {
		panic(fmt.Sprintf("presets: presets_builtin.json 不合法（请用 tools/presetdump 重新生成）: %v", err))
	}
	return out
}

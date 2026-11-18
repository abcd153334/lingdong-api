package agent

import (
	"encoding/json"
	"strings"
)

// 本文件是「知识包读取」的工具实现（skill_read）。
//
// 对外文档的读写（v7）在 `tools_docs_v7.go`：那组工具有自己的作用域闸门与
// 发布判据，与知识包读取放在一起会让"读一篇 md"和"改一篇对外文档"混在同一个文件里。
// 上传文档的分段读取（docs_read）在 `docs.go`。

// skillReadParams 由知识包注册表生成 enum（md 增删后模型不会看到过期 id）。
func skillReadParams() json.RawMessage {
	m := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"id": map[string]any{
				"type": "string", "enum": SkillIDs(),
				"description": "知识包 id（见系统提示词里的目录）",
			},
			"offset": map[string]any{"type": "integer", "description": "字节偏移，续读时传上一轮的 next_offset"},
			"limit":  map[string]any{"type": "integer", "description": "本次读取字节数，默认 4096，上限 8192"},
		},
		"required": []string{"id"},
	}
	b, _ := json.Marshal(m)
	return b
}

func toolSkillRead(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		ID     string `json:"id"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "id"), nil
	}
	// 先在工具层夹取并**显式告知**：ReadSkill 内部也会夹，但那是静默的，
	// 模型会以为自己要了 16KB、然后按 8192 续读 → 中间一段从未被读过。
	off := in.Offset
	if off < 0 {
		off = 0
	}
	limit := in.Limit
	limitClamped := false
	if limit <= 0 {
		limit = 4 << 10
	}
	if limit > 8<<10 {
		limit = 8 << 10
		limitClamped = true
	}
	text, next, more, ok := ReadSkill(strings.TrimSpace(in.ID), off, limit)
	if !ok {
		return toolError("not_found", "没有这篇知识包："+in.ID, "id"), nil
	}
	out := map[string]any{
		"ok": true, "id": in.ID, "text": text,
		"next_offset": next, "eof": !more, "total_bytes": SkillLen(in.ID),
		// offset 必须回显：clipResult 在正文被二次截断时靠它把 next_offset
		// 退回本次起点（宁可重读，不许跳段）。缺了它 next_offset 保持截断前的
		// 值，而正文只给了一半 —— 那一段会被**静默跳过**。
		"offset": off,
	}
	if limitClamped {
		// 与 docs_read 同口径：入参超上限被夹取时要明说，否则模型以为要了 16KB。
		out["limit_clamped"] = true
	}
	return out, nil
}

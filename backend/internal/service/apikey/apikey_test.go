package apikey

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// TestGenerate 覆盖明文格式与索引三元组（ld- + 32 位 base62，prefix = 前 11 位）。
func TestGenerate(t *testing.T) {
	plain, hash, prefix, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "ld-") {
		t.Errorf("明文前缀应为 ld-，实际 %q", plain)
	}
	if len(plain) != 35 {
		t.Errorf("明文长度应为 35（ld- + 32），实际 %d", len(plain))
	}
	if prefix != plain[:11] {
		t.Errorf("prefix 应为明文前 11 位，实际 %q", prefix)
	}
	if hash != Hash(plain) {
		t.Errorf("hash 与 Hash(plain) 不一致")
	}
	if len(hash) != 64 {
		t.Errorf("SHA-256 十六进制长度应为 64，实际 %d", len(hash))
	}
}

// TestGenerateUnique 两次生成不重复。
func TestGenerateUnique(t *testing.T) {
	p1, _, _, _ := Generate()
	p2, _, _, _ := Generate()
	if p1 == p2 {
		t.Errorf("两次生成的明文不应相同")
	}
}

// TestAllowsModel 覆盖双白名单契约（空 = 全放行；任一不匹配 = 拒绝）。
func TestAllowsModel(t *testing.T) {
	empty := &model.APIKey{}
	if !AllowsModel(empty, "gpt-4o", "chat") {
		t.Errorf("双白名单为空时应放行全部模型")
	}

	byModel := &model.APIKey{ModelsAllowed: model.MustJSON([]string{"gpt-4o"})}
	if !AllowsModel(byModel, "gpt-4o", "chat") {
		t.Errorf("白名单内模型应放行")
	}
	if AllowsModel(byModel, "claude-sonnet-4-5", "chat") {
		t.Errorf("白名单外模型应拒绝")
	}

	byModal := &model.APIKey{ModalitiesAllowed: model.MustJSON([]string{"chat", "embedding"})}
	if AllowsModel(byModal, "gpt-4o", "chat") != true {
		t.Errorf("白名单内模态应放行")
	}
	if AllowsModel(byModal, "doubao-seedream-4.0", "image") {
		t.Errorf("白名单外模态应拒绝")
	}

	both := &model.APIKey{
		ModelsAllowed:     model.MustJSON([]string{"gpt-4o"}),
		ModalitiesAllowed: model.MustJSON([]string{"chat"}),
	}
	if !AllowsModel(both, "gpt-4o", "chat") {
		t.Errorf("双白名单同时命中应放行")
	}
	if AllowsModel(both, "gpt-4o", "embedding") {
		t.Errorf("模态白名单未命中应拒绝")
	}
}

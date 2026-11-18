package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// TestEnforceScopeOnlyAllowsDeclaredEntities 是「新建适配器」入口的安全边界。
//
// 背景：从「新建适配器」进弹窗时，AI 完全可以顺手生成一条改价或改别人渠道的提案，
// 而运营看到的是"帮我建个适配器"——点应用就真改了。scope 是**入口的自我约束**，
// 由前端按"从哪个按钮进来"给出，服务端只校验不存储。
func TestEnforceScopeOnlyAllowsDeclaredEntities(t *testing.T) {
	prop := func(entities ...string) *model.AgentProposal {
		parts := make([]string, 0, len(entities))
		for i, e := range entities {
			parts = append(parts, fmt.Sprintf(`{"op":"create","entity":%q,"key":"k%d"}`, e, i))
		}
		raw := `{"summary":"s","operations":[` + strings.Join(parts, ",") + `]}`
		return &model.AgentProposal{Kind: "change", Plan: model.JSON([]byte(raw))}
	}

	// 空 scope = 不限制：独立助手页不传，行为必须与改造前完全一致。
	if err := EnforceScope(prop(EntityChannel, EntityModel), nil); err != nil {
		t.Fatalf("空 scope 不应限制任何实体，实际 %v", err)
	}

	// 只含 adapter_spec → 通过
	if err := EnforceScope(prop(EntityAdapterSpec), []string{EntityAdapterSpec}); err != nil {
		t.Fatalf("只含声明实体的提案应通过，实际 %v", err)
	}

	// 混入 channel → 越界
	err := EnforceScope(prop(EntityAdapterSpec, EntityChannel), []string{EntityAdapterSpec})
	if !errors.Is(err, ErrOutOfScope) {
		t.Fatalf("含未声明实体应返回 ErrOutOfScope，实际 %v", err)
	}
	// 错误里必须点名是哪个实体：只说"越界"时运营不知道该去哪个入口处理。
	if !strings.Contains(err.Error(), EntityChannel) {
		t.Fatalf("越界错误应点名实体，实际：%v", err)
	}

	// 空操作清单（unsupported 提案的形态）→ 通不过 apply 但那不是 scope 的事
	if err := EnforceScope(prop(), []string{EntityAdapterSpec}); err != nil {
		t.Fatalf("无操作的提案在 scope 上应通过（由 NotApplyable 负责拦），实际 %v", err)
	}

	// nil 提案 → 找不到
	if err := EnforceScope(nil, []string{EntityAdapterSpec}); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("nil 提案应返回 ErrProposalNotFound，实际 %v", err)
	}
}

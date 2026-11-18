package billing

import (
	"context"
	"strconv"
	"testing"

	"lingdong-api/internal/model"
)

// 统一释放出口：第一次真退，第二次幂等返回 false（不动账、不报错）。
//
// 这条约束是 4 处释放逻辑收敛的前提 —— 调用方不该再为「别人已经处理完了」
// 写错误分支，那些分支正是历史上语义分叉的来源。
func TestReleaseByUsageIDIdempotent(t *testing.T) {
	base := context.Background()
	svc, usageID := seedPending(t, base, 1000, 100, tokenSnap())

	released, err := svc.ReleaseByUsageID(base, usageID, SettleInput{
		StatusCode: 500, ErrorMsg: "test", UsageSource: "none",
	})
	if err != nil {
		t.Fatalf("首次释放失败: %v", err)
	}
	if !released {
		t.Fatal("首次释放应返回 released=true")
	}

	var u model.User
	if err := svc.db.First(&u, 1).Error; err != nil {
		t.Fatalf("读用户失败: %v", err)
	}
	if u.Balance != 10000 || u.Frozen != 0 {
		t.Fatalf("冻结未退回：balance=%d frozen=%d", u.Balance, u.Frozen)
	}

	released, err = svc.ReleaseByUsageID(base, usageID, SettleInput{StatusCode: 500})
	if err != nil {
		t.Fatalf("重复释放必须幂等（err=nil），实际 %v", err)
	}
	if released {
		t.Fatal("重复释放应返回 released=false")
	}

	var n int64
	if err := svc.db.Model(&model.Transaction{}).
		Where("ref_type = 'usage' AND ref_id = ? AND type = ?", strconv.FormatUint(usageID, 10), model.TxnRelease).
		Count(&n).Error; err != nil {
		t.Fatalf("统计释放流水失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("释放流水应恰有 1 条，实际 %d", n)
	}
}

// usageID=0（没有会话）必须直接返回，不查库、不报错。
func TestReleaseByUsageIDZeroIsNoop(t *testing.T) {
	svc := New(nil)
	released, err := svc.ReleaseByUsageID(context.Background(), 0, SettleInput{})
	if err != nil || released {
		t.Fatalf("usageID=0 应为空操作：released=%v err=%v", released, err)
	}
}

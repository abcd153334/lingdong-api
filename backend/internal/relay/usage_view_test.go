package relay

import (
	"testing"
	"time"

	"lingdong-api/internal/service/logs"
)

// TestUsageRowToViewFillsKeyName 守住「调用记录列表带密钥名」。
//
// 这是曾经漏掉的一环：`logs.PortalItem.KeyName` 字段有、`logs.PortalItemOf` 也拷贝它、
// 前端「调用记录」页也有 KEY 列，唯独 `usageRow.toItem` 构造 `logs.Item{}` 时没传 ——
// 表现为门户那一列**恒显示「—」**，而对外 API 更是无法把一笔消费归到某个应用
// （应用排查"这笔钱是不是我的用户花的"时唯一可用的线索就是这个字段）。
func TestUsageRowToViewFillsKeyName(t *testing.T) {
	r := usageRow{ID: 42, KeyID: 7, Model: "gpt-4o-mini", Modality: "chat", Status: "settled", Charge: 1000}
	// 两步走与生产同路：先建内部 Item（补号也在这个对象上做），再投影成 PortalItem。
	v := logs.PortalItemOf(r.toItem(time.Now(), map[uint64]string{7: "写作助手 托管"}), "", 0, 0, 0)
	if v.KeyName != "写作助手 托管" {
		t.Fatalf("KeyName = %q，期望 %q", v.KeyName, "写作助手 托管")
	}
	if v.KeyID != 7 {
		t.Fatalf("KeyID = %d，期望 7", v.KeyID)
	}
}

// TestUsageRowToViewMissingKeyNameIsEmpty 取不到名字时退化为空串而不是 panic。
//
// key_id 可能指向已软删的密钥：logs.KeyNames 用 `WHERE id IN (...)` 直接查表，
// 软删行（deleted_at 非空）也在表里，但真删过的 id 就查不到 —— 那时返回空 map，
// 页面显示空白，而不是让整个列表 500。
func TestUsageRowToViewMissingKeyNameIsEmpty(t *testing.T) {
	r := usageRow{ID: 1, KeyID: 99, Status: "settled"}
	v := logs.PortalItemOf(r.toItem(time.Now(), map[uint64]string{}), "", 0, 0, 0)
	if v.KeyName != "" {
		t.Fatalf("KeyName = %q，期望空串", v.KeyName)
	}
}

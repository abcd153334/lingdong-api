package worker

import (
	"context"
	"testing"

	"lingdong-api/internal/model"
)

// TestPrefetchChannelsDedupesPerBatch 守「一轮 200 条同渠道只查一次、只解密一次」。
//
// 改造前每条任务各查一次 channels 全行（含密钥密文）并各解密一次：
// 200 条/轮 × 每 5 秒一轮，纯 CPU 与连接池浪费。这里只验「同一批次内按渠道去重
// 且解密结果被复用」——查询次数本身由实现保证（一次 IN）。
func TestPrefetchChannelsDedupesPerBatch(t *testing.T) {
	gdb := workerTestDB(t)
	if err := gdb.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("迁移 channels 失败：%v", err)
	}
	ch := model.Channel{
		Name: "预取渠道", AdapterID: "openai", BaseURL: "https://api.example.com",
		Keys:   model.MustJSON([]string{"token-a", "token-b"}),
		Status: model.ChannelStatusEnabled,
	}
	if err := gdb.Create(&ch).Error; err != nil {
		t.Fatalf("构造渠道失败：%v", err)
	}

	rows := make([]model.Task, 0, 200)
	for i := 0; i < 200; i++ {
		rows = append(rows, model.Task{ChannelID: ch.ID})
	}
	pf := prefetchChannels(context.Background(), rows)
	if len(pf.ch) != 1 {
		t.Fatalf("200 条同渠道任务应只预取 1 个渠道，实际 %d", len(pf.ch))
	}

	got, res, ok := pf.lookup(ch.ID)
	if !ok || got == nil || got.ID != ch.ID {
		t.Fatalf("预取结果应按 channel_id 命中：ok=%v got=%+v", ok, got)
	}
	// 解密结果被复用：两次 lookup 拿到同一份（而不是每次都重新解密）。
	if res.Total != 2 {
		t.Fatalf("应复用一次解密得到的 2 个条目，实际 Total=%d", res.Total)
	}
	if _, _, ok := pf.lookup(ch.ID + 999); ok {
		t.Fatal("未预取的渠道必须未命中（调用方回落单条查询）")
	}

	// 空批次不炸（轮询可能取到空批）。
	empty := prefetchChannels(context.Background(), nil)
	if _, _, ok := empty.lookup(ch.ID); ok {
		t.Fatal("空预取不该命中任何渠道")
	}
}

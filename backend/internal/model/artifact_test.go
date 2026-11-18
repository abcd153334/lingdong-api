package model

import (
	"testing"
	"time"
)

// TestArtifactListing 锁死产物清单的归一规则：跳过过期、序号稠密、MIME 生效、渲染档。
//
// 这几条规则原先在 task.ArtifactsOf 与 logs.artifactRefs 各写一份，漂移的表现是
// 「同一张图在任务页能预览、在调用记录里显示成文件」。收成一处后由本测试守住。
func TestArtifactListing(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	rows := []File{
		{ArtifactIndex: 0, Filename: "a.png", ContentType: "image/png", SizeBytes: 100},
		// 过期项必须跳过，且不占号（否则后续序号整体后移，点开错位）。
		{ArtifactIndex: 1, Filename: "b.png", ContentType: "image/png", SizeBytes: 200, ExpiresAt: &past},
		// octet-stream 要按扩展名补出真实 MIME，否则 kind 退化成 file。
		{ArtifactIndex: 2, Filename: "c.mp3", ContentType: "application/octet-stream", SizeBytes: 300},
		{ArtifactIndex: 3, Filename: "d.txt", ContentType: "text/plain", SizeBytes: 400, ExpiresAt: &future},
	}

	got := ArtifactListing(rows, now)
	if len(got) != 3 {
		t.Fatalf("应跳过 1 个过期项，实际 %d 项: %+v", len(got), got)
	}
	want := []Artifact{
		{Index: 0, ContentType: "image/png", Bytes: 100, Kind: "image"},
		{Index: 1, ContentType: "audio/mpeg", Bytes: 300, Kind: "audio"},
		{Index: 2, ContentType: "text/plain", Bytes: 400, Kind: "text"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项不符：want %+v, got %+v", i, want[i], got[i])
		}
	}
}

// TestArtifactListingEmpty 空输入返回 nil（不返回空切片，调用方按 nil 判缺省）。
func TestArtifactListingEmpty(t *testing.T) {
	if got := ArtifactListing(nil, time.Now()); got != nil {
		t.Fatalf("空输入应为 nil，实际 %+v", got)
	}
	if got := ArtifactListing([]File{}, time.Now()); got != nil {
		t.Fatalf("空切片应为 nil，实际 %+v", got)
	}
}

// TestArtifactListingSVGIsFile SVG 不当作可内联图片（可内嵌脚本），渲染档固定 file。
func TestArtifactListingSVGIsFile(t *testing.T) {
	got := ArtifactListing([]File{{
		Filename: "x.svg", ContentType: "image/svg+xml", SizeBytes: 10,
	}}, time.Now())
	if len(got) != 1 || got[0].Kind != "file" {
		t.Fatalf("SVG 应为 file，实际 %+v", got)
	}
}

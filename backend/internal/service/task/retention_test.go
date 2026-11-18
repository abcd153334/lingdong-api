package task

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/storage"
)

// fakeStore 是产物删除用的假后端（只关心删除成败与被删 key）。
type fakeStore struct {
	err     error
	deleted []string
}

func (f *fakeStore) Name() string { return "fake" }

func (f *fakeStore) Bucket() string { return "" }

func (f *fakeStore) Put(context.Context, string, io.Reader, int64, string) error { return nil }

func (f *fakeStore) Open(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, storage.ErrObjectNotFound
}

// Head 跟随 err：注入错误时模拟「删了但复核不了」，正常时模拟「已不存在」。
func (f *fakeStore) Head(context.Context, string) (storage.ObjectInfo, error) {
	if f.err != nil {
		return storage.ObjectInfo{}, f.err
	}
	return storage.ObjectInfo{}, storage.ErrObjectNotFound
}

func (f *fakeStore) CheckBucket(context.Context) (storage.BucketState, error) {
	return storage.BucketState{Exists: true}, nil
}

func (f *fakeStore) Delete(_ context.Context, key string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, key)
	return nil
}

// withFakeStore 把 deleteRefVerified 换成「假后端的 删除 + 复核」（用例结束恢复）。
//
// 注入点只到 DeleteRefVerified 这一层：写入/读取的路由由 storage 包自己保证，
// 这里要验证的是「对象删不掉时不许删行」，不需要真的落一个后端。
func withFakeStore(t *testing.T, st storage.Store) {
	t.Helper()
	prev := deleteRefVerified
	deleteRefVerified = func(ctx context.Context, ref storage.Ref) (storage.DeleteOutcome, error) {
		_ = st.Delete(ctx, ref.Key)
		if _, err := st.Head(ctx, ref.Key); errors.Is(err, storage.ErrObjectNotFound) {
			return storage.DeleteGone, nil
		}
		// 复核失败或对象仍在：与真实 DeleteRefVerified 同语义（保留行）。
		return storage.DeleteUnverified, storage.ErrDeleteUnverified
	}
	t.Cleanup(func() { deleteRefVerified = prev })
}

// seedTask 插入一条任务，返回其 id（created_at 显式指定，不受 GORM 自动填充影响）。
func seedTask(t *testing.T, status string, createdAt time.Time) uint64 {
	t.Helper()
	row := model.Task{
		UserID: 1, KeyID: 1, UsageID: 1, Model: "m", Modality: "video",
		Status: status, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if err := db.DB.Create(&row).Error; err != nil {
		t.Fatalf("插入任务失败: %v", err)
	}
	return row.ID
}

// seedFile 插入一条产物记录。
func seedFile(t *testing.T, taskID uint64, index int, size int64) {
	t.Helper()
	seedFileOn(t, taskID, index, size, "none", "files/1/a.bin")
}

// seedFileOn 插入一条指定存储后端的产物记录（用于「后端已消失」场景）。
func seedFileOn(t *testing.T, taskID uint64, index int, size int64, backend, key string) {
	t.Helper()
	row := model.File{
		TaskID: taskID, ArtifactIndex: index, UserID: 1, Filename: "a.bin",
		SizeBytes: size, StorageBackend: backend, StorageKey: key, ContentType: "image/png",
	}
	if err := db.DB.Create(&row).Error; err != nil {
		t.Fatalf("插入产物失败: %v", err)
	}
}

// TestPurgeTerminalTasksNotStarvedByDeadBackend 守住「清理队列不被不可用后端饿死」。
//
// 场景：一个较早的任务，其产物当初写到了 oss，而 oss 已从配置里移除；
// 另一个较晚的任务落在本地磁盘。查询是 ORDER BY created_at LIMIT batch，
// 若只把「后端不可构建」放在循环里 continue，每轮都先取到那个 oss 任务、
// 跳过、再取同一批 —— 正常任务永远清不掉，磁盘只涨不降。
func TestPurgeTerminalTasksNotStarvedByDeadBackend(t *testing.T) {
	gdb := withTestDB(t)
	if err := gdb.AutoMigrate(&model.File{}); err != nil {
		t.Fatalf("迁移 files 失败: %v", err)
	}
	st := &fakeStore{}
	withFakeStore(t, st)

	now := time.Now()
	deadID := seedTask(t, StatusSucceeded, now.Add(-60*24*time.Hour))
	seedFileOn(t, deadID, 0, 100, "oss", "lost/0")
	liveID := seedTask(t, StatusFailed, now.Add(-40*24*time.Hour))
	seedFile(t, liveID, 0, 50)

	rep, err := PurgeTerminalTasks(context.Background(), 30, 1, now)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if rep.Tasks != 1 || rep.Files != 1 {
		t.Fatalf("batch=1 应清掉那条正常任务: %+v", rep)
	}
	if len(st.deleted) != 1 || st.deleted[0] != "files/1/a.bin" {
		t.Fatalf("只应删除本地那个对象: %v", st.deleted)
	}
	// 后端已移除的任务必须整条保留：对象还在桶里，删行等于制造孤儿对象。
	var deadLeft, deadFiles int64
	db.DB.Unscoped().Model(&model.Task{}).Where("id = ?", deadID).Count(&deadLeft)
	db.DB.Model(&model.File{}).Where("task_id = ?", deadID).Count(&deadFiles)
	if deadLeft != 1 || deadFiles != 1 {
		t.Fatalf("后端不可用的任务必须原样保留（task=%d file=%d）", deadLeft, deadFiles)
	}
	var liveLeft int64
	db.DB.Unscoped().Model(&model.Task{}).Where("id = ?", liveID).Count(&liveLeft)
	if liveLeft != 0 {
		t.Fatal("正常的超期终态任务应被物理删除")
	}
}

// TestPurgeTerminalTasksReportOnly 未配置保留期时只统计、不删除（默认行为）。
func TestPurgeTerminalTasksReportOnly(t *testing.T) {
	gdb := withTestDB(t)
	if err := gdb.AutoMigrate(&model.File{}); err != nil {
		t.Fatalf("迁移 files 失败: %v", err)
	}
	st := &fakeStore{}
	withFakeStore(t, st)

	now := time.Now()
	seedTask(t, StatusSucceeded, now.Add(-400*24*time.Hour))
	seedTask(t, StatusFailed, now.Add(-400*24*time.Hour))
	seedTask(t, StatusRunning, now.Add(-400*24*time.Hour))

	rep, err := PurgeTerminalTasks(context.Background(), 0, 200, now)
	if err != nil {
		t.Fatalf("只报告模式不应报错: %v", err)
	}
	if rep.Enabled {
		t.Fatal("days=0 时不得进入删除模式")
	}
	if rep.Candidates != 2 {
		t.Fatalf("候选数应为 2（只算终态），实际 %d", rep.Candidates)
	}
	if rep.Tasks != 0 || rep.Files != 0 {
		t.Fatalf("只报告模式不得删除任何数据: %+v", rep)
	}
	var left int64
	db.DB.Model(&model.Task{}).Count(&left)
	if left != 3 {
		t.Fatalf("任务被误删：期望 3 条，实际 %d", left)
	}
}

// TestPurgeTerminalTasksDeletesOnlyOldTerminal 只删「超期 + 终态」，其余原样保留。
func TestPurgeTerminalTasksDeletesOnlyOldTerminal(t *testing.T) {
	gdb := withTestDB(t)
	if err := gdb.AutoMigrate(&model.File{}); err != nil {
		t.Fatalf("迁移 files 失败: %v", err)
	}
	st := &fakeStore{}
	withFakeStore(t, st)

	now := time.Now()
	oldID := seedTask(t, StatusSucceeded, now.Add(-40*24*time.Hour))
	seedFile(t, oldID, 0, 100)
	seedFile(t, oldID, 1, 250)
	recentID := seedTask(t, StatusSucceeded, now.Add(-2*time.Hour))
	seedFile(t, recentID, 0, 10)
	runningID := seedTask(t, StatusRunning, now.Add(-90*24*time.Hour))
	seedFile(t, runningID, 0, 10)

	rep, err := PurgeTerminalTasks(context.Background(), 30, 200, now)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if !rep.Enabled || rep.Tasks != 1 || rep.Files != 2 || rep.Bytes != 350 {
		t.Fatalf("清理统计不符: %+v", rep)
	}
	if len(st.deleted) != 2 {
		t.Fatalf("应删除 2 个对象，实际 %v", st.deleted)
	}

	var taskLeft int64
	db.DB.Unscoped().Model(&model.Task{}).Where("id = ?", oldID).Count(&taskLeft)
	if taskLeft != 0 {
		t.Fatal("超期终态任务应被物理删除")
	}
	var fileLeft int64
	db.DB.Model(&model.File{}).Where("task_id = ?", oldID).Count(&fileLeft)
	if fileLeft != 0 {
		t.Fatal("超期任务的产物行应被删除")
	}
	// 未超期与在途任务必须原封不动 —— 删掉 running 任务会让预扣额度变成无主挂账。
	for _, id := range []uint64{recentID, runningID} {
		var n int64
		db.DB.Unscoped().Model(&model.Task{}).Where("id = ?", id).Count(&n)
		if n != 1 {
			t.Fatalf("任务 %d 被误删", id)
		}
	}
}

// TestPurgeTerminalTasksKeepsTaskWhenObjectDeleteFails 对象删不掉时**整条保留**待下轮重试。
//
// 这是「先删对象后删行」的直接后果：若先删行，一次存储抖动就会留下永远找不到的孤儿对象。
func TestPurgeTerminalTasksKeepsTaskWhenObjectDeleteFails(t *testing.T) {
	gdb := withTestDB(t)
	if err := gdb.AutoMigrate(&model.File{}); err != nil {
		t.Fatalf("迁移 files 失败: %v", err)
	}
	withFakeStore(t, &fakeStore{err: errors.New("s3 503")})

	now := time.Now()
	id := seedTask(t, StatusFailed, now.Add(-40*24*time.Hour))
	seedFile(t, id, 0, 100)

	rep, err := PurgeTerminalTasks(context.Background(), 30, 200, now)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if rep.Tasks != 0 || rep.Skipped != 1 {
		t.Fatalf("删除失败时应跳过而不是删行: %+v", rep)
	}
	var tasks, files int64
	db.DB.Unscoped().Model(&model.Task{}).Where("id = ?", id).Count(&tasks)
	db.DB.Model(&model.File{}).Where("task_id = ?", id).Count(&files)
	if tasks != 1 || files != 1 {
		t.Fatalf("任务行/产物行必须保留待重试（task=%d file=%d）", tasks, files)
	}
}

// TestPurgeTaskRejectsNonTerminal 单条物理删除只认终态。
func TestPurgeTaskRejectsNonTerminal(t *testing.T) {
	withTestDB(t)
	for _, status := range []string{StatusQueued, StatusRunning} {
		row := &model.Task{ID: 1, Status: status}
		if _, _, err := PurgeTask(context.Background(), row); !errors.Is(err, ErrNotTerminal) {
			t.Fatalf("%s 应返回 ErrNotTerminal，实际 %v", status, err)
		}
	}
}

// TestPurgeTaskDeletesArtifactsAndTask 单条删除：产物先删、任务后删。
func TestPurgeTaskDeletesArtifactsAndTask(t *testing.T) {
	gdb := withTestDB(t)
	if err := gdb.AutoMigrate(&model.File{}); err != nil {
		t.Fatalf("迁移 files 失败: %v", err)
	}
	st := &fakeStore{}
	withFakeStore(t, st)

	now := time.Now()
	id := seedTask(t, StatusSucceeded, now.Add(-time.Hour))
	seedFile(t, id, 0, 40)
	seedFile(t, id, 1, 60)

	row, err := Get(context.Background(), id, AllScope())
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	files, bytes, perr := PurgeTask(context.Background(), row)
	if perr != nil {
		t.Fatalf("删除失败: %v", perr)
	}
	if files != 2 || bytes != 100 || len(st.deleted) != 2 {
		t.Fatalf("删除统计不符: files=%d bytes=%d deleted=%v", files, bytes, st.deleted)
	}
	var n int64
	db.DB.Unscoped().Model(&model.Task{}).Where("id = ?", id).Count(&n)
	if n != 0 {
		t.Fatal("任务行应被物理删除")
	}
}

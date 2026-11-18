package db

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// migrateLockID 是迁移用的 advisory lock 编号。
//
// 取值规则：任意固定的 int64，只要全平台一致。选这个数是因为它不像业务 ID，
// 全局 grep 能唯一定位；**改它等于换锁**，两个版本共存时会各锁各的（等于没锁）。
const migrateLockID int64 = 741923001

// migrateLockWait 是第二个实例等锁的上限。
//
// 迁移本身通常 <10s，但首次建库 + 索引 CONCURRENTLY 可能到分钟级。
// 超时后**拒绝启动**而不是跳过迁移：跳过意味着带着旧表结构对外服务，
// 表现是随机 500（列不存在），比启动失败难查得多。
const migrateLockWait = 120 * time.Second

// DocsWarmLockID 是「文档预热」的 advisory lock 编号。
//
// 与迁移锁不同号：两者可能同时发生（一个实例在迁移、另一个已完成并开始预热），
// 共号会让预热被迁移锁挡住或反之。编号一旦发布就不要改 —— 改了等于换锁，
// 滚动升级期间新旧版本会各锁各的（等于没锁）。
const DocsWarmLockID int64 = 741923002

// TryAdvisoryLock 尝试获取一个会话级 advisory lock；拿不到立即返回 ok=false（不阻塞）。
//
// 用途：只允许一个实例做「全表重算」这类一次性工作（main.warmDocs）。
// 与 WithMigrationLock 的区别：迁移必须**等到**锁（跳过迁移会带着旧表结构对外），
// 而预热是幂等增强，等锁只会把启动拖慢、把容器探针拖过 timeout，所以 try + 跳过。
func TryAdvisoryLock(gdb *gorm.DB, id int64) (release func(), ok bool, err error) {
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, false, fmt.Errorf("db: 取连接池失败: %w", err)
	}
	ctx := context.Background()
	// 必须独占一条连接：advisory lock 是会话级的，从池里随手取会导致
	// "锁在这条、解锁在另一条"，锁要等连接被回收才释放。
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("db: 取独占连接失败: %w", err)
	}
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", id).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("db: 获取 advisory lock 失败: %w", err)
	}
	if !got {
		_ = conn.Close()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", id)
		_ = conn.Close()
	}, true, nil
}

// WithMigrationLock 在会话级 advisory lock 内执行 fn。
//
// 为什么必须加锁：多副本同时启动时，AutoMigrate 会并发执行 ALTER TABLE。
// PG 的 DDL 虽然有表级锁，但 GORM 的 AutoMigrate 是「先查 information_schema
// 再决定 ALTER」，两个副本都查到"缺列"就都会 ALTER，第二个必然报
// duplicate column，于是**整个副本启动失败**并进入重启循环。
// 索引 CONCURRENTLY 更糟：并发建同名索引会留下 invalid 索引。
//
// 会话级（pg_advisory_lock）而不是事务级：AutoMigrate 与个别数据迁移内部
// 有自己的事务，把 DDL 包进外层事务会让 CONCURRENTLY 直接不可用。
func WithMigrationLock(gdb *gorm.DB, fn func() error) error {
	sqlDB, err := gdb.DB()
	if err != nil {
		return fmt.Errorf("db: 取连接池失败: %w", err)
	}
	ctx := context.Background()
	// 必须独占一条连接：advisory lock 是会话级的，从连接池里随手取连接
	// 会导致"锁在这条、解锁在另一条"，锁永远不释放（直到那条连接被回收）。
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("db: 取独占连接失败: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// lock_timeout 让「另一个副本正在迁移」表现为可控的错误，而不是无限挂住
	// （编排层看到的是容器一直 not healthy，没有任何日志）。
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET lock_timeout = '%dms'", migrateLockWait.Milliseconds())); err != nil {
		return fmt.Errorf("db: 设置 lock_timeout 失败: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrateLockID); err != nil {
		return fmt.Errorf("db: 获取迁移锁失败（另一实例正在迁移或等待超时）: %w", err)
	}
	// 解锁失败不值得阻断：连接关闭时会话级锁自动释放。
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrateLockID)
	}()

	return fn()
}

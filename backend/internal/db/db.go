// Package db 负责数据库与 Redis 连接初始化。
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/storage"
)

// DB 是全局 GORM 连接。
var DB *gorm.DB

// RDB 是全局 Redis 客户端；RedisReady 表示连接可用（不可用时降级）。
var RDB *redis.Client

var redisReady atomic.Bool

// Init 初始化 GORM（postgres driver）并配置连接池与 UTC 时间。
func Init(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("db: DATABASE_URL 未配置")
	}
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("db: 连接 PostgreSQL 失败: %w", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("db: 获取连接池失败: %w", err)
	}
	// 连接池上限必须可配：它是「单副本能吃多少 DB 连接」的唯一闸门。
	//
	// 原来的 50 是硬编码，压测里 10 VU 就打满了（每请求 1.84 事务），
	// 20 VU 起吞吐从 5,047 掉到 1,565 rps —— 不是 CPU 不够，是请求在池上排队。
	// 默认 100 的依据：单机 PG 默认 max_connections=100，留 3 条给超管，
	// 单副本跑 100 正好吃满；多副本部署必须**按副本数下调**（150÷3 副本 = 50），
	// 否则后起的副本会连不上（症状是启动即 "too many clients"）。
	sqlDB.SetMaxOpenConns(envInt("LD_DB_MAX_OPEN", 100))
	sqlDB.SetMaxIdleConns(envInt("LD_DB_MAX_IDLE", 20))
	sqlDB.SetConnMaxLifetime(time.Hour)

	DB = gdb
	return gdb, nil
}

// InitRedis 初始化 Redis；连不上时打 warning 并降级（不阻断启动，限流与 leader 锁跳过）。
func InitRedis(addr, password string, dbIndex int) *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       dbIndex,
	})
	RDB = client
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		redisReady.Store(false)
		logger.L().Warn("Redis 不可用，已降级（限流与 leader 锁将跳过）", zap.Error(err))
		return client
	}
	redisReady.Store(true)
	return client
}

// RedisReady 表示 Redis 是否可用。
func RedisReady() bool { return redisReady.Load() && RDB != nil }

// redisFailLimit 是判定 Redis「不可用」所需的连续失败次数。
// 取 3 而不是 1：一次网络抖动不该把限流闸、leader 锁、密钥冷却全部关掉。
const redisFailLimit = 3

// StartRedisHealthProbe 周期性 PING Redis 并维护 RedisReady 标志（P2-18）。
//
// 为什么必须做：原实现只在启动时 Ping 一次。于是
//   - 启动时 Redis 还没起来 → 之后**永远**降级（限流、leader 锁、key 冷却全失效，且日志只在启动打过一次）；
//   - 运行中 Redis 挂掉 → 标志仍为 true，每个请求都去连一个死掉的 Redis（超时放大）。
//
// 语义：成功一次立即恢复（恢复要快）；连续失败 redisFailLimit 次才置为不可用（避免抖动）。
func StartRedisHealthProbe(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		fails := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			// 先把客户端**取一次**再用：RDB 是全局变量，测试与热重连都会替换它。
			// 直接写 `RDB.Ping(...)` 是 TOCTOU —— 判空通过之后另一个 goroutine 把
			// RDB 置 nil，这一行就 nil 解引用 panic（探测 goroutine 里的 panic
			// 会带走整个进程，而它只在「Redis 刚被替换」这种时机触发，极难复现）。
			rdb := RDB
			if rdb == nil {
				continue
			}
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := rdb.Ping(pingCtx).Err()
			cancel()
			if err == nil {
				fails = 0
				if !redisReady.Load() {
					redisReady.Store(true)
					logger.L().Info("Redis 已恢复，限流与 leader 锁重新生效")
				}
				continue
			}
			fails++
			if fails >= redisFailLimit && redisReady.Load() {
				redisReady.Store(false)
				logger.L().Warn("Redis 连续探测失败，已降级（限流与 leader 锁暂停）",
					zap.Int("fails", fails), zap.Error(err))
			}
		}
	}()
}

// LoadSystemConfig 读取 system_config 全表为内存镜像数据（**已解封凭据**）。
//
// 解封放在这里而不是调用方：这是全表装载的唯一入口，启动引导（main）与周期热更新
// （configsync）都走它。任何一处漏解封的表现都是「配置页显示正常、几十秒后
// 所有云端写入开始 401」—— 因为内存镜像里躺的是密文，被当成 AK 发去了上游。
func LoadSystemConfig(gdb *gorm.DB) (map[string]json.RawMessage, error) {
	var rows []model.SystemConfig
	if err := gdb.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("db: 读取 system_config 失败: %w", err)
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, r := range rows {
		out[r.Key] = json.RawMessage(r.Value)
	}
	// 逐键解封（键集来自 storage.SecretConfigKeys，不在这里写死）。
	//
	// 支付渠道同样要解封：回调验签与下单签名直接读这些字段，拿到密文的表现是
	// 「验签全失败 / 上游 401」，而配置页看起来完全正常。
	// 解密失败时字段被置空 → 该渠道 ValidateChannelRequired 不通过 →
	// 从收银台隐藏且回调按 ErrNotConfigured 拒绝，比拿密文去签要安全得多。
	for _, key := range storage.SecretConfigKeys() {
		raw, ok := out[key]
		if !ok {
			continue
		}
		plain, failed, err := storage.UnsealSecrets(key, raw)
		if err != nil {
			// 结构坏了就原样交给上层：配置读取方各自有「解析失败」的分支，
			// 在这里吞掉错误只会让问题更难定位。
			continue
		}
		out[key] = plain
		if key == "storage" {
			// 只有存储后端有「凭据不可读」这个状态位：BackendStates 会把它报成
			// credential_unreadable，清理任务据此跳过而不是把后端当「没配」。
			storage.MarkCredentialFailures(failed)
		}
		if len(failed) > 0 {
			logger.L().Error("凭据无法解密，相关配置按未配置处理（请重新填写凭据或检查 APP_ENCRYPTION_KEY）",
				zap.String("config_key", key), zap.Strings("fields", failed))
		}
	}
	return out, nil
}

// envInt 读一个正整数环境变量；非法值/空值回落默认。
//
// 只接受 >0：池上限写成 0 会让 database/sql 认为"不限制"，
// 那正是这轮要消掉的隐患，不能靠拼错环境变量把它带回来。
func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

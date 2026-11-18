package logger

import (
	"context"

	"go.uber.org/zap"
)

var global = zap.NewNop()

// Init 初始化全局结构化日志（JSON 输出），返回 logger 实例。
func Init(level string) *zap.Logger {
	lvl := zap.NewAtomicLevel()
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl.SetLevel(zap.InfoLevel)
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = lvl
	l, err := cfg.Build()
	if err != nil {
		global = zap.NewNop()
		return global
	}
	global = l
	return l
}

// L 返回全局 logger。
func L() *zap.Logger { return global }

// WithRequestID 返回带 request_id 字段的 logger。
func WithRequestID(requestID string) *zap.Logger {
	return global.With(zap.String("request_id", requestID))
}

// requestIDCtxKey 是 request_id 在 context.Context 里的键（未导出类型，避免与其他包的键碰撞）。
type requestIDCtxKey struct{}

// WithRequestIDValue 把 request_id 写入 context，供深层函数（适配器、worker、存储）取用。
//
// 为什么同时存在 gin.Context 与 context.Context 两处：
// gin.Context 只在 HTTP 处理器内可见，而 relay → adapter → netguard 的调用链只用 context.Context，
// 不写进去的话下游日志无法与本次请求关联。
func WithRequestIDValue(ctx context.Context, requestID string) context.Context {
	if ctx == nil {
		return context.WithValue(context.Background(), requestIDCtxKey{}, requestID)
	}
	return context.WithValue(ctx, requestIDCtxKey{}, requestID)
}

// FromContext 取出 context 里的 request_id（未设置返回空串）。
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// WithContext 返回带 context 中 request_id 的 logger（未设置时等价于 L()）。
func WithContext(ctx context.Context) *zap.Logger {
	if id := FromContext(ctx); id != "" {
		return WithRequestID(id)
	}
	return global
}

// Sync 刷新缓冲日志。
func Sync() {
	_ = global.Sync()
}

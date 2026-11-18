package storage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// 域内错误哨兵。controller 层按它们映射对外错误码，因此每新增一个都必须同时
// 更新 controller 的 storageFailDef（唯一映射点）。
var (
	// ErrUnavailable 表示后端可用但本次失败（网络/5xx/鉴权/配置残缺）。
	ErrUnavailable = errors.New("storage: 存储后端不可用")
	// ErrObjectNotFound 表示对象不存在（404 语义）。
	ErrObjectNotFound = errors.New("storage: 对象不存在")
	// ErrObjectReadTimeout 表示单次读空闲超时（对端卡住而不是整体太慢）。
	ErrObjectReadTimeout = errors.New("storage: 对象读取超时")
	// ErrTimeout 表示操作整体超时。
	ErrTimeout = errors.New("storage: 存储操作超时")
	// ErrBackendGone 表示记录里的后端已从配置中消失或无法构建。
	//
	// 对外绝不能折成 410：410 的语义是「永久删除，别再来找」，而真实状态是
	// 「对象还在桶里，补配置就能读」。折错会让用户去重建文件，而账单仍在计费。
	ErrBackendGone = errors.New("storage: 存储后端配置已移除")
	// ErrBackendMissing 表示记录里根本没有后端标识（写路径漏记，属于 bug）。
	ErrBackendMissing = errors.New("storage: 对象记录缺少存储后端")
	// ErrBucketMissing 表示记录在云后端上却没有桶标识（写路径漏记，属于 bug）。
	//
	// 与 ErrBackendMissing 分开只为让日志一句话说清缺的是哪一列。
	// **绝不回退到当前配置的桶**：那正是「换桶后删错地方却报成功」的成因。
	ErrBucketMissing = errors.New("storage: 对象记录缺少存储桶")
	// ErrDeleteUnverified 表示删除后复核仍能读到对象（或无法复核）。
	//
	// 语义是「状态不明」：调用方**不得**删记录行，否则对象会失去唯一的定位线索。
	// 与 ErrUnavailable 分开是因为处置不同：这个是「保留并等下轮/等人工」，
	// 那个是「后端抖了，重试」。
	ErrDeleteUnverified = errors.New("storage: 删除后复核仍能读到对象")
	// ErrNeedLength 表示调用方没有给出真实长度（内部契约错误）。
	ErrNeedLength = errors.New("storage: 写入必须提供真实长度")
	// ErrRangeUnsupported 表示后端不支持区间读取，调用方应降级为全量读取。
	ErrRangeUnsupported = errors.New("storage: 后端不支持区间读取")
	// ErrInvalidKey 表示对象 key 非法（越界路径）。
	ErrInvalidKey = errors.New("storage: 非法对象 key")
	// ErrNotConfigured 表示 system_config 里没有可用的 storage 配置。
	//
	// 它**只能**出现在写路径（Active）与配置展示上：读/删按记录路由，
	// 后端不可用时是 ErrBackendGone。写路径遇到它就拒绝写入（503），
	// 绝不回落本地磁盘 —— 否则「配置丢了」会静默变成「文件写在容器里」，
	// 一次重建全部丢失，而管理台看起来一切正常。
	ErrNotConfigured = errors.New("storage: 尚未配置存储位置")
)

// IsTimeout 判断错误是否属于超时类（含单次读空闲超时与 context 超时）。
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTimeout) || errors.Is(err, ErrObjectReadTimeout) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isNotExistErr 把「对象不存在」的各种底层形态归一（本地盘 os.IsNotExist / 驱动哨兵）。
func isNotExistErr(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrObjectNotFound)
}

// backendGonef 构造「后端无法构建」错误，统一带上后端名与原因。
//
// 同时包住原始 error（Go 1.20+ 多 %w）：调用方既能用 errors.Is(err, ErrBackendGone)
// 分类，也保留 errors.Is(err, ErrUnavailable) 等底层判据。
func backendGonef(err error, format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrBackendGone, err, fmt.Sprintf(format, args...))
}

// quoteName 让错误串里的后端名可辨认（空串会显示成 ""而不是看不出问题）。
func quoteName(name string) string { return `"` + strings.TrimSpace(name) + `"` }

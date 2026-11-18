// Package releaseupdate 是一键更新的**指令与进度**唯一写入/读取点。
//
// 职责边界（很重要）：
//   - 它不执行任何命令，也不知道 docker/git 的存在；
//   - 它只维护"有没有一条更新指令、走到哪一步、结果如何"；
//   - 真正执行的是宿主机上的 ld-updater（deploy/agent），它直接读写这张表。
//
// 为什么状态必须落 PostgreSQL：更新会替换 backend 自己。若状态在进程内或 Redis，
// 新容器起来后无法回答"上次更新到哪一步了"，管理员会看到一个空白弹窗，
// 而机器可能正停在切换中途。
package releaseupdate

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/service/audit"
)

// 状态机（与 deploy/agent/ld-updater.sh 的写入值逐字对应）。
const (
	StatusQueued       = "queued"
	StatusFetching     = "fetching"
	StatusBuilding     = "building"
	StatusBackingUp    = "backing_up"
	StatusSwitching    = "switching"
	StatusVerifying    = "verifying"
	StatusSuccess      = "success"
	StatusFailed       = "failed"
	StatusRollingBack  = "rolling_back"
	StatusRolledBack   = "rolled_back"
	StatusManualNeeded = "manual_required"
)

// ActiveStatuses 是"占用全局更新位"的状态集合。
//
// 它同时出现在两处，必须逐字一致：
//  1. db/indexes.go 的部分唯一索引谓词（数据库兜底）；
//  2. 本文件的 IsActive（应用层判断）。
//
// 改一处不改另一处，症状是"库里能插两条 active"或"应用认为没在跑但插不进去"。
var ActiveStatuses = []string{
	StatusQueued, StatusFetching, StatusBuilding, StatusBackingUp,
	StatusSwitching, StatusVerifying, StatusRollingBack,
}

// 终态：不再变化，也不再占位。
var terminalStatuses = map[string]bool{
	StatusSuccess:      true,
	StatusFailed:       true,
	StatusRolledBack:   true,
	StatusManualNeeded: true,
}

// 超时与窗口。数值都有具体理由，不要当成"经验值"随手改。
const (
	// queuedTimeout：agent 每 5s 轮询一次。60s 还没被领走，就一定是"没装/没起/连不上库"。
	// 没有这条，一行 queued 会永久占住唯一索引 —— 之后每次点更新都是 409，只能改库恢复。
	queuedTimeout = 60 * time.Second
	// heartbeatTimeout：agent 的心跳是**独立子进程**每 10s 刷一次，构建期间不停。
	// 90s = 连丢 9 次，足以排除"宿主机负载高导致一次卡顿"。
	heartbeatTimeout = 90 * time.Second
	// switchTimeout：切换要停旧容器、起新容器，正常几十秒；给 3 分钟。
	// 超过它仍是 switching，说明进程在切换中途死了 —— 容器可能停在新旧之间，
	// 这种情况**不能**当 failed 让人再点一次，必须转人工。
	switchTimeout = 180 * time.Second
	// recentFailureWindow：同一个 tag 连续失败时的防抖窗口。
	recentFailureWindow = 10 * time.Minute
	// retention：任务表只留最近 200 条（每次提交时顺手清理）。
	retention = 200
)

// IsActive 报告状态是否占用全局更新位。
func IsActive(status string) bool {
	for _, s := range ActiveStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// IsTerminal 报告状态是否是终态。
func IsTerminal(status string) bool { return terminalStatuses[status] }

// 业务错误（由 controller 映射成 4xx）。
var (
	ErrInProgress     = errors.New("releaseupdate: 已有更新在进行中")
	ErrFailedRecently = errors.New("releaseupdate: 同一版本刚失败过")
	ErrNotCancellable = errors.New("releaseupdate: 当前状态不可取消")
	ErrNotFound       = errors.New("releaseupdate: 任务不存在")
)

// SubmitInput 是一次更新提交（已完成格式与策略校验）。
type SubmitInput struct {
	RequestID    string
	TargetTag    string
	TargetCommit string
	FromVersion  string
	AdminID      uint64
	// Force 跳过"同版本 10 分钟内失败过"的防抖。降级判断在 controller 用同一个开关。
	Force bool
}

// Service 是这张表的唯一读写点。
type Service struct{ gdb *gorm.DB }

func New(gdb *gorm.DB) *Service { return &Service{gdb: gdb} }

// Submit 创建一条更新指令。
//
// 幂等：同一 request_id 重复提交返回**原来那一行**（网络重试/连点都安全）。
// 并发：靠部分唯一索引收敛，不靠"先查再插"（那在并发下会两条都通过）。
func (s *Service) Submit(ctx context.Context, in SubmitInput) (*model.ReleaseUpdateJob, error) {
	if err := s.Sweep(ctx); err != nil {
		return nil, err
	}

	// 先按 request_id 查：重试请求不该被下面的"已有进行中"拦下来。
	if job, err := s.byRequestID(ctx, in.RequestID); err != nil {
		return nil, err
	} else if job != nil {
		return job, nil
	}

	if !in.Force {
		var n int64
		err := s.gdb.WithContext(ctx).Model(&model.ReleaseUpdateJob{}).
			Where("target_tag = ? AND status IN ? AND finished_at > ?",
				in.TargetTag, []string{StatusFailed, StatusRolledBack},
				time.Now().Add(-recentFailureWindow)).
			Count(&n).Error
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, ErrFailedRecently
		}
	}

	job := &model.ReleaseUpdateJob{
		RequestID:    in.RequestID,
		TargetTag:    in.TargetTag,
		TargetCommit: in.TargetCommit,
		FromVersion:  in.FromVersion,
		Status:       StatusQueued,
		Step:         "queued",
		Progress:     0,
		Message:      "已提交，等待更新程序领取",
		RequestedBy:  in.AdminID,
		RequestedAt:  time.Now(),
	}
	if err := s.gdb.WithContext(ctx).Create(job).Error; err != nil {
		if pgerr.IsUnique(err) {
			// 两种可能：并发同 request_id（返回已有行）或已有进行中的任务（409）。
			if old, e := s.byRequestID(ctx, in.RequestID); e == nil && old != nil {
				return old, nil
			}
			return nil, ErrInProgress
		}
		return nil, err
	}

	s.trim(ctx)
	return job, nil
}

// Sweep 把"卡住的任务"收敛到终态。**惰性执行**：只在读/写路径调用，不常驻循环。
//
// 三条规则各有不同的落点，不能合并：
//   - queued 超时 → failed(agent_offline)：没装/没起更新程序，允许人再点；
//   - 其它非切换态心跳超时 → failed(agent_lost)：进程死了，容器可能没动，允许再点；
//   - switching 心跳超时 → manual_required：容器可能停在"旧已停、新未起"，**不能自动重试**。
func (s *Service) Sweep(ctx context.Context) error {
	db := s.gdb.WithContext(ctx)
	now := time.Now()

	if err := db.Model(&model.ReleaseUpdateJob{}).
		Where("status = ? AND requested_at < ?", StatusQueued, now.Add(-queuedTimeout)).
		Updates(map[string]any{
			"status":       StatusFailed,
			"step":         "failed",
			"progress":     100,
			"failure_code": "agent_offline",
			"message":      "更新程序未响应（未安装或未启动），本次已作废，可重新提交",
			"finished_at":  now,
		}).Error; err != nil {
		return err
	}

	if err := db.Model(&model.ReleaseUpdateJob{}).
		Where("status IN ? AND COALESCE(last_heartbeat_at, started_at, requested_at) < ?",
			[]string{StatusFetching, StatusBuilding, StatusBackingUp, StatusVerifying, StatusRollingBack},
			now.Add(-heartbeatTimeout)).
		Updates(map[string]any{
			"status":       StatusFailed,
			"step":         "failed",
			"progress":     100,
			"failure_code": "agent_lost",
			"message":      "更新程序失去联系，本次已中止；容器未被切换",
			"finished_at":  now,
		}).Error; err != nil {
		return err
	}

	return db.Model(&model.ReleaseUpdateJob{}).
		Where("status = ? AND COALESCE(last_heartbeat_at, started_at, requested_at) < ?",
			StatusSwitching, now.Add(-switchTimeout)).
		Updates(map[string]any{
			"status":       StatusManualNeeded,
			"step":         "interrupted",
			"progress":     100,
			"failure_code": "interrupted_during_switch",
			"message":      "在切换容器期间失去联系：服务可能停在新旧之间，请按 runbook 人工确认",
			"finished_at":  now,
		}).Error
}

// Current 返回"进行中的任务"与"最近一次终态任务"。顺带做惰性判死与补写审计。
func (s *Service) Current(ctx context.Context) (active, last *model.ReleaseUpdateJob, err error) {
	if err := s.Sweep(ctx); err != nil {
		return nil, nil, err
	}
	var rows []model.ReleaseUpdateJob
	if err := s.gdb.WithContext(ctx).
		Order("id DESC").Limit(20).Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	for i := range rows {
		r := &rows[i]
		if active == nil && IsActive(r.Status) {
			active = r
		}
		if last == nil && IsTerminal(r.Status) {
			last = r
		}
	}
	if last != nil {
		s.auditTerminal(ctx, last)
	}
	return active, last, nil
}

// Get 按 id 取一条任务。
func (s *Service) Get(ctx context.Context, id uint64) (*model.ReleaseUpdateJob, error) {
	var job model.ReleaseUpdateJob
	if err := s.gdb.WithContext(ctx).First(&job, id).Error; err != nil {
		return nil, ErrNotFound
	}
	return &job, nil
}

// Cancel 只能取消 queued：进入执行之后"取消"意味着要杀掉正在构建/切换的进程，
// 那是另一种事故，留给运维按 runbook 处理。
func (s *Service) Cancel(ctx context.Context, id uint64) error {
	res := s.gdb.WithContext(ctx).Model(&model.ReleaseUpdateJob{}).
		Where("id = ? AND status = ?", id, StatusQueued).
		Updates(map[string]any{
			"status":       StatusFailed,
			"step":         "cancelled",
			"progress":     100,
			"failure_code": "cancelled",
			"message":      "已被管理员取消",
			"finished_at":  time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 分清"不存在"与"不可取消"，否则前端只能说"失败"。
		if _, err := s.Get(ctx, id); err != nil {
			return err
		}
		return ErrNotCancellable
	}
	return nil
}

// AgentState 返回宿主机更新程序的心跳（从未上报过时返回 nil, nil）。
func (s *Service) AgentState(ctx context.Context) (*model.ReleaseAgentState, error) {
	var st model.ReleaseAgentState
	err := s.gdb.WithContext(ctx).Where("id = ?", 1).First(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// RunningTasks 返回当前在途任务数（给确认弹窗用：让人知道"现在更新会打断多少活"）。
func (s *Service) RunningTasks(ctx context.Context) (int64, error) {
	var n int64
	err := s.gdb.WithContext(ctx).Model(&model.Task{}).
		Where("status IN ?", []string{"queued", "running"}).
		Count(&n).Error
	return n, err
}

func (s *Service) byRequestID(ctx context.Context, requestID string) (*model.ReleaseUpdateJob, error) {
	if requestID == "" {
		return nil, nil
	}
	var job model.ReleaseUpdateJob
	err := s.gdb.WithContext(ctx).Where("request_id = ?", requestID).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// trim 保留最近 retention 条。不做定时任务：写入频率极低，顺手删最省事。
func (s *Service) trim(ctx context.Context) {
	var ids []uint64
	if err := s.gdb.WithContext(ctx).Model(&model.ReleaseUpdateJob{}).
		Order("id DESC").Offset(retention).Limit(200).Pluck("id", &ids).Error; err != nil || len(ids) == 0 {
		return
	}
	_ = s.gdb.WithContext(ctx).Where("id IN ?", ids).
		Delete(&model.ReleaseUpdateJob{}).Error
}

// auditTerminal 在**读路径**补写结果审计，用 result_audited_at 保证只写一次。
//
// 为什么由后端写而不是 agent 写：admin_audit 的唯一写入口是 audit.Record（含
// 动作白名单校验）。让 agent 用 psql 直接插行等于绕过白名单，第二份写入路径。
func (s *Service) auditTerminal(ctx context.Context, job *model.ReleaseUpdateJob) {
	if job.ResultAuditedAt != nil {
		return
	}
	detail := map[string]any{
		"job_id":     job.ID,
		"target_tag": job.TargetTag,
		"status":     job.Status,
	}
	if job.FailureCode != "" {
		detail["failure_code"] = job.FailureCode
	}
	if job.RollbackResult != "" {
		detail["rollback_result"] = job.RollbackResult
	}
	audit.RecordOrWarn(s.gdb, job.RequestedBy, audit.ActionReleaseUpdateResult,
		"release_update", strings.TrimSpace(job.TargetTag), detail)

	now := time.Now()
	if err := s.gdb.WithContext(ctx).Model(&model.ReleaseUpdateJob{}).
		Where("id = ? AND result_audited_at IS NULL", job.ID).
		Update("result_audited_at", now).Error; err == nil {
		job.ResultAuditedAt = &now
	}
}

package request

import (
	"fmt"

	"github.com/tmnhs/crony/common/pkg/runpolicy"
)

// 阶段 1（任务模型 2.0）：执行记录查询入参。
//
// 与旧 /job/log 的区别：job_run 是"一次执行"的权威记录，
// 带幂等键、触发来源、策略快照、重试次数与步骤明细，是 AI Agent 后续
// "读执行历史 → 推断根因"的数据底座（job_log 继续保留兼容旧前端）。
type (
	ReqJobRunSearch struct {
		PageInfo
		RunID    string `json:"run_id" form:"run_id"`
		JobID    int    `json:"job_id" form:"job_id"`
		JobName  string `json:"job_name" form:"job_name"`
		NodeUUID string `json:"node_uuid" form:"node_uuid"`
		// Status 执行状态（0..6，见 models.RunStatus）；nil 表示不过滤
		Status *int `json:"status" form:"status"`
		// TriggerSource 触发来源（1=scheduler 2=manual 3=retry 4=api 5=agent 6=self_heal）
		TriggerSource *int `json:"trigger_source" form:"trigger_source"`
		// StartTime / EndTime 按发起时间过滤（Unix 秒，左闭右开）
		StartTime int64 `json:"start_time" form:"start_time"`
		EndTime   int64 `json:"end_time" form:"end_time"`
		// FailedOnly 只看失败（含 failed/timeout/canceled），用于"最近失败"面板
		FailedOnly bool `json:"failed_only" form:"failed_only"`
	}
	ReqJobRunFind struct {
		// RunID 与 JobID 二选一：给了 run_id 精确查一次执行，否则取该任务最近一次
		RunID string `json:"run_id" form:"run_id"`
		JobID int    `json:"job_id" form:"job_id"`
	}
	ReqJobRunSteps struct {
		PageInfo
		RunID string `json:"run_id" form:"run_id" binding:"required"`
	}
)

func (r *ReqJobRunSearch) Check() {
	r.PageInfo.Check()
	// 防止 PageSize 被放大成拖库查询
	if r.PageSize > 200 {
		r.PageSize = 200
	}
}

func (r *ReqJobRunSteps) Check() {
	r.PageInfo.Check()
	if r.PageSize > 500 {
		r.PageSize = 500
	}
}

// ValidConcurrencyPolicy 校验并发策略取值。
//
// 为什么在入口就拒绝：节点侧对非法值会回退成 skip，用户会看到"我配了并发策略
// 但完全没生效"这种极难排查的现象；宁可保存时报错。
func ValidConcurrencyPolicy(policy int) error {
	switch runpolicy.ConcurrencyPolicy(policy) {
	case runpolicy.ConcurrencySkip, runpolicy.ConcurrencyBufferOne, runpolicy.ConcurrencyBufferAll,
		runpolicy.ConcurrencyCancelOther, runpolicy.ConcurrencyTerminateOther:
		return nil
	default:
		return fmt.Errorf("非法的并发策略取值: %d（合法值 0=skip 1=buffer_one 2=buffer_all 3=cancel_other 4=terminate_other）", policy)
	}
}

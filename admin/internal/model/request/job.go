package request

import (
	"encoding/json"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/cronspec"
)

type (
	ReqJobSearch struct {
		PageInfo
		ID    int            `json:"id" form:"id"`
		Name  string         `json:"name" form:"name"`
		RunOn string         `json:"run_on" form:"run_on"`
		Type  models.JobType `json:"job_type" form:"type"`
		// Status 用指针区分"不筛选"与"筛选未分配(status=0)"：
		// 未分配就是 0，用 int 时 0 会被当成"没传"，导致该筛选条件永远失效。
		Status *int `json:"status" form:"status"`
	}
	ReqJobLogSearch struct {
		PageInfo
		Name     string `json:"name" form:"name"`
		JobId    int    `json:"job_id" form:"job_id"`
		NodeUUID string `json:"node_uuid" form:"node_uuid"`
		Success  *bool  `json:"success" form:"success"`
	}
	ReqJobUpdate struct {
		*models.Job
		Allocation int `json:"allocation" form:"allocation" binding:"required"`
	}
	ReqJobOnce struct {
		JobId    int    `json:"job_id" form:"job_id"`
		NodeUUID string `json:"node_uuid" form:"node_uuid"`
	}
	ReqJobKill struct {
		JobId    int    `json:"job_id" form:"job_id"`
		NodeUUID string `json:"node_uuid" form:"node_uuid"`
	}
)

func (r *ReqJobUpdate) Valid() error {
	// default automatic assignment
	if r.Allocation == 0 {
		r.Allocation = models.AutoAllocation
	}
	// 阶段 1：并发策略必须在枚举范围内（节点侧对非法值会静默回退成 skip，
	// 在入口拦下可以避免"配置了但没生效"的困惑）
	if err := ValidConcurrencyPolicy(r.ConcurrencyPolicy); err != nil {
		return err
	}
	// cron 表达式必须在**写入**这一层拦住段数问题：本系统调度库把 5 段解释为
	// 「秒 分 时 日 月」，于是 "0 3 * * *"（本意每天 3 点）实际是"每小时第 3 分钟"，
	// 而且**不报错**——只在运行频率上错。这是运维在真集群上踩过的坑
	// （4 个 e2e 任务照抄控制台默认值 5 段 → 每小时重跑 → 污染成功率与 SLO 指标）。
	// 详见 common/pkg/cronspec 顶部注释。存量 5 段任务不受影响（调度侧只告警不改行为）。
	if err := cronspec.Validate(r.Spec); err != nil {
		return err
	}
	notifyTo, _ := json.Marshal(r.NotifyToArray)
	r.NotifyTo = notifyTo
	scriptID, _ := json.Marshal(r.ScriptIDArray)
	r.ScriptID = scriptID
	return r.Check()
}

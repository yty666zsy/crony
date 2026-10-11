package request

import (
	"encoding/json"
	"github.com/tmnhs/crony/common/models"
)

type (
	ReqJobSearch struct {
		PageInfo
		ID     int            `json:"id" form:"id"`
		Name   string         `json:"name" form:"name"`
		RunOn  string         `json:"run_on" form:"run_on"`
		Type   models.JobType `json:"job_type" form:"type"`
		Status int            `json:"status" form:"status"`
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
	notifyTo, _ := json.Marshal(r.NotifyToArray)
	r.NotifyTo = notifyTo
	scriptID, _ := json.Marshal(r.ScriptIDArray)
	r.ScriptID = scriptID
	return r.Check()
}

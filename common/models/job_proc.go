package models

import (
	"encoding/json"
	"sync"
	"time"
)

type JobProcVal struct {
	Time   time.Time `json:"time"`   // 开始执行时间
	Killed bool      `json:"killed"` // 是否强制杀死
	// RunID 阶段 1 新增：该执行标记对应的执行记录 ID。
	// 取消时优先取消这个 run；为空（老节点/老数据）时退化为按 job_id 取消该任务全部执行。
	RunID string `json:"run_id,omitempty"`
	// NodePID 阶段 1 新增：节点进程自身的 OS pid，仅用于排查；
	// 被执行的子进程 pid 记录在执行器 artifacts 中（shell 执行器会写入 pid）。
	NodePID int `json:"node_pid,omitempty"`
}

type JobProc struct {
	ID       int    `json:"id"` // 执行槽位编号（历史字段名沿用 pid）
	JobID    int    `json:"job_id"`
	NodeUUID string `json:"node_uuid"`
	JobProcVal

	Running int32
	Wg      sync.WaitGroup
}

func (p *JobProc) Val() (string, error) {
	b, err := json.Marshal(&p.JobProcVal)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

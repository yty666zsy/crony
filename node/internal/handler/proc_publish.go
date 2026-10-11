package handler

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/logger"
)

// ProcPublisher 在执行期间公布/撤销"该任务正在本节点执行"的标记。
//
// 为什么还需要它（阶段 1 已经改成 context 取消）：
//   - admin 的 /job/kill 是**通过 etcd 上的 /crony/proc 键**找到待取消目标的，
//     没有键它就只会返回 "don't have such process"，kill 依然是个死接口；
//   - 运维面板的"当前正在执行"计数也读 /crony/proc 前缀。
//
// 与旧实现的区别：键里的数字段不再是 OS pid（可能是 shell 脚本，
// 真正的子进程 pid 在执行器的 artifacts 里），而是进程内自增的执行槽位编号；
// 键值里的 run_id 才是取消的精准目标。取消不再向 pid 发信号，
// 而是让执行器终止整棵进程树（Unix 进程组 / Windows taskkill /T）。
type ProcPublisher interface {
	// Publish 公布标记，返回撤销函数（可为 nil）。
	Publish(jobID int, nodeUUID, runID string) func()
}

// etcdProcPublisher 生产实现。
type etcdProcPublisher struct{}

// procSlotSeq 进程内自增的执行槽位编号（仅用于构造唯一 etcd 键）。
var procSlotSeq int64

func (etcdProcPublisher) Publish(jobID int, nodeUUID, runID string) func() {
	slot := int(atomic.AddInt64(&procSlotSeq, 1))
	proc := &JobProc{
		JobProc: &models.JobProc{
			ID:       slot,
			JobID:    jobID,
			NodeUUID: nodeUUID,
			JobProcVal: models.JobProcVal{
				Time:    time.Now(),
				RunID:   runID,
				NodePID: os.Getpid(),
			},
		},
	}
	if err := proc.Start(); err != nil {
		// 标记只是"可被取消/可被观测"的辅助手段，写失败不影响任务执行本身
		logger.GetLogger().Warn(fmt.Sprintf("job[%d] run[%s] 公布执行标记失败（不影响执行，但 /job/kill 可能找不到目标）: %s",
			jobID, runID, err.Error()))
		return nil
	}
	return proc.Stop
}

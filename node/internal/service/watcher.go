package service

import (
	"encoding/json"
	"fmt"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/etcdclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/utils"
	"github.com/tmnhs/crony/node/internal/handler"
	"strings"
)

func (srv *NodeServer) watchJobs() {
	rch := handler.WatchJobs(srv.UUID)
	for wresp := range rch {
		for _, ev := range wresp.Events {
			switch {
			case ev.IsCreate():
				var job handler.Job
				if err := json.Unmarshal(ev.Kv.Value, &job); err != nil {
					err = fmt.Errorf("watch job[%s] create json umarshal err: %s", string(ev.Kv.Key), err.Error())
					continue
				}
				srv.setJob(job.ID, &job)
				job.InitNodeInfo(models.JobStatusAssigned, srv.UUID, srv.Hostname, srv.IP)
				srv.addJob(&job)
			case ev.IsModify():
				var job handler.Job
				if err := json.Unmarshal(ev.Kv.Value, &job); err != nil {
					err = fmt.Errorf("watch job[%s] modify json umarshal err: %s", string(ev.Kv.Key), err.Error())
					continue
				}
				job.InitNodeInfo(models.JobStatusAssigned, srv.UUID, srv.Hostname, srv.IP)
				srv.modifyJob(&job)
			case ev.Type == mvccpb.DELETE:
				srv.deleteJob(handler.GetJobIDFromKey(string(ev.Kv.Key)))
			default:
				logger.GetLogger().Warn(fmt.Sprintf("watch job unknown event type[%v] from job[%s]", ev.Type, string(ev.Kv.Key)))
			}
		}
	}
}

// watchKilledProc 处理 /job/kill：admin 把 /crony/proc/<uuid>/<job_id>/<pid> 的值改成
// {killed:true} 后，节点侧把该任务**正在进行的执行**全部取消。
//
// 阶段 1 的实现变化（旧实现的问题）：
//   - 旧代码 `syscall.Kill(proc.ID, syscall.SIGKILL)` 只杀直接子进程，且 Windows 上
//     syscall.Kill 不可用（该函数之前因此被整段注释掉，kill 接口实际失效）；
//   - 现在按 job_id 取消 context，由 executor 终止**整棵进程树**
//     （Unix: kill(-pgid)；Windows: taskkill /T /F），并在 job_run 里落 canceled 状态。
func (srv *NodeServer) watchKilledProc() {
	rch := handler.WatchProc(srv.UUID)
	for wresp := range rch {
		for _, ev := range wresp.Events {
			switch {
			case ev.IsModify(), ev.IsCreate():
				proc, err := handler.GetProcFromKey(string(ev.Kv.Key))
				if err != nil {
					logger.GetLogger().Error(fmt.Sprintf("watch killed proc error:%s kv:%s", err.Error(), ev.Kv.String()))
					continue
				}
				procVal := &models.JobProcVal{}
				if err := json.Unmarshal(ev.Kv.Value, procVal); err != nil {
					logger.GetLogger().Warn(fmt.Sprintf("watch killed proc json warn:%s kv:%s", err.Error(), ev.Kv.String()))
					continue
				}
				proc.JobProcVal = *procVal
				if !proc.Killed {
					continue
				}
				// 优先按 run_id 精准取消本次执行；老数据没有 run_id 时退化为取消该任务全部执行
				if proc.RunID != "" && handler.DefaultRunCoordinator().CancelRun(proc.RunID) {
					logger.GetLogger().Warn(fmt.Sprintf("job[%d] run[%s] 收到 kill 指令，已取消（进程树将被终止）", proc.JobID, proc.RunID))
					continue
				}
				canceled := handler.DefaultRunCoordinator().CancelJob(proc.JobID)
				if canceled == 0 {
					logger.GetLogger().Warn(fmt.Sprintf("job[%d] 收到 kill 指令，但本节点没有正在进行的执行（可能已结束或由其它节点执行）", proc.JobID))
					continue
				}
				logger.GetLogger().Warn(fmt.Sprintf("job[%d] 收到 kill 指令，已取消 %d 个进行中的执行（进程树将被终止）", proc.JobID, canceled))
			}
		}
	}
}

func (srv *NodeServer) watchSystemInfo() {
	rch := handler.WatchSystem(srv.UUID)
	for wresp := range rch {
		for _, ev := range wresp.Events {
			switch {
			case ev.IsCreate() || ev.IsModify():
				key := string(ev.Kv.Key)
				if string(ev.Kv.Value) != models.NodeSystemInfoSwitch || srv.Node.UUID != getUUID(key) {
					logger.GetLogger().Error(fmt.Sprintf("get system info from node[%s] ,switch is not alive ", srv.UUID))
					continue
				}
				s, err := utils.GetServerInfo()
				if err != nil {
					logger.GetLogger().Error(fmt.Sprintf("get system info from node[%s] error: %s", srv.UUID, err.Error()))
					continue
				}
				b, err := json.Marshal(s)
				if err != nil {
					logger.GetLogger().Error(fmt.Sprintf("get system info from node[%s] json marshal error: %s", srv.UUID, err.Error()))
					continue
				}
				_, err = etcdclient.PutWithTtl(fmt.Sprintf(etcdclient.KeyEtcdSystemGet, getUUID(key)), string(b), 5*60)
				if err != nil {
					logger.GetLogger().Error(fmt.Sprintf("get system info from node[%s] etcd put error: %s", srv.UUID, err.Error()))
					continue
				}

			}
		}
	}
}

func getUUID(key string) string {
	// /crony/node/<node_uuid>
	index := strings.LastIndex(key, "/")
	if index == -1 {
		return ""
	}
	return key[index+1:]
}

func (srv *NodeServer) watchOnce() {
	rch := handler.WatchOnce()
	for wresp := range rch {
		for _, ev := range wresp.Events {
			switch {
			case ev.IsModify(), ev.IsCreate():
				// is not executed on this node
				if len(ev.Kv.Value) != 0 && string(ev.Kv.Value) != srv.UUID {
					continue
				}
				j, ok := srv.getJob(handler.GetJobIDFromKey(string(ev.Kv.Key)))
				if !ok {
					continue
				}
				go j.RunWithRecovery()
			}
		}
	}
}

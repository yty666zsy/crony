package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jakecoffman/cron"
	"github.com/ouqiang/goutil"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/etcdclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/utils"
	"github.com/tmnhs/crony/node/internal/handler"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type NodeServer struct {
	*etcdclient.ServerReg
	*models.Node
	*cron.Cron

	jobs handler.Jobs
}

func NewNodeServer() (*NodeServer, error) {
	// 阶段 3 修正：节点身份必须**跨重启稳定**。
	// 上游实现每次启动都生成随机 UUID，导致 job_run.node_uuid 无法归属、
	// node_metric 断线、admin→node 探测请求发给一个永不存在的节点。
	uuid, warning := LoadOrCreateNodeUUID(nodeIdentityDir())
	if warning != "" {
		logger.GetLogger().Warn("[node] " + warning)
	}
	ip, err := utils.LocalIP()
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = uuid
		err = nil
	}
	return &NodeServer{
		Node: &models.Node{
			UUID:     uuid,
			PID:      strconv.Itoa(os.Getpid()),
			IP:       ip.String(),
			Hostname: hostname,
			UpTime:   time.Now().Unix(),
			Status:   models.NodeConnSuccess,
			Version:  config.GetConfigModels().System.Version,
		},
		Cron:      cron.New(),
		jobs:      make(handler.Jobs, 8),
		ServerReg: etcdclient.NewServerReg(config.GetConfigModels().System.NodeTtl),
	}, nil

}

// registeredPID 从 etcd 节点键的值里解析出"注册进程的 PID"。
//
// 当前格式是 Register() 写入的 Node JSON（含 pid 字段）；同时兼容更早的实现
// （值直接是裸 PID 字符串）。解析不出来时返回 ok=false——**调用方绝不能据此删键**。
func registeredPID(val []byte) (pid int, ok bool) {
	var n models.Node
	if err := json.Unmarshal(val, &n); err == nil {
		if p, err := strconv.Atoi(strings.TrimSpace(n.PID)); err == nil && p > 0 {
			return p, true
		}
		return 0, false
	}
	// 兼容更早的实现：值直接是裸 PID。
	if p, err := strconv.Atoi(strings.TrimSpace(string(val))); err == nil && p > 0 {
		return p, true
	}
	return 0, false
}

// exist 判断同一 uuid 是否已有**存活**节点在 etcd 注册。
// 若在，返回其 PID；否则返回 -1。
//
// 阶段 5 收尾修正（20 节点舰队重启实测暴露）：旧实现把节点键的值按**整数 PID** 解析，
// 而 Register() 写入的是 Node JSON —— 于是解析必然失败，代码顺手**把这个节点键删掉**，
// 伪造出一次"节点失联"事件。后果是可观测且真实的：
//   - 一个**健康节点**上的任务被无谓 FailOver 到别的节点（任务白搬一次）；
//   - admin 会按下线处理并删掉该节点的库内行，而节点随即重新注册 —— 于是出现
//     "etcd 里有它、库里没有它"的漂移（节点页/统计/DB 侧调度全都看不见它）。
//
// 现在的原则：**解析失败一律不删键**（让租约自然过期或由新注册覆盖），
// 只有解析出 PID 且该进程确实不存活时才返回 -1。
func (srv *NodeServer) exist(nodeUUID string) (pid int, err error) {
	resp, err := etcdclient.Get(fmt.Sprintf(etcdclient.KeyEtcdNode, nodeUUID))
	if err != nil {
		return
	}

	if len(resp.Kvs) == 0 {
		return -1, nil
	}

	registeredPid, ok := registeredPID(resp.Kvs[0].Value)
	if !ok {
		logger.GetLogger().Warn(fmt.Sprintf(
			"[node] etcd 节点键 %s 的值无法解析出 PID，按未注册处理并**保留该键**（不删键：删键会伪造一次节点失联，误伤健康节点）",
			nodeUUID))
		return -1, nil
	}

	p, err := os.FindProcess(registeredPid)
	if err != nil {
		return -1, nil
	}

	if p != nil && p.Signal(syscall.Signal(0)) == nil {
		return registeredPid, nil
	}
	return -1, nil
}

// Register into ETCD with /crony/node/<node_id>
func (srv *NodeServer) Register() error {
	pid, err := srv.exist(srv.UUID)
	if err != nil {
		return err
	}
	if pid != -1 {
		return fmt.Errorf("node[%s] with pid[%d] exist", srv.UUID, pid)
	}
	b, err := json.Marshal(&srv.Node)
	if err != nil {
		return fmt.Errorf("node[%s] with pid[%d] json error%s", srv.UUID, pid, err.Error())
	}
	//creates a new lease
	if err := srv.ServerReg.Register(fmt.Sprintf(etcdclient.KeyEtcdNode, srv.UUID), string(b)); err != nil {
		return err
	}
	return nil
}

func (srv *NodeServer) Stop(i interface{}) {
	srv.Down()
	_, err := etcdclient.Delete(fmt.Sprintf(etcdclient.KeyEtcdNode, srv.UUID))
	if err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("failed to delete etcd node[%s] key error:%s", srv.UUID, err.Error()))
	}
	_, err = etcdclient.Delete(fmt.Sprintf(etcdclient.KeyEtcdSystemGet, srv.UUID))
	if err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("failed to delete system etcd node[%s] key error:%s", srv.UUID, err.Error()))
	}

	_ = srv.Client.Close()
	srv.Cron.Stop()
}

// Remove the survival information from mysql after the node instance is deactivated
func (srv *NodeServer) Down() {
	srv.Status = models.NodeConnFail
	srv.DownTime = time.Now().Unix()
	err := srv.Node.Update()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("failed to update  node[%s] down  error:%s", srv.UUID, err.Error()))
	}
	err = dbclient.GetMysqlDB().Table(models.CronyJobTableName).Select("status").Where("run_on = ? ", srv.UUID).Updates(models.Job{
		Status: models.JobStatusNotAssigned,
	}).Error

	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("failed to update job on node[%s] down  error:%s", srv.UUID, err.Error()))
	}
}

func (srv *NodeServer) Run() (err error) {
	defer func() {
		if err != nil {
			srv.Stop(err)
		}
	}()

	if err = srv.loadJobs(); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("node[%s] failed to load job error:%s", srv.UUID, err.Error()))
		return
	}
	// 阶段 2：装配 Agent 运行时（未配置 llm 时只告警，不影响普通任务）。
	srv.initAgentRuntime()
	// 按 uuid 就地登记：重启时复用同一行（Update），避免同一节点在库里出现多行；
	// 若该行已被清理（例如真失联后 admin 做过 FailOver 清理），则重新插入。
	// 旧实现无条件 Insert()，重启一次就多一行（节点页重复、状态更新写到最旧那行上）。
	insertId, err := srv.Node.Upsert()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("failed to create node[%s] into db error:%s", srv.UUID, err.Error()))
		return
	}
	srv.Node.ID = insertId
	//start cron
	srv.Cron.Start()
	go srv.watchJobs()
	// 阶段 1 恢复：/job/kill 现在通过 context 取消 + 进程树终止实现（见 watcher.go）
	go srv.watchKilledProc()
	go srv.watchOnce()
	go srv.watchSystemInfo()
	// 阶段 3 收尾：资源水位上报。没有它，admin 的 node_status 工具永远只能
	// 输出"资源水位=未上报"（读的是 node_metric 表，而此前没人往里写）。
	go srv.StartMetricPublisher(context.Background())
	return
}

func (srv *NodeServer) loadJobs() (err error) {
	defer func() {
		//When an outage occurs, get the context passed by panic and print it
		if r := recover(); r != nil {
			logger.GetLogger().Error(fmt.Sprintf("load jobs panic:%v", r))
		}
	}()
	// Obtain the scheduled job assigned by the local server
	jobs, err := handler.GetJobs(srv.UUID)
	if err != nil {
		return
	}

	if len(jobs) == 0 {
		return
	}
	srv.jobs = jobs

	for _, j := range jobs {
		j.InitNodeInfo(models.JobStatusAssigned, srv.UUID, srv.Hostname, srv.IP)
		srv.addJob(j)
	}

	return
}

func (srv *NodeServer) addJob(j *handler.Job) {
	if err := j.Check(); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("job[%d] check error :%s", j.ID, err.Error()))
		return
	}
	//run script
	if j.Type == models.JobTypeCmd {
		for _, id := range j.ScriptIDArray {
			script := &models.Script{ID: id}
			err := script.FindById()
			if err != nil {
				logger.GetLogger().Warn(fmt.Sprintf("job[%d] find script[%d] error :%s", j.ID, id, err.Error()))
				continue
			}
			err = script.Check()
			if err != nil {
				logger.GetLogger().Warn(fmt.Sprintf("script[%d] check error :%s", id, err.Error()))
				continue
			}
			result, err := handler.RunPresetScript(script)
			if err != nil {
				logger.GetLogger().Warn(fmt.Sprintf("job[%d] run script[%d] error :%s", j.ID, id, err.Error()))
			}
			logger.GetLogger().Info(fmt.Sprintf("job[%d] run script[%d] result :%s", j.ID, id, result))
		}
	}
	taskFunc := handler.CreateJob(j)
	if taskFunc == nil {
		logger.GetLogger().Error(fmt.Sprintf("Failed to create a task to process the Job. The task protocol was not supported:%v", j.Type))
		return
	}
	err := goutil.PanicToError(func() {
		srv.Cron.AddFunc(j.Spec, taskFunc, srv.jobCronName(j.ID))
	})
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("Failed to add a task to the scheduler#%v", err.Error()))
	}

	return
}
func (srv *NodeServer) jobCronName(jobId int) string {
	return fmt.Sprintf(srv.UUID+"/%d", jobId)
}

func (srv *NodeServer) modifyJob(j *handler.Job) {
	oldJob, ok := srv.jobs[j.ID]
	if !ok {
		srv.addJob(j)
		return
	}
	srv.deleteJob(oldJob.ID)
	srv.addJob(j)
	return
}

func (srv *NodeServer) deleteJob(jobId int) {
	if _, ok := srv.jobs[jobId]; ok {
		srv.Cron.RemoveJob(srv.jobCronName(jobId))
		delete(srv.jobs, jobId)
		return
	}
	return
}

package handler

import (
	"encoding/json"
	"fmt"
	"github.com/coreos/etcd/clientv3"
	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/etcdclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"time"
)

type JobRouter struct {
}

var defaultJobRouter = new(JobRouter)

func (j *JobRouter) CreateOrUpdate(c *gin.Context) {
	var req request.ReqJobUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[create_job] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[create_job] request parameter error", c)
		return
	}
	if err := req.Valid(); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("create_job check error:%s", err.Error()))
		// 返回具体校验原因（例如"非法的并发策略取值: 9"）：
		// 只回 "check error" 会让调用方（含前端控制台）无法定位是哪一项参数不合法
		resp.FailWithDetailed(resp.ErrorJobFormat, map[string]interface{}{},
			"[create_job] check error: "+err.Error(), c)
		return
	}
	// 阶段 4-B：编排定义必须在这里就被拒绝（含成环、依赖不存在、未知字段）。
	// 理由：一份"存得下但节点执行不了"的定义，只会在最需要它跑的那一刻暴露。
	if !validateJobPlanOrReply(req.Job, c) {
		return
	}

	var err error
	var insertId int
	t := time.Now()

	if req.Allocation == models.AutoAllocation {
		if !config.GetConfigModels().System.CmdAutoAllocation && req.Type == models.JobTypeCmd {
			resp.FailWithMessage(resp.ERROR, "[create_job] The shell command is not supported to automatically assign nodes by default.", c)
			return
		}
		// 与"节点接入分配 / 失联迁移"统一走 4-D 调度器
		// （打分优先、旧策略兜底、兜底不越过硬约束）。
		//
		// 此前这里是裸 AutoAllocateNode（任务数最少）：带 node_selector 硬约束的任务
		// 在**创建**时就完全不受约束，会被随手派到一台装不了它的机器上（F3）。
		// 创建是最常见的分配入口，这里不统一，F3 就只是被修了一半。
		nodeUUID := service.DefaultJobService.AutoAllocateNodeFor(req.Job)
		if nodeUUID == "" {
			if service.DefaultSchedulerService.HasHardRequirement(req.Job) {
				// 明确区分"集群里没有可用节点"和"没有满足硬约束的节点"：
				// 后者是可行动的（上线带相应标签/能力的节点，或改手动指定），
				// 含糊的 auto allocate node error 会让人去查集群，其实是任务声明的问题。
				logger.GetLogger().Warn(fmt.Sprintf("[create_job] no node satisfies hard requirement (node_selector=%s)", req.NodeSelector))
				resp.FailWithMessage(resp.ERROR,
					"[create_job] 没有满足该任务硬约束(node_selector)的节点，无法自动分配：请先上线具备相应标签/能力的节点，或改为手动指定节点", c)
				return
			}
			logger.GetLogger().Error(fmt.Sprintf("[create_job] auto allocate node error"))
			resp.FailWithMessage(resp.ERROR, "[create_job] auto allocate node error", c)
			return
		}
		req.RunOn = nodeUUID
	} else if req.Allocation == models.ManualAllocation {
		// Manual assignment
		if len(req.RunOn) == 0 {
			resp.FailWithMessage(resp.ERROR, "[create_job] manually assigned node can't be null", c)
			return
		}
		node := &models.Node{UUID: req.RunOn}
		if err = node.FindByUUID(); err != nil {
			// 原实现忽略该错误（`_ = node.FindByUUID()`），节点不存在时 Status 是零值，
			// 不等于 NodeConnFail，检查会被绕过 → 任务被派发到"幽灵节点"。
			logger.GetLogger().Error(fmt.Sprintf("[create_job] find node[%s] error:%s", req.RunOn, err.Error()))
			resp.FailWithMessage(resp.ERROR, "[create_job] manually assigned node not exist", c)
			return
		}
		if node.Status == models.NodeConnFail {
			resp.FailWithMessage(resp.ERROR, "[create_job] manually assigned node inactivation", c)
			return
		}
	}
	if req.ID > 0 {
		//update
		job := &models.Job{ID: req.ID}
		_ = job.FindById()
		oldNodeUUID := job.RunOn
		if oldNodeUUID != "" {
			_, err = etcdclient.Delete(fmt.Sprintf(etcdclient.KeyEtcdJob, oldNodeUUID, req.ID))
			if err != nil {
				logger.GetLogger().Error(fmt.Sprintf("[update_job] delete etcd node[%s]  error:%s", oldNodeUUID, err.Error()))
				resp.FailWithMessage(resp.ERROR, "[update_job] delete etcd node error", c)
				return
			}
		}
		req.Updated = t.Unix()
		err = req.Update()
		if err != nil {
			logger.GetLogger().Error(fmt.Sprintf("[update_job] into db  error:%s", err.Error()))
			resp.FailWithMessage(resp.ERROR, "[update_job] into db id error", c)
			return
		}
	} else {
		//create
		req.Created = t.Unix()
		insertId, err = req.Insert()
		if err != nil {
			logger.GetLogger().Error(fmt.Sprintf("[create_job] insert job into db error:%s", err.Error()))
			resp.FailWithMessage(resp.ERROR, "[create_job] insert job into db error", c)
			return
		}
		req.ID = insertId
	}
	b, err := json.Marshal(req)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[create_job] json marshal job error:%s", err.Error()))
		resp.FailWithMessage(resp.ERROR, "[create_job] json marshal job error", c)
		return
	}
	_, err = etcdclient.Put(fmt.Sprintf(etcdclient.KeyEtcdJob, req.RunOn, req.ID), string(b))
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[create_job] etcd put job error:%s", err.Error()))
		resp.FailWithMessage(resp.ERROR, "[create_job] etcd put job error", c)
		return
	}

	resp.OkWithDetailed(req, "operate success", c)
	if planLog := marshalPlanForLog(req.Job); planLog != "" {
		// 保存成功时把编排结构记一条日志：出问题时可以回答"当时存进去的到底是哪张图"。
		logger.GetLogger().Info(fmt.Sprintf("[create_or_update_job] job[%d] orchestration: %s", req.ID, planLog))
	}
}

func (j *JobRouter) Delete(c *gin.Context) {
	var req request.ByIDS
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[delete_job] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[delete_job] request parameter error", c)
		return
	}
	for _, id := range req.IDS {
		job := models.Job{ID: id}
		err := job.FindById()
		if err != nil {
			logger.GetLogger().Error(fmt.Sprintf("[delete_job] find job by id :%d error:%s", id, err.Error()))
			continue
		}
		_, err = etcdclient.Delete(fmt.Sprintf(etcdclient.KeyEtcdJob, job.RunOn, id))
		if err != nil {
			logger.GetLogger().Error(fmt.Sprintf("[delete_job] etcd delete job error:%s", err.Error()))
			continue
		}
		err = job.Delete()
		if err != nil {
			logger.GetLogger().Error(fmt.Sprintf("[delete_job] into db error:%s", err.Error()))
			continue
		}
	}
	resp.OkWithMessage("delete success", c)
}

func (j *JobRouter) FindById(c *gin.Context) {
	var req request.ByID
	if err := c.ShouldBindQuery(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[find_job] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[find_job] request parameter error", c)
		return
	}
	job := models.Job{ID: req.ID}
	err := job.FindById()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[find_job] find job by id :%d error:%s", req.ID, err.Error()))
		resp.FailWithMessage(resp.ERROR, "[find_job] find job by id error", c)
		return
	}
	if len(job.NotifyTo) != 0 {
		_ = job.Unmarshal()
	}
	resp.OkWithDetailed(job, "find success", c)
}

func (j *JobRouter) Search(c *gin.Context) {
	var req request.ReqJobSearch
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[search_job] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[search_job] request parameter error", c)
		return
	}
	req.Check()
	jobs, total, err := service.DefaultJobService.Search(&req)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[search_job] search job error:%s", err.Error()))
		resp.FailWithMessage(resp.ERROR, "[search_job] search job error", c)
		return
	}
	var resultJobs []models.Job
	for _, job := range jobs {
		_ = job.Unmarshal()
		resultJobs = append(resultJobs, job)
	}
	resp.OkWithDetailed(resp.PageResult{
		List:     resultJobs,
		Total:    total,
		Page:     req.Page,
		PageSize: req.PageSize,
	}, "search success", c)
}

func (j *JobRouter) SearchLog(c *gin.Context) {
	var req request.ReqJobLogSearch
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[search_job_log] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[search_job_log] request parameter error", c)
		return
	}
	req.Check()
	jobs, total, err := service.DefaultJobService.SearchJobLog(&req)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[search_job_log] db error:%s", err.Error()))
		resp.FailWithMessage(resp.ERROR, "[search_job_log] db error", c)
		return
	}
	resp.OkWithDetailed(resp.PageResult{
		List:     jobs,
		Total:    total,
		Page:     req.Page,
		PageSize: req.PageSize,
	}, "search success", c)
}

// execute immediately
func (j *JobRouter) Once(c *gin.Context) {
	var req request.ReqJobOnce
	var err error
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[job_once] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[job_once] request parameter error", c)
		return
	}
	//find node
	node := &models.Node{UUID: req.NodeUUID}
	err = node.FindByUUID()
	if err != nil || node.Status == models.NodeConnFail {
		logger.GetLogger().Error(fmt.Sprintf("[job_once] node[%s] conn fail:%v", req.NodeUUID, err))
		resp.FailWithMessage(resp.ERROR, "[job_once] node conn fail ", c)
		return
	}
	job := &models.Job{ID: req.JobId}
	err = job.FindById()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[job_once] job_id[%d] not exist db:%s", req.JobId, err.Error()))
		resp.FailWithMessage(resp.ERROR, "[job_once] job not exist ", c)
		return
	}

	err = service.DefaultJobService.Once(&req)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[job_once] etcd put job_id :%d error:%s", req.JobId, err.Error()))
		resp.FailWithMessage(resp.ERROR, "[job_once] put  error", c)
		return
	}
	resp.OkWithMessage("job once success", c)
}

func (j *JobRouter) Kill(c *gin.Context) {
	var req request.ReqJobKill
	var err error
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[job_once] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[job_once] request parameter error", c)
		return
	}
	resps, err := etcdclient.Get(fmt.Sprintf(etcdclient.KeyEtcdJobProcProfile, req.NodeUUID, req.JobId), clientv3.WithPrefix())
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[job_kill]etcd get error:%s", err.Error()))
		resp.FailWithMessage(resp.ERROR, "[job_kill] etcd get error", c)
		return
	}
	if len(resps.Kvs) == 0 {
		resp.FailWithMessage(resp.ERROR, "[job_kill] don't have such process", c)
		return
	}
	for _, p := range resps.Kvs {
		var proc models.JobProcVal
		if err := json.Unmarshal(p.Value, &proc); err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("job_kill[%s] unmarshal error: %s", string(p.Key), err.Error()))
			continue
		}
		//进程已经被杀死
		if proc.Killed {
			continue
		}
		proc.Killed = true
		b, err := json.Marshal(&proc)
		if err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("job_kill[%s] marshal error: %s", string(p.Key), err.Error()))
			continue
		}
		//修改
		_, err = etcdclient.Put(string(p.Key), string(b))
		if err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("job_kill[%s] etcd put  error: %s", string(p.Key), err.Error()))
			continue
		}
	}
	resp.OkWithMessage("job kill success", c)
}

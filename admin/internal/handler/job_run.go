package handler

import (
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/logger"
)

// JobRunRouter 执行记录路由（阶段 1 新增）。
//
// 接口设计对齐前端控制台：
//
//	/runs 页面       → POST /job/run/search（分页 + 过滤 + 只看失败）
//	执行详情抽屉      → GET  /job/run/find?run_id=xxx（含输出全文）
//	执行步骤时间线    → POST /job/run/steps（按 step_index 排序）
//	仪表盘成功率/跳过率 → GET  /job/run/overview
//	任务详情"上次执行"  → GET  /job/run/find?job_id=123
type JobRunRouter struct {
}

var defaultJobRunRouter = new(JobRunRouter)

func (r *JobRunRouter) Search(c *gin.Context) {
	var req request.ReqJobRunSearch
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[run_search] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[run_search] request parameter error", c)
		return
	}
	req.Check()
	runs, total, err := service.DefaultJobRunService.Search(&req)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[run_search] db error:%s", err.Error()))
		resp.FailWithMessage(resp.ERROR, "[run_search] db error", c)
		return
	}
	resp.OkWithDetailed(resp.PageResult{
		List:     runs,
		Total:    total,
		Page:     req.Page,
		PageSize: req.PageSize,
	}, "search success", c)
}

// Find 支持两种查询：run_id 精确查；job_id 查最近一次。
func (r *JobRunRouter) Find(c *gin.Context) {
	runID := c.Query("run_id")
	jobIDStr := c.Query("job_id")

	if runID == "" && jobIDStr == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, "run_id 与 job_id 至少提供一个", c)
		return
	}

	var (
		run *models.JobRun
		err error
	)
	if runID != "" {
		run, err = service.DefaultJobRunService.FindByRunID(runID)
	} else {
		jobID, convErr := strconv.Atoi(jobIDStr)
		if convErr != nil || jobID <= 0 {
			resp.FailWithMessage(resp.ErrorRequestParameter, "job_id 必须是正整数", c)
			return
		}
		run, err = service.DefaultJobRunService.FindLatestByJob(jobID)
	}
	if err != nil {
		if models.IsRecordNotFound(err) {
			resp.FailWithMessage(resp.ERROR, "执行记录不存在", c)
			return
		}
		logger.GetLogger().Error(fmt.Sprintf("[run_find] db error:%s", err.Error()))
		resp.FailWithMessage(resp.ERROR, "[run_find] db error", c)
		return
	}
	resp.OkWithData(run, c)
}

func (r *JobRunRouter) Steps(c *gin.Context) {
	var req request.ReqJobRunSteps
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[run_steps] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[run_steps] request parameter error", c)
		return
	}
	req.Check()
	steps, total, err := service.DefaultJobRunService.Steps(&req)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[run_steps] db error:%s", err.Error()))
		resp.FailWithMessage(resp.ERROR, "[run_steps] db error", c)
		return
	}
	resp.OkWithDetailed(resp.PageResult{
		List:     steps,
		Total:    total,
		Page:     req.Page,
		PageSize: req.PageSize,
	}, "search success", c)
}

// Overview 执行概览。since 缺省为最近 24 小时。
func (r *JobRunRouter) Overview(c *gin.Context) {
	var since int64
	if raw := c.Query("since"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			resp.FailWithMessage(resp.ErrorRequestParameter, "since 必须是 Unix 秒时间戳", c)
			return
		}
		since = parsed
	}
	overview, err := service.DefaultJobRunService.Overview(since)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[run_overview] db error:%s", err.Error()))
		resp.FailWithMessage(resp.ERROR, "[run_overview] db error", c)
		return
	}
	resp.OkWithData(overview, c)
}

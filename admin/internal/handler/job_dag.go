package handler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/orchestration"
)

// 阶段 4-B：多步骤编排（DAG）控制面接口。
//
// 只有两个接口，而且都是**只读校验/展示**：
//
//	/job/dag/validate  校验一份编排定义（保存前预检，编辑器边写边查）
//	/job/dag/find      读取某个任务的编排摘要（步骤、边、层深、告警）
//
// 刻意不做"编排专用写接口"：编排是任务定义的一部分，写入路径就是 /job/add。
// 多一条写路径就多一处"两边校验不一致"的风险，而这类不一致在生产上表现为
// "校验说没问题，节点却不执行"。

// ReqJobDagValidate 校验请求。
//
// 两种用法：
//   - 直接给 steps_json（编辑器里的草稿，还没保存）；
//   - 给 job_id（校验库里已存在的任务，忽略 steps_json）。
type ReqJobDagValidate struct {
	JobID     int    `json:"job_id"`
	StepsJSON string `json:"steps_json"`
	// JobType / Timeout 只影响步骤的默认类型与超时继承，不传按 shell + 不超时处理。
	JobType models.JobType `json:"job_type"`
	Timeout int64          `json:"timeout"`
}

// ReqJobDagFind 查询请求。
type ReqJobDagFind struct {
	JobID int `json:"job_id"`
}

// jobDagValidate 校验编排定义，并返回可直接渲染的摘要。
func (j *JobRouter) DagValidate(c *gin.Context) {
	var req ReqJobDagValidate
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[dag_validate] request parameter error", c)
		return
	}

	job := &models.Job{Type: req.JobType, Timeout: req.Timeout, StepsJSON: req.StepsJSON}
	if req.JobID > 0 {
		loaded := &models.Job{ID: req.JobID}
		if err := loaded.FindById(); err != nil {
			resp.FailWithMessage(resp.ErrorJobFormat, fmt.Sprintf("任务不存在（job_id=%d）", req.JobID), c)
			return
		}
		job = loaded
	}

	if strings.TrimSpace(job.StepsJSON) == "" {
		resp.Result(resp.ErrorJobPlanNotFound, gin.H{
			"valid": false,
			"error": "该任务没有编排定义（steps_json 为空）：单步任务不需要编排",
		}, "该任务没有编排定义（steps_json 为空）：单步任务不需要编排", c)
		return
	}

	summary, err := orchestration.Summarize(job)
	if err != nil {
		// 摘要在非法图上也会尽量给出（可能为 nil），但结论一定是"不合法"。
		var payload gin.H
		if summary.Steps != nil {
			payload = gin.H{"valid": false, "error": err.Error(), "plan": summary}
		} else {
			payload = gin.H{"valid": false, "error": err.Error()}
		}
		// msg 里放**具体原因**（哪个步骤、哪个环），而不是"invalid orchestration"：
		// 控制台的错误提示直接取 msg，放一句泛泛的英文等于让运维自己去猜。
		resp.Result(resp.ErrorJobPlanInvalid, payload, err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{"valid": true, "plan": summary}, "orchestration is valid", c)
}

// jobDagFind 返回某个任务的编排摘要（没有编排时返回明确业务码，而不是空图）。
func (j *JobRouter) DagFind(c *gin.Context) {
	var req ReqJobDagFind
	if err := c.ShouldBindJSON(&req); err != nil || req.JobID <= 0 {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[dag_find] job_id is required", c)
		return
	}
	job := &models.Job{ID: req.JobID}
	if err := job.FindById(); err != nil {
		resp.FailWithMessage(resp.ErrorJobFormat, fmt.Sprintf("任务不存在（job_id=%d）", req.JobID), c)
		return
	}
	if strings.TrimSpace(job.StepsJSON) == "" {
		resp.Result(resp.ErrorJobPlanNotFound, gin.H{
			"job_id": job.ID,
			"name":   job.Name,
			"error":  "该任务是单步任务（没有 steps_json）",
		}, "该任务是单步任务（没有 steps_json）", c)
		return
	}
	summary, err := orchestration.Summarize(job)
	payload := gin.H{
		"job_id":     job.ID,
		"name":       job.Name,
		"job_type":   int(job.Type),
		"steps_json": job.StepsJSON,
	}
	if summary.Steps != nil {
		payload["plan"] = summary
	}
	if err != nil {
		payload["error"] = err.Error()
		// 非法定义也要能打开（否则用户看不到自己存进去的东西），但必须标明"当前不可执行"。
		payload["executable"] = false
		resp.Result(resp.ErrorJobPlanInvalid, payload, err.Error(), c)
		return
	}
	payload["executable"] = true
	resp.OkWithDetailed(payload, "operation success", c)
}

// validateJobPlanOrReply 在保存任务前校验编排定义。
//
// 返回 true 表示可以继续；false 表示已经写过响应，调用方必须 return。
// 保存入口必须调用它：**宁可拒绝保存，也不要存下一份节点执行不了的定义**——
// 后者会在真正需要它跑的那一刻暴露（通常是故障恢复的时候）。
func validateJobPlanOrReply(job *models.Job, c *gin.Context) bool {
	if err := orchestration.ValidateJobPlan(job); err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("[save_job] orchestration invalid: %s", err.Error()))
		resp.FailWithDetailed(resp.ErrorJobPlanInvalid, map[string]interface{}{
			"error":      err.Error(),
			"steps_json": job.StepsJSON,
		}, "[save_job] 编排定义非法，已拒绝保存: "+err.Error(), c)
		return false
	}
	return true
}

// marshalPlanForLog 生成编排的紧凑日志（只用于排错，不落库）。
func marshalPlanForLog(job *models.Job) string {
	if strings.TrimSpace(job.StepsJSON) == "" {
		return ""
	}
	var buf strings.Builder
	summary, err := orchestration.Summarize(job)
	if err != nil {
		return err.Error()
	}
	b, _ := json.Marshal(summary)
	buf.Write(b)
	return buf.String()
}

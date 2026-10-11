package agentruntime

import (
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agent"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/runpolicy"
)

// ErrRunNotFound 指定的运行记录不存在（供上层映射为业务码 1010）。
var ErrRunNotFound = fmt.Errorf("运行记录不存在")

// LoadRunContext 从数据库装配一次执行的诊断材料。
//
// 设计取舍：**宁可标注"未知"也不猜**。
// 旧实现里没有的字段（例如没记过退出码、没有 job_log）会保持零值，
// 提示词会明确告诉模型"这些是未知"，避免模型把 0 当成"退出码 0=成功"。
func LoadRunContext(runID string) (agent.RunContext, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return agent.RunContext{}, fmt.Errorf("run_id 不能为空")
	}
	if err := ensureDB(); err != nil {
		return agent.RunContext{}, err
	}
	run := &models.JobRun{RunID: runID}
	if err := run.FindByRunID(); err != nil {
		if models.IsRecordNotFound(err) {
			return agent.RunContext{}, fmt.Errorf("%w: %s（可能已过清理周期或从未存在）", ErrRunNotFound, runID)
		}
		return agent.RunContext{}, err
	}

	rc := agent.RunContext{
		JobID:       run.JobID,
		JobName:     run.JobName,
		JobType:     jobTypeName(run.JobType),
		Spec:        run.Spec,
		NodeUUID:    run.NodeUUID,
		RunID:       run.RunID,
		Status:      models.RunStatus(run.Status).String(),
		ExitCode:    run.ExitCode,
		ScheduledAt: unixSeconds(run.ScheduledTime),
		StartedAt:   unixSeconds(run.StartedAt),
		FinishedAt:  unixSeconds(run.FinishedAt),
		DurationMS:  run.DurationMs,
		Attempts:    run.Attempt,
		RunOn:       run.NodeUUID,
		Command:     llm.Redact(run.Command),
		StdoutTail:  llm.Truncate(llm.Redact(run.Output), 2000),
		StderrTail:  llm.Truncate(llm.Redact(run.ErrorMsg), 2000),
	}

	// 步骤摘要：这是"到底哪一步失败了"的第一手材料。
	if steps, err := loadStepSummaries(runID); err == nil {
		rc.StepSummaries = steps
	} else {
		rc.Extra = append(rc.Extra, "警告：步骤明细读取失败："+err.Error())
	}

	// 最近若干次：区分"一直失败"还是"最近才开始失败"。
	if recent, err := loadRecentRunSummaries(run.JobID, 5, runID); err == nil {
		rc.RecentRuns = recent
	} else {
		rc.Extra = append(rc.Extra, "警告：历史运行读取失败："+err.Error())
	}

	rc.Extra = append(rc.Extra,
		fmt.Sprintf("本次重试次数：%d", run.RetryCount),
		"并发/重试策略快照："+orDash(run.PolicySnapshot),
		"trace_id："+orDash(run.TraceID),
		"触发来源："+models.TriggerSource(run.TriggerSource).String()+
			"（由 "+orDash(run.TriggeredBy)+" 触发）",
	)

	// 节点信息（只读、非敏感）：判断"是任务的问题还是节点的问题"。
	if node := findNode(run.NodeUUID); node != nil {
		rc.Extra = append(rc.Extra, fmt.Sprintf("节点：uuid=%s hostname=%s ip=%s status=%d 上线=%s",
			node.UUID, orDash(node.Hostname), orDash(node.IP), node.Status,
			time.Unix(node.UpTime, 0).Format("2006-01-02 15:04:05")))
	} else if run.NodeUUID != "" {
		rc.Extra = append(rc.Extra, "节点信息：查询不到该节点记录（可能节点已下线或被删除）")
	}

	// 任务定义：重试配置/超时/并发策略决定了"失败是不是配置本身导致的"。
	if job := findJob(run.JobID); job != nil {
		rc.Extra = append(rc.Extra,
			fmt.Sprintf("任务定义：超时=%ds 重试=%d 次/间隔 %ds 并发策略=%s 备注=%s",
				job.Timeout, job.RetryTimes, job.RetryInterval,
				runpolicy.ConcurrencyPolicy(job.ConcurrencyPolicy).String(), orDash(job.Note)),
		)
	}

	// job_log 是旧模型里的执行结果，仍可能含有 job_run 没记下的线索。
	if logs := loadJobLogs(run.JobID, 2); len(logs) > 0 {
		rc.Extra = append(rc.Extra, "最近 job_log：")
		for _, l := range logs {
			rc.Extra = append(rc.Extra, fmt.Sprintf("  - %s 成功=%v 重试=%d 输出=%s",
				time.Unix(l.StartTime, 0).Format("01-02 15:04:05"), l.Success, l.RetryTimes,
				llm.Truncate(llm.Redact(l.Output), 200)))
		}
	}
	return rc, nil
}

// loadStepSummaries 读取步骤明细。
func loadStepSummaries(runID string) ([]string, error) {
	steps := make([]*models.JobRunStep, 0, 4)
	err := db().Table(models.CronyJobRunStepTableName).
		Where("run_id = ?", runID).Order("step_index ASC").Find(&steps).Error
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, fmt.Sprintf("步骤%d %s[%s] 状态=%s 退出码=%d 用时=%dms 重试=%d 输出=%s 错误=%s",
			s.StepIndex, orDash(s.Name), orDash(s.Kind),
			models.RunStatus(s.Status).String(), s.ExitCode, s.DurationMs, s.RetryCount,
			llm.Truncate(llm.Redact(s.Output), 400), llm.Truncate(llm.Redact(s.ErrorMsg), 200)))
	}
	return out, nil
}

// loadRecentRunSummaries 读取同一任务最近 n 次执行摘要（可排除某次）。
func loadRecentRunSummaries(jobID, n int, excludeRunID string) ([]agent.RunSummary, error) {
	if jobID <= 0 || n <= 0 {
		return nil, nil
	}
	runs := make([]*models.JobRun, 0, n)
	q := db().Table(models.CronyJobRunTableName).Where("job_id = ?", jobID)
	if strings.TrimSpace(excludeRunID) != "" {
		q = q.Where("run_id <> ?", excludeRunID)
	}
	if err := q.Order("id DESC").Limit(n).Find(&runs).Error; err != nil {
		return nil, err
	}
	out := make([]agent.RunSummary, 0, len(runs))
	for _, r := range runs {
		note := ""
		if strings.TrimSpace(r.ErrorMsg) != "" {
			note = llm.Truncate(llm.Redact(r.ErrorMsg), 200)
		} else if strings.TrimSpace(r.Output) != "" {
			note = llm.Truncate(llm.Redact(r.Output), 120)
		}
		out = append(out, agent.RunSummary{
			RunID:       r.RunID,
			Status:      models.RunStatus(r.Status).String(),
			ScheduledAt: unixSeconds(r.ScheduledTime),
			DurationMS:  r.DurationMs,
			ExitCode:    r.ExitCode,
			Note:        note,
		})
	}
	return out, nil
}

// loadJobLogs 读取最近 n 条旧模型执行日志。
func loadJobLogs(jobID, n int) []*models.JobLog {
	if jobID <= 0 || n <= 0 {
		return nil
	}
	logs := make([]*models.JobLog, 0, n)
	err := db().Table(models.CronyJobLogTableName).
		Where("job_id = ?", jobID).Order("id DESC").Limit(n).Find(&logs).Error
	if err != nil {
		return nil
	}
	return logs
}

func findJob(jobID int) *models.Job {
	if jobID <= 0 {
		return nil
	}
	job := &models.Job{ID: jobID}
	if err := job.FindById(); err != nil {
		return nil
	}
	return job
}

func findNode(uuid string) *models.Node {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return nil
	}
	node := &models.Node{UUID: uuid}
	if err := node.FindByUUID(); err != nil {
		return nil
	}
	return node
}

// jobTypeName 任务类型 → 人话（提示词里直接使用）。
func jobTypeName(t int) string {
	switch models.JobType(t) {
	case models.JobTypeCmd:
		return "shell 命令"
	case models.JobTypeHttp:
		return "HTTP 请求"
	case models.JobTypeAgent:
		return "Agent 型任务（只读巡检）"
	default:
		return fmt.Sprintf("未知(%d)", t)
	}
}

func unixSeconds(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

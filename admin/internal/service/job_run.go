package service

import (
	"time"

	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
)

// JobRunService 执行记录（job_run / job_run_step）查询服务。
//
// 阶段 1 新增：这是"任务模型 2.0"对外的读取面。
// 与 job_log 的分工：
//   - job_log  = 旧接口，一行一次执行，只有 成功/失败/输出/重试次数；
//   - job_run  = 新模型，带 run_id 幂等键、触发来源、状态机、策略快照、步骤明细。
//
// 当前实现直接读表；阶段 2 起会在此之上叠加"执行历史 → Agent 根因分析"。
type JobRunService struct {
}

var DefaultJobRunService = new(JobRunService)

// Search 分页查询执行记录（按发起时间倒序）。
func (s *JobRunService) Search(req *request.ReqJobRunSearch) ([]models.JobRun, int64, error) {
	db := dbclient.GetMysqlDB().Table(models.CronyJobRunTableName)
	if req.RunID != "" {
		db = db.Where("run_id = ?", req.RunID)
	}
	if req.JobID > 0 {
		db = db.Where("job_id = ?", req.JobID)
	}
	if req.JobName != "" {
		db = db.Where("job_name like ?", req.JobName+"%")
	}
	if req.NodeUUID != "" {
		db = db.Where("node_uuid = ?", req.NodeUUID)
	}
	if req.Status != nil {
		db = db.Where("status = ?", *req.Status)
	}
	if req.TriggerSource != nil {
		db = db.Where("trigger_source = ?", *req.TriggerSource)
	}
	if req.StartTime > 0 {
		db = db.Where("scheduled_time >= ?", req.StartTime)
	}
	if req.EndTime > 0 {
		db = db.Where("scheduled_time < ?", req.EndTime)
	}
	if req.FailedOnly {
		db = db.Where("status in ?", []int{
			int(models.RunStatusFailed),
			int(models.RunStatusTimeout),
			int(models.RunStatusCanceled),
		})
	}

	runs := make([]models.JobRun, 0)
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	// 大纲查询：不返回 output 大字段（列表页不需要，避免拖慢接口）
	err := db.Select("id, run_id, job_id, job_name, spec, job_type, node_uuid, hostname, ip, trigger_source, " +
		"triggered_by, status, attempt, scheduled_time, started_at, finished_at, duration_ms, exit_code, " +
		"step_count, success_steps, failed_steps, retry_count, error_msg, trace_id, policy_snapshot, created, updated").
		Order("scheduled_time desc, id desc").
		Limit(req.PageSize).Offset((req.Page - 1) * req.PageSize).
		Find(&runs).Error
	if err != nil {
		return nil, 0, err
	}
	return runs, total, nil
}

// FindByRunID 精确查询一次执行（含输出全文）。
func (s *JobRunService) FindByRunID(runID string) (*models.JobRun, error) {
	run := &models.JobRun{RunID: runID}
	if err := run.FindByRunID(); err != nil {
		return nil, err
	}
	return run, nil
}

// FindLatestByJob 取任务最近一次执行（用于任务详情页"上次执行"）。
func (s *JobRunService) FindLatestByJob(jobID int) (*models.JobRun, error) {
	run := &models.JobRun{}
	err := dbclient.GetMysqlDB().Table(models.CronyJobRunTableName).
		Where("job_id = ?", jobID).
		Order("scheduled_time desc, id desc").
		First(run).Error
	if err != nil {
		return nil, err
	}
	return run, nil
}

// Steps 查询一次执行的步骤明细。
func (s *JobRunService) Steps(req *request.ReqJobRunSteps) ([]models.JobRunStep, int64, error) {
	db := dbclient.GetMysqlDB().Table(models.CronyJobRunStepTableName).Where("run_id = ?", req.RunID)
	steps := make([]models.JobRunStep, 0)
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := db.Order("step_index asc").Limit(req.PageSize).Offset((req.Page - 1) * req.PageSize).Find(&steps).Error
	if err != nil {
		return nil, 0, err
	}
	return steps, total, nil
}

// RunOverview 执行概览：给控制台仪表盘/任务详情用的一组聚合数字。
//
// 全部来自 job_run（新模型），因此包含"被跳过"的执行——
// 这一点很重要：**跳过率**是并发策略是否合理的直接证据。
type RunOverview struct {
	Total       int64            `json:"total"`
	Success     int64            `json:"success"`
	Failed      int64            `json:"failed"`
	Timeout     int64            `json:"timeout"`
	Canceled    int64            `json:"canceled"`
	Skipped     int64            `json:"skipped"`
	Running     int64            `json:"running"`
	Pending     int64            `json:"pending"`
	RetryTotal  int64            `json:"retry_total"`
	SuccessRate float64          `json:"success_rate"`
	SkipRate    float64          `json:"skip_rate"`
	ByStatus    map[string]int64 `json:"by_status"`
	Since       int64            `json:"since"`
}

// RunStatusCount 单个状态的分组计数（DB 聚合结果行）。
type RunStatusCount struct {
	Status int
	Count  int64
	Retry  int64
}

// Overview 统计 since（Unix 秒）之后的执行情况；since<=0 时默认最近 24 小时。
func (s *JobRunService) Overview(since int64) (*RunOverview, error) {
	if since <= 0 {
		since = time.Now().Add(-24 * time.Hour).Unix()
	}
	rows := make([]RunStatusCount, 0)
	err := dbclient.GetMysqlDB().Table(models.CronyJobRunTableName).
		Select("status, COUNT(*) AS count, COALESCE(SUM(retry_count),0) AS retry").
		Where("scheduled_time >= ?", since).
		Group("status").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return SummarizeRuns(rows, since), nil
}

// SummarizeRuns 把分组计数汇总成概览指标（纯函数，便于单元测试）。
//
// 口径说明（容易被算错，因此固化成测试）：
//   - success_rate = 成功 / 真正执行过的次数（total - skipped）：
//     被并发策略跳过不是失败，把它算进分母会让正常的并发保护看起来像故障；
//   - skip_rate    = 跳过 / 全部触发次数：
//     跳过率长期偏高说明调度间隔或并发策略需要调整。
func SummarizeRuns(rows []RunStatusCount, since int64) *RunOverview {
	overview := &RunOverview{ByStatus: make(map[string]int64), Since: since}
	for _, r := range rows {
		status := models.RunStatus(r.Status)
		overview.Total += r.Count
		overview.RetryTotal += r.Retry
		overview.ByStatus[status.String()] = r.Count
		switch status {
		case models.RunStatusSuccess:
			overview.Success += r.Count
		case models.RunStatusFailed:
			overview.Failed += r.Count
		case models.RunStatusTimeout:
			overview.Timeout += r.Count
		case models.RunStatusCanceled:
			overview.Canceled += r.Count
		case models.RunStatusSkipped:
			overview.Skipped += r.Count
		case models.RunStatusRunning:
			overview.Running += r.Count
		case models.RunStatusPending:
			overview.Pending += r.Count
		}
	}
	executed := overview.Total - overview.Skipped
	if executed > 0 {
		overview.SuccessRate = float64(overview.Success) / float64(executed)
	}
	if overview.Total > 0 {
		overview.SkipRate = float64(overview.Skipped) / float64(overview.Total)
	}
	return overview
}

// CleanupRuns 清理过期的执行记录（与旧 job_log 清理策略并行）。
//
// 注意：先删步骤再删执行，避免留下孤儿步骤。
func CleanupRuns(expirationTime int64) error {
	db := dbclient.GetMysqlDB()
	deadline := time.Now().Unix() - expirationTime
	if err := db.Exec("delete from "+models.CronyJobRunStepTableName+
		" where run_id in (select run_id from "+models.CronyJobRunTableName+" where created < ?)", deadline).Error; err != nil {
		return err
	}
	return db.Exec("delete from "+models.CronyJobRunTableName+" where created < ?", deadline).Error
}
